package ledger

import (
	"fmt"
	"math/big"
	"time"

	"ssfd-engine/domain"
)

type legKey struct {
	account domain.AccountID
	side    domain.Side
}

func opposite(s domain.Side) domain.Side {
	if s == domain.Debit {
		return domain.Credit
	}
	return domain.Debit
}

func normalSide(a domain.Account) domain.Side {
	if a.Class == domain.Asset || a.Class == domain.Expense {
		return domain.Debit
	}
	return domain.Credit
}

func normalBalance(a domain.Account, debitPositive *big.Int) *big.Int {
	if normalSide(a) == domain.Credit {
		return new(big.Int).Neg(debitPositive)
	}
	return new(big.Int).Set(debitPositive)
}

func (l *Ledger) checkReserve(t domain.Transaction, id domain.AccountID) error {
	if id == 0 {
		if l.parties[t.Debtor] != domain.Institution {
			return fmt.Errorf("customer authorization requires a funding account")
		}
		return nil
	}
	a := l.accounts[id]
	if a.ID == 0 || a.Owner != t.Debtor || a.Currency != t.Principal.Currency ||
		a.State != domain.Active || (a.Class != domain.Asset && a.Class != domain.Liability) {
		return fmt.Errorf("invalid authorization funding account")
	}
	d := l.accountDetails[id]
	free := normalBalance(a, &d.balance)
	free.Sub(free, &d.reserved)
	free.Sub(free, big.NewInt(t.Principal.Minor))
	if len(d.holds) != 0 || free.Cmp(big.NewInt(d.policy.MinimumNormal)) < 0 {
		return fmt.Errorf("account %d held or has insufficient available funds", id)
	}
	return nil
}

func (l *Ledger) changeTransaction(in ChangeTransaction, at time.Time, seq uint64) (domain.EntryID, error) {
	t, ok := l.transactions[in.ID]
	if !ok || t.State != in.From {
		return 0, fmt.Errorf("unknown transaction or unexpected current state")
	}
	d := l.transactionDetails[t.ID]
	same := in.From == in.To
	if same {
		if (in.From != domain.Cleared && in.From != domain.Disputed) || in.Journal == nil || in.Journal.Kind != domain.Adjustment {
			return 0, fmt.Errorf("only a principal adjustment may leave workflow state unchanged")
		}
	} else if !in.From.CanTransitionTo(in.To) {
		return 0, fmt.Errorf("invalid transaction transition")
	}
	if in.To == domain.Authorized {
		if err := l.checkReserve(t, in.ReserveAccount); err != nil {
			return 0, err
		}
	} else if in.ReserveAccount != 0 {
		return 0, fmt.Errorf("reserve account belongs to authorization")
	}
	booking := in.From == domain.Authorized && in.To == domain.Cleared
	requiresEntry := booking || in.To == domain.Reversed || same
	if requiresEntry != (in.Journal != nil) {
		return 0, fmt.Errorf("journal presence does not match transition")
	}
	var entry domain.JournalEntry
	var effects []accountEffect
	var remaining map[legKey]*big.Int
	if requiresEntry {
		j := in.Journal
		entry = domain.JournalEntry{
			Book: l.book, ID: j.ID, Transaction: t.ID, Kind: j.Kind, Original: j.Original,
			EffectiveAt: j.EffectiveAt, RecordedAt: at, CommitSequence: seq, Postings: j.Postings,
		}
		if _, exists := l.entries[j.ID]; exists || j.EffectiveAt.Before(l.earliest) || j.EffectiveAt.Before(t.CreatedAt) {
			return 0, fmt.Errorf("repeated journal ID or effective instant outside permitted window")
		}
		if err := entry.Validate(l.catalog, l.accounts); err != nil {
			return 0, err
		}

		var err error
		if booking {
			if err = l.checkBooking(t, d.reserve, entry); err != nil {
				return 0, err
			}
			remaining = inverseLegs(entry.Postings)
		} else {
			remaining, err = l.checkUnwind(d, entry, in.To == domain.Reversed)
			if err != nil {
				return 0, err
			}
		}
		release := domain.AccountID(0)
		if booking {
			release = d.reserve
		}
		effects, err = l.checkEffects(entry, j.Servicing && (d.reserve == 0 || !booking), release, t.Principal.Minor)
		if err != nil {
			return 0, err
		}
	}

	// Every fallible check completed. Publish all effects under the write lock.
	if in.To == domain.Authorized {
		d.reserve = in.ReserveAccount
		if d.reserve != 0 {
			a := l.accountDetails[d.reserve]
			a.reserved.Add(&a.reserved, big.NewInt(t.Principal.Minor))
			a.lastActivity = at
		}
	}
	if in.From == domain.Authorized && d.reserve != 0 && (booking || in.To == domain.Cancelled) {
		a := l.accountDetails[d.reserve]
		a.reserved.Sub(&a.reserved, big.NewInt(t.Principal.Minor))
		d.reserve = 0
	}
	if requiresEntry {
		if booking {
			d.booking = entry.ID
		}
		d.remaining = remaining
		l.installEntry(entry, effects)
	}
	if !same {
		t.State = in.To
		l.transactions[t.ID] = t
		l.transactionHistory = append(l.transactionHistory, domain.TransactionEvent{
			Book: l.book, Transaction: t.ID, Sequence: seq, RecordedAt: at, From: in.From, To: in.To, Journal: entry.ID,
		})
	}
	return entry.ID, nil
}

