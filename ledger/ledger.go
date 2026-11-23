// Package ledger implements the atomic, in-memory, single-book commit boundary.
package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sync"
	"time"

	"ssfd-engine/domain"
)

var (
	ErrConflict = errors.New("idempotency key reused with different content")
	ErrSequence = errors.New("command expected a different committed sequence")
)

// Config is copied at construction. EarliestEffective is the start of the
// open accounting window. Freeze and servicing commands are trusted inputs.
type Config struct {
	Book              domain.BookID
	Currencies        domain.CurrencyCatalog
	Parties           []domain.Counterparty
	Rates             []domain.FXRate
	EarliestEffective time.Time
}

// AccountPolicy controls the account's normal-side balance and inactivity.
type AccountPolicy struct {
	EnforceFloor  bool
	MinimumNormal int64
	DormantAfter  time.Duration
}

type OpenAccount struct {
	ID       domain.AccountID
	Owner    domain.CounterpartyID
	Class    domain.AccountClass
	Purpose  domain.AccountPurpose
	Currency domain.Currency
	Policy   AccountPolicy
}

type ChangeAccount struct {
	ID       domain.AccountID
	From, To domain.AccountState
}

// ChangeHold is a trusted control input independent of lifecycle state.
type ChangeHold struct {
	Account domain.AccountID
	Hold    uint64
	Place   bool
}

type OpenTransaction struct {
	ID               domain.TransactionID
	Kind             domain.IntentKind
	Debtor, Creditor domain.CounterpartyID
	Principal        domain.Money
	Quote            *domain.FXQuote
}

// Journal is proposed financial content. The ledger assigns book, transaction,
// recorded instant and sequence. Servicing bypasses dormancy, subject to holds.
type Journal struct {
	ID, Original domain.EntryID
	Kind         domain.EntryKind
	EffectiveAt  time.Time
	Postings     []domain.Posting
	Servicing    bool
}

// ChangeTransaction moves the workflow or commits an adjustment in place.
// ReserveAccount is supplied only on authorization. Zero denotes trusted
// institutional accounting, which does not place a customer funds reserve.
type ChangeTransaction struct {
	ID             domain.TransactionID
	From, To       domain.TransactionState
	ReserveAccount domain.AccountID
	Journal        *Journal
}

// Command contains exactly one operation. CheckSequence enables optimistic
// concurrency. Accepted retries are checked before sequence and time guards.
type Command struct {
	Key               domain.RequestKey
	RecordedAt        time.Time
	CheckSequence     bool
	ExpectedSequence  uint64
	OpenAccount       *OpenAccount
	ChangeAccount     *ChangeAccount
	ChangeHold        *ChangeHold
	OpenTransaction   *OpenTransaction
	ChangeTransaction *ChangeTransaction
}

type Receipt struct {
	Sequence uint64
	Entry    domain.EntryID
}

type requestRecord struct {
	payload []byte
	receipt Receipt
}

type accountRecord struct {
	policy       AccountPolicy
	balance      big.Int // debit-positive
	reserved     big.Int // positive normal-side funds reserved
	holds        map[uint64]bool
	lastActivity time.Time
	edges        []PostingRef
}

type transactionRecord struct {
	reserve   domain.AccountID
	booking   domain.EntryID
	remaining map[legKey]*big.Int
}

type Ledger struct {
	mu                 sync.RWMutex
	book               domain.BookID
	catalog            domain.CurrencyCatalog
	parties            map[domain.CounterpartyID]domain.PartyKind
	rates              map[domain.RateID]domain.FXRate
	earliest           time.Time
	lastRecorded       time.Time
	sequence           uint64
	requests           map[domain.RequestKey]requestRecord
	accounts           map[domain.AccountID]domain.Account
	accountDetails     map[domain.AccountID]*accountRecord
	transactions       map[domain.TransactionID]domain.Transaction
	transactionDetails map[domain.TransactionID]*transactionRecord
	entries            map[domain.EntryID]domain.JournalEntry
	journalOrder       []domain.EntryID
	accountHistory     []domain.AccountEvent
	transactionHistory []domain.TransactionEvent
	holdHistory        []HoldEvent
}

