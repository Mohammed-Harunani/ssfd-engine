package chaos

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	"ssfd-engine/domain"
	"ssfd-engine/ledger"
	"ssfd-engine/prng"
	"ssfd-engine/simclock"
)

const (
	// This namespace is disjoint from the behavioral fixture's sequential IDs.
	base                              = 1 << 40
	Institution domain.CounterpartyID = base + 1
	Payer       domain.CounterpartyID = base + 2
	Payee       domain.CounterpartyID = base + 3
)

type lane struct {
	currency           domain.Currency
	cash, payer, payee domain.AccountID
}

var (
	gbp = lane{"GBP", base + 1, base + 2, base + 3}
	usd = lane{"USD", base + 4, base + 5, base + 6}
)

const (
	overdraftAccount domain.AccountID = base + 7
	interestAsset    domain.AccountID = base + 8
	interestIncome   domain.AccountID = base + 9
)

func Counterparties() []domain.Counterparty {
	return []domain.Counterparty{{ID: Institution, Kind: domain.Institution}, {ID: Payer, Kind: domain.Individual}, {ID: Payee, Kind: domain.Business}}
}

// AccruedInterest returns the exact synthetic GBP interest for [start,end].
type AccruedInterest func(start, end simclock.Date) (int64, error)

type Injector struct {
	bookID   domain.BookID
	book     *ledger.Ledger
	clock    *simclock.Clock
	zone     *time.Location
	interest AccruedInterest
	weights  map[Fault]uint16
	streams  map[Fault]*prng.RNG
	stats    map[Fault]*FaultStats
	request  domain.RequestID
	tx       domain.TransactionID
	entry    domain.EntryID
	delayed  *intent
}

// New initializes dedicated actors' accounts and opening journals in the same
// book the caller uses for the behavioral simulation. The book's catalog must
// support GBP and USD and include Counterparties. Use a single scenario writer.
func New(cfg Config, bookID domain.BookID, book *ledger.Ledger, clock *simclock.Clock, interest AccruedInterest) (*Injector, error) {
	if bookID == 0 || book == nil || clock == nil || interest == nil {
		return nil, fmt.Errorf("book, clock and interest calculation required")
	}
	z, err := simclock.London.Location()
	if err != nil {
		return nil, err
	}
	i := &Injector{bookID: bookID, book: book, clock: clock, zone: z, interest: interest,
		weights: make(map[Fault]uint16), streams: make(map[Fault]*prng.RNG), stats: make(map[Fault]*FaultStats),
		tx: base, entry: base}
	root := prng.New(cfg.Seed)
	for _, f := range order {
		w := cfg.Weights[f]
		if w > 10000 {
			return nil, fmt.Errorf("invalid probability for %s: %d", f, w)
		}
		i.weights[f] = w
		i.streams[f] = prng.New(root.Uint64())
		i.stats[f] = &FaultStats{Fault: f, WeightBPS: w}
	}
	for f := range cfg.Weights {
		if i.stats[f] == nil {
			return nil, fmt.Errorf("unknown fault %q", f)
		}
	}
	if err := i.initialize(); err != nil {
		return nil, err
	}
	return i, nil
}

func (i *Injector) Stats() []FaultStats {
	out := make([]FaultStats, 0, len(order))
	for _, f := range order {
		out = append(out, *i.stats[f])
	}
	return out
}

func (i *Injector) selectFault(f Fault) bool {
	s := i.stats[f]
	s.Eligible++
	n, _ := i.streams[f].Below(10000)
	if n >= uint64(i.weights[f]) {
		return false
	}
	s.Injected++
	return true
}

func (i *Injector) initialize() error {
	for _, l := range []lane{gbp, usd} {
		if err := i.openAccount(l.cash, 0, domain.Asset, l.currency, 0, false); err != nil {
			return err
		}
		for _, a := range []struct {
			id    domain.AccountID
			owner domain.CounterpartyID
		}{{l.payer, Payer}, {l.payee, Payee}} {
			if err := i.openAccount(a.id, a.owner, domain.Liability, l.currency, 0, true); err != nil {
				return err
			}
		}
		const opening int64 = 10_000_000
		if err := i.internal(l.currency, l.cash, l.payer, opening, Payer); err != nil {
			return err
		}
	}
	if err := i.openAccount(overdraftAccount, Payer, domain.Liability, "GBP", -500, true); err != nil {
		return err
	}
	if err := i.openAccount(interestAsset, 0, domain.Asset, "GBP", 0, false); err != nil {
		return err
	}
	return i.openAccount(interestIncome, 0, domain.Revenue, "GBP", 0, false)
}

func (i *Injector) openAccount(id domain.AccountID, owner domain.CounterpartyID, class domain.AccountClass, c domain.Currency, floor int64, enforce bool) error {
	_, err := i.apply(ledger.Command{OpenAccount: &ledger.OpenAccount{ID: id, Owner: owner, Class: class,
		Currency: c, Purpose: domain.General, Policy: ledger.AccountPolicy{EnforceFloor: enforce, MinimumNormal: floor}}})
	return err
}

func (i *Injector) stamp(c ledger.Command) ledger.Command {
	i.request++
	c.Key = domain.RequestKey{Book: i.bookID, Source: 2, Request: i.request}
	c.RecordedAt = i.clock.Now()
	return c
}