func (l *Ledger) checkBooking(t domain.Transaction, funding domain.AccountID, e domain.JournalEntry) error {
	if e.Kind != domain.Booking {
		return fmt.Errorf("clearing requires a booking")
	}
	var baseFunded, baseDeposited, quoteDeposited big.Int
	for _, p := range e.Postings {
		a := l.accounts[p.Account]
		amount := big.NewInt(p.Amount.Minor)
		base := a.Currency == t.Principal.Currency
		quote := t.Quote != nil && a.Currency == t.Quote.Target.Currency
		switch {
		case !base && !quote:
			return fmt.Errorf("booking includes currency outside intent")
		case funding == 0:
			if p.Side == domain.Debit {
				if base {
					baseDeposited.Add(&baseDeposited, amount)
				} else {
					quoteDeposited.Add(&quoteDeposited, amount)
				}
			}
		case base && p.Account == funding && p.Side != normalSide(a):
			baseFunded.Add(&baseFunded, amount)
		case t.Quote != nil && a.Purpose == domain.FXPosition &&
			((base && p.Side == normalSide(l.accounts[funding])) || (quote && p.Side == domain.Debit)):
			// The base counter-leg offsets the funding account. A customer
			// FX target is a credit to a deposit liability.
		case a.Owner == t.Creditor && a.ID != funding &&
			(a.Class == domain.Asset || a.Class == domain.Liability) && p.Side == normalSide(a):
			if t.Quote == nil && base {
				baseDeposited.Add(&baseDeposited, amount)
			} else if quote && a.Class == domain.Liability {
				quoteDeposited.Add(&quoteDeposited, amount)
			} else {
				return fmt.Errorf("beneficiary posting mismatches intent")
			}
		default:
			return fmt.Errorf("customer booking includes an unexpected leg")
		}
	}
	if funding != 0 && baseFunded.Cmp(big.NewInt(t.Principal.Minor)) != 0 {
		return fmt.Errorf("funding postings do not match reserved principal")
	}
	if (funding == 0 || t.Quote == nil) && baseDeposited.Cmp(big.NewInt(t.Principal.Minor)) != 0 {
		return fmt.Errorf("booking principal does not match intent")
	}
	if t.Quote != nil && quoteDeposited.Cmp(big.NewInt(t.Quote.Target.Minor)) != 0 {
		return fmt.Errorf("booking target does not match locked quote")
	}
	return nil
}

func inverseLegs(postings []domain.Posting) map[legKey]*big.Int {
	remaining := make(map[legKey]*big.Int)
	for _, p := range postings {
		key := legKey{p.Account, opposite(p.Side)}
		if remaining[key] == nil {
			remaining[key] = new(big.Int)
		}
		remaining[key].Add(remaining[key], big.NewInt(p.Amount.Minor))
	}
	return remaining
}