func New(config Config) (*Ledger, error) {
	if config.Book == 0 || len(config.Currencies) == 0 {
		return nil, fmt.Errorf("book and pinned currency catalog required")
	}
	l := &Ledger{
		book: config.Book, earliest: config.EarliestEffective.UTC(),
		catalog: make(domain.CurrencyCatalog), parties: make(map[domain.CounterpartyID]domain.PartyKind),
		rates: make(map[domain.RateID]domain.FXRate), requests: make(map[domain.RequestKey]requestRecord),
		accounts: make(map[domain.AccountID]domain.Account), accountDetails: make(map[domain.AccountID]*accountRecord),
		transactions:       make(map[domain.TransactionID]domain.Transaction),
		transactionDetails: make(map[domain.TransactionID]*transactionRecord),
		entries:            make(map[domain.EntryID]domain.JournalEntry),
	}
	for code := range config.Currencies {
		spec, err := config.Currencies.Lookup(code)
		if err != nil {
			return nil, err
		}
		l.catalog[code] = spec
	}
	for _, party := range config.Parties {
		if party.ID == 0 || party.Kind < domain.Individual || party.Kind > domain.Institution || l.parties[party.ID] != 0 {
			return nil, fmt.Errorf("invalid or repeated synthetic counterparty %d", party.ID)
		}
		l.parties[party.ID] = party.Kind
	}
	for _, rate := range config.Rates {
		if rate.ID == 0 || l.rates[rate.ID].ID != 0 || rate.Base == rate.Quote ||
			rate.Numerator == 0 || rate.Denominator == 0 ||
			rate.ValidFrom.IsZero() || !rate.ValidFrom.Before(rate.ValidUntil) {
			return nil, fmt.Errorf("invalid or repeated FX rate %d", rate.ID)
		}
		if _, err := l.catalog.Lookup(rate.Base); err != nil {
			return nil, err
		}
		if _, err := l.catalog.Lookup(rate.Quote); err != nil {
			return nil, err
		}
		rate.ValidFrom, rate.ValidUntil = rate.ValidFrom.UTC(), rate.ValidUntil.UTC()
		l.rates[rate.ID] = rate
	}
	return l, nil
}

func (c Command) normalized() Command {
	c.RecordedAt = c.RecordedAt.UTC()
	if !c.CheckSequence {
		c.ExpectedSequence = 0
	}
	if c.OpenTransaction != nil {
		tx := *c.OpenTransaction
		tx.Quote = copyQuote(tx.Quote)
		c.OpenTransaction = &tx
	}
	if c.ChangeTransaction != nil {
		change := *c.ChangeTransaction
		if change.Journal != nil {
			j := *change.Journal
			j.EffectiveAt = j.EffectiveAt.UTC()
			j.Postings = append([]domain.Posting(nil), j.Postings...)
			change.Journal = &j
		}
		c.ChangeTransaction = &change
	}
	return c
}

func copyQuote(q *domain.FXQuote) *domain.FXQuote {
	if q == nil {
		return nil
	}
	out := *q
	return &out
}