func (i *Injector) apply(c ledger.Command) (ledger.Receipt, error) { return i.book.Apply(i.stamp(c)) }

type intent struct {
	id       domain.TransactionID
	funding  domain.AccountID
	amount   domain.Money
	postings []domain.Posting
}

func (i *Injector) open(l lane, minor int64) (intent, error) {
	m := domain.Money{Currency: l.currency, Minor: minor}
	return i.openIntent(Payer, Payee, l.payer, m, []domain.Posting{leg(l.payer, domain.Debit, m), leg(l.payee, domain.Credit, m)})
}

func leg(a domain.AccountID, side domain.Side, m domain.Money) domain.Posting {
	return domain.Posting{Account: a, Side: side, Amount: m}
}

func (i *Injector) openIntent(debtor, creditor domain.CounterpartyID, funding domain.AccountID, m domain.Money, p []domain.Posting) (intent, error) {
	i.tx++
	_, err := i.apply(ledger.Command{OpenTransaction: &ledger.OpenTransaction{ID: i.tx, Kind: domain.SingleCurrency,
		Debtor: debtor, Creditor: creditor, Principal: m}})
	return intent{i.tx, funding, m, p}, err
}

func (i *Injector) auth(t intent) ledger.Command {
	return move(t, domain.Pending, domain.Authorized, t.funding, nil)
}

func move(t intent, from, to domain.TransactionState, reserve domain.AccountID, j *ledger.Journal) ledger.Command {
	return ledger.Command{ChangeTransaction: &ledger.ChangeTransaction{ID: t.id, From: from, To: to, ReserveAccount: reserve, Journal: j}}
}

func (i *Injector) draft(t intent, from, to domain.TransactionState, k domain.EntryKind, original domain.EntryID, p []domain.Posting) ledger.Command {
	i.entry++
	return move(t, from, to, 0, &ledger.Journal{ID: i.entry, Original: original, Kind: k, EffectiveAt: i.clock.Now(), Postings: p})
}

func (i *Injector) booking(t intent) ledger.Command {
	return i.draft(t, domain.Authorized, domain.Cleared, domain.Booking, 0, t.postings)
}

func (i *Injector) accept(f Fault, c ledger.Command) (ledger.Receipt, error) {
	r, err := i.apply(c)
	if err == nil && r.Entry != 0 && i.stats[f] != nil {
		i.stats[f].Journals++
	}
	return r, err
}

func (i *Injector) settle(f Fault, t intent) (ledger.Receipt, error) {
	if _, err := i.apply(i.auth(t)); err != nil {
		return ledger.Receipt{}, err
	}
	return i.accept(f, i.booking(t))
}

func (i *Injector) internal(c domain.Currency, debit, credit domain.AccountID, minor int64, recipient domain.CounterpartyID) error {
	m := domain.Money{Currency: c, Minor: minor}
	t, err := i.openIntent(Institution, recipient, 0, m, []domain.Posting{leg(debit, domain.Debit, m), leg(credit, domain.Credit, m)})
	if err != nil {
		return err
	}
	_, err = i.settle("", t)
	return err
}

func (i *Injector) reject(f Fault, c ledger.Command, expected error, accounts ...domain.AccountID) error {
	before := i.observe(accounts)
	r, err := i.book.Apply(c)
	if err == nil || (expected != nil && !errors.Is(err, expected)) || r != (ledger.Receipt{}) {
		return fmt.Errorf("%s: expected rejection, got receipt %v and %v", f, r, err)
	}
	if !reflect.DeepEqual(before, i.observe(accounts)) {
		return fmt.Errorf("%s rejection changed balances", f)
	}
	i.stats[f].Rejected++
	return nil
}

func (i *Injector) observe(ids []domain.AccountID) []ledger.AccountView {
	out := make([]ledger.AccountView, 0, len(ids))
	for _, id := range ids {
		a, _ := i.book.Account(id)
		out = append(out, a)
	}
	return out
}

// AssertDrained verifies that the final date flushed earlier delayed clearings.
func (i *Injector) AssertDrained() error {
	if i.delayed != nil {
		return fmt.Errorf("unsettled delayed clearing at simulation end")
	}
	return nil
}

func dateTime(d simclock.Date, h, m, s, ns int) simclock.Civil {
	return simclock.Civil{Year: d.Year, Month: d.Month, Day: d.Day, Hour: h, Minute: m, Second: s, Nanosecond: ns}
}

func (i *Injector) advance(d simclock.Date, h, m, s, ns int) error {
	t, err := dateTime(d, h, m, s, ns).Resolve(i.zone, simclock.RejectAmbiguous)
	if err != nil {
		return err
	}
	return i.clock.AdvanceTo(t)
}

func (i *Injector) amount(f Fault) int64 {
	n, _ := i.streams[f].Below(400)
	return 100 + int64(n)
}

func (i *Injector) instantPair(f Fault, l lane) error {
	for range 2 {
		t, err := i.open(l, i.amount(f))
		if err != nil {
			return err
		}
		if _, err := i.settle(f, t); err != nil {
			return err
		}
	}
	return nil
}