func (l *Ledger) checkUnwind(d *transactionRecord, e domain.JournalEntry, terminal bool) (map[legKey]*big.Int, error) {
	original, ok := l.entries[d.booking]
	if !ok || e.Original != d.booking || e.EffectiveAt.Before(original.EffectiveAt) {
		return nil, fmt.Errorf("unwind must reference the original booking in accounting order")
	}
	if e.Kind == domain.Reversal {
		if !terminal || len(e.Postings) != len(original.Postings) {
			return nil, fmt.Errorf("full reversal requires original legs and terminal state")
		}
		for i, p := range original.Postings {
			p.Side = opposite(p.Side)
			if e.Postings[i] != p {
				return nil, fmt.Errorf("full reversal differs from original at leg %d", i+1)
			}
		}
	} else if e.Kind != domain.Adjustment {
		return nil, fmt.Errorf("unwind requires reversal or adjustment")
	}

	next := make(map[legKey]*big.Int, len(d.remaining))
	for key, amount := range d.remaining {
		next[key] = new(big.Int).Set(amount)
	}
	for _, p := range e.Postings {
		key := legKey{p.Account, p.Side}
		amount := next[key]
		if amount == nil || amount.Sub(amount, big.NewInt(p.Amount.Minor)).Sign() < 0 {
			return nil, fmt.Errorf("unwind exceeds remaining reversible amount")
		}
		if amount.Sign() == 0 {
			delete(next, key)
		}
	}
	if terminal != (len(next) == 0) {
		return nil, fmt.Errorf("terminal reversal must exhaust principal")
	}
	return next, nil
}

type accountEffect struct {
	id    domain.AccountID
	delta big.Int
}

func (l *Ledger) checkEffects(e domain.JournalEntry, servicing bool, release domain.AccountID, principal int64) ([]accountEffect, error) {
	order := make([]accountEffect, 0)
	index := make(map[domain.AccountID]int)
	for _, p := range e.Postings {
		a := l.accounts[p.Account]
		d := l.accountDetails[p.Account]
		outward := p.Side != normalSide(a)
		if a.State == domain.Closed || a.State == domain.Frozen ||
			(outward && (len(d.holds) != 0 || (a.State == domain.Dormant && !servicing))) {
			return nil, fmt.Errorf("account %d blocks posting", a.ID)
		}
		i, found := index[a.ID]
		if !found {
			i = len(order)
			index[a.ID] = i
			order = append(order, accountEffect{id: a.ID})
		}
		amount := big.NewInt(p.Amount.Minor)
		if p.Side == domain.Credit {
			amount.Neg(amount)
		}
		order[i].delta.Add(&order[i].delta, amount)
	}
	for i := range order {
		effect := &order[i]
		d := l.accountDetails[effect.id]
		a := l.accounts[effect.id]
		reserved := new(big.Int).Set(&d.reserved)
		if effect.id == release {
			reserved.Sub(reserved, big.NewInt(principal))
		}
		if !d.policy.EnforceFloor && reserved.Sign() == 0 {
			continue
		}
		projected := new(big.Int).Add(&d.balance, &effect.delta)
		free := normalBalance(a, projected)
		free.Sub(free, reserved)
		if free.Cmp(big.NewInt(d.policy.MinimumNormal)) < 0 {
			return nil, fmt.Errorf("account %d would violate balance floor or other reserves", effect.id)
		}
	}
	return order, nil
}

func (l *Ledger) installEntry(e domain.JournalEntry, effects []accountEffect) {
	l.entries[e.ID] = e
	l.journalOrder = append(l.journalOrder, e.ID)
	for i := range effects {
		effect := &effects[i]
		d := l.accountDetails[effect.id]
		d.balance.Add(&d.balance, &effect.delta)
	}
	for i, p := range e.Postings {
		d := l.accountDetails[p.Account]
		d.edges = append(d.edges, PostingRef{e.ID, uint64(i) + 1, p.Side})
		if !e.RecordedAt.Before(d.lastActivity) && l.accounts[p.Account].State == domain.Active {
			d.lastActivity = e.RecordedAt
		}
	}
}