// Apply validates and commits one command atomically. Failed commands leave
// state and the accepted-request index untouched.
func (l *Ledger) Apply(c Command) (Receipt, error) {
	if c.Key.Book != l.book || c.Key.Source == 0 || c.Key.Request == 0 || c.RecordedAt.IsZero() {
		return Receipt{}, fmt.Errorf("invalid command identity or recorded time")
	}
	count := 0
	for _, present := range []bool{c.OpenAccount != nil, c.ChangeAccount != nil, c.ChangeHold != nil,
		c.OpenTransaction != nil, c.ChangeTransaction != nil} {
		if present {
			count++
		}
	}
	if count != 1 {
		return Receipt{}, fmt.Errorf("command must contain exactly one operation")
	}
	c = c.normalized()
	payload, err := json.Marshal(c)
	if err != nil {
		return Receipt{}, fmt.Errorf("encode canonical command: %w", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if existing, ok := l.requests[c.Key]; ok {
		if !bytes.Equal(existing.payload, payload) {
			return Receipt{}, ErrConflict
		}
		return existing.receipt, nil
	}
	if c.CheckSequence && c.ExpectedSequence != l.sequence {
		return Receipt{}, ErrSequence
	}
	if c.RecordedAt.Before(l.lastRecorded) || l.sequence == math.MaxUint64 {
		return Receipt{}, fmt.Errorf("recorded time regressed or sequence exhausted")
	}
	receipt := Receipt{Sequence: l.sequence + 1}
	switch {
	case c.OpenAccount != nil:
		err = l.openAccount(*c.OpenAccount, c.RecordedAt, receipt.Sequence)
	case c.ChangeAccount != nil:
		err = l.changeAccount(*c.ChangeAccount, c.RecordedAt, receipt.Sequence)
	case c.ChangeHold != nil:
		err = l.changeHold(*c.ChangeHold, c.RecordedAt, receipt.Sequence)
	case c.OpenTransaction != nil:
		err = l.openTransaction(*c.OpenTransaction, c, receipt.Sequence)
	case c.ChangeTransaction != nil:
		receipt.Entry, err = l.changeTransaction(*c.ChangeTransaction, c.RecordedAt, receipt.Sequence)
	}
	if err != nil {
		return Receipt{}, err
	}
	l.sequence, l.lastRecorded = receipt.Sequence, c.RecordedAt
	l.requests[c.Key] = requestRecord{payload, receipt}
	return receipt, nil
}

func (l *Ledger) openAccount(in OpenAccount, at time.Time, seq uint64) error {
	_, exists := l.accounts[in.ID]
	if in.ID == 0 || exists || (in.Owner != 0 && l.parties[in.Owner] == 0) ||
		in.Class < domain.Asset || in.Class > domain.Expense ||
		in.Purpose < domain.General || in.Purpose > domain.FXPosition ||
		(in.Purpose == domain.FXPosition && in.Owner != 0) ||
		in.Policy.DormantAfter < 0 || (!in.Policy.EnforceFloor && in.Policy.MinimumNormal != 0) {
		return fmt.Errorf("invalid or repeated account %d", in.ID)
	}
	if _, err := l.catalog.Lookup(in.Currency); err != nil {
		return err
	}
	l.accounts[in.ID] = domain.Account{
		Book: l.book, ID: in.ID, Owner: in.Owner, Class: in.Class, Purpose: in.Purpose,
		Currency: in.Currency, OpenedAt: at, State: domain.Active,
	}
	l.accountDetails[in.ID] = &accountRecord{policy: in.Policy, lastActivity: at}
	l.accountHistory = append(l.accountHistory, domain.AccountEvent{
		Book: l.book, Account: in.ID, Sequence: seq, RecordedAt: at, To: domain.Active,
	})
	return nil
}

func (l *Ledger) changeAccount(in ChangeAccount, at time.Time, seq uint64) error {
	a, ok := l.accounts[in.ID]
	if !ok || a.State != in.From || !in.From.CanTransitionTo(in.To) {
		return fmt.Errorf("invalid account transition for %d", in.ID)
	}
	d := l.accountDetails[in.ID]
	inactive := d.policy.DormantAfter > 0 && at.Sub(d.lastActivity) >= d.policy.DormantAfter
	if in.To == domain.Dormant && !inactive {
		return fmt.Errorf("account %d has not met the dormancy threshold", in.ID)
	}
	if in.To == domain.Closed && (d.balance.Sign() != 0 || d.reserved.Sign() != 0 || len(d.holds) != 0) {
		return fmt.Errorf("account %d has booked funds or open obligations", in.ID)
	}
	if in.To == domain.Active {
		d.lastActivity = at
	}
	a.State = in.To
	l.accounts[in.ID] = a
	l.accountHistory = append(l.accountHistory, domain.AccountEvent{
		Book: l.book, Account: in.ID, Sequence: seq, RecordedAt: at, From: in.From, To: in.To,
	})
	return nil
}

type HoldEvent struct {
	Sequence   uint64
	RecordedAt time.Time
	ChangeHold
}

func (l *Ledger) changeHold(in ChangeHold, at time.Time, seq uint64) error {
	a, ok := l.accounts[in.Account]
	if !ok || a.State == domain.Closed || in.Hold == 0 {
		return fmt.Errorf("invalid account or hold identity")
	}
	d := l.accountDetails[in.Account]
	if d.holds[in.Hold] == in.Place {
		return fmt.Errorf("hold is already in requested state")
	}
	if in.Place {
		if d.holds == nil {
			d.holds = make(map[uint64]bool)
		}
		d.holds[in.Hold] = true
	} else {
		delete(d.holds, in.Hold)
	}
	l.holdHistory = append(l.holdHistory, HoldEvent{seq, at, in})
	return nil
}

func (l *Ledger) openTransaction(in OpenTransaction, c Command, seq uint64) error {
	_, exists := l.transactions[in.ID]
	if exists || l.parties[in.Debtor] == 0 || l.parties[in.Creditor] == 0 {
		return fmt.Errorf("repeated transaction or unknown party")
	}
	t := domain.Transaction{Book: l.book, ID: in.ID, Request: c.Key, Kind: in.Kind,
		State: domain.Pending, Debtor: in.Debtor, Creditor: in.Creditor, Principal: in.Principal,
		Quote: in.Quote, CreatedAt: c.RecordedAt}
	if err := t.Validate(l.catalog, l.rates); err != nil {
		return err
	}
	l.transactions[in.ID] = t
	l.transactionDetails[in.ID] = new(transactionRecord)
	l.transactionHistory = append(l.transactionHistory, domain.TransactionEvent{
		Book: l.book, Transaction: t.ID, Sequence: seq, RecordedAt: c.RecordedAt, To: domain.Pending,
	})
	return nil
}
