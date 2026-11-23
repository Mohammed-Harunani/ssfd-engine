package chaos

import (
	"fmt"
	"time"

	"ssfd-engine/domain"
	"ssfd-engine/ledger"
	"ssfd-engine/simclock"
)

// Day runs the morning fault scenarios for a local civil date. Call on each
// successive simulation day before the normal 09:00 jobs. canDelay permits
// newly queued settlements only when another simulated date follows.
func (i *Injector) Day(d simclock.Date, canDelay bool) error {
	midnight, err := dateTime(d, 0, 0, 0, 0).Resolve(i.zone, simclock.RejectAmbiguous)
	if err != nil {
		return err
	}
	if i.clock.Now().After(midnight) {
		return fmt.Errorf("chaos date regressed or scheduled after midnight")
	}
	if i.selectFault(Midnight) {
		before := midnight.Add(-time.Nanosecond)
		if !before.Before(i.clock.Now()) {
			if err := i.clock.AdvanceTo(before); err != nil {
				return err
			}
			if err := i.single(Midnight, gbp); err != nil {
				return err
			}
		}
		if err := i.clock.AdvanceTo(midnight); err != nil {
			return err
		}
		if err := i.instantPair(Midnight, gbp); err != nil {
			return err
		}
	}
	if d.Month == time.February && d.Day == 29 && i.selectFault(LeapDay) {
		if err := i.clock.AdvanceTo(midnight); err != nil {
			return err
		}
		if err := i.leap(d); err != nil {
			return err
		}
	}
	if err := i.dst(d); err != nil {
		return err
	}
	if err := i.advance(d, 8, 0, 0, 0); err != nil {
		return err
	}
	if i.delayed != nil {
		if _, err := i.accept(Ordering, i.booking(*i.delayed)); err != nil {
			return err
		}
		i.stats[Ordering].DelayedSettlements++
		i.delayed = nil
	}
	if canDelay && i.selectFault(Ordering) {
		if err := i.queue(); err != nil {
			return err
		}
	}
	for _, scenario := range []struct {
		fault Fault
		run   func() error
	}{
		{Duplicate, i.duplicate}, {Overdraft, i.overdraft}, {Chargeback, i.chargeback}, {Freeze, i.freeze},
	} {
		if i.selectFault(scenario.fault) {
			if err := scenario.run(); err != nil {
				return fmt.Errorf("%s on %v: %w", scenario.fault, d, err)
			}
		}
	}
	return nil
}

func (i *Injector) single(f Fault, l lane) error {
	t, err := i.open(l, i.amount(f))
	if err != nil {
		return err
	}
	_, err = i.settle(f, t)
	return err
}

func (i *Injector) leap(d simclock.Date) error {
	start := simclock.Date{Year: d.Year, Month: time.February, Day: 28}
	n, err := i.interest(start, d)
	if err != nil {
		return err
	}
	if n <= 0 {
		return fmt.Errorf("leap-day interest must be positive")
	}
	m := domain.Money{Currency: "GBP", Minor: n}
	t, err := i.openIntent(Institution, Institution, 0, m, []domain.Posting{
		leg(interestAsset, domain.Debit, m), leg(interestIncome, domain.Credit, m),
	})
	if err != nil {
		return err
	}
	if _, err := i.settle(LeapDay, t); err != nil {
		return err
	}
	return i.single(LeapDay, gbp)
}

func (i *Injector) dst(d simclock.Date) error {
	civil := dateTime(d, 1, 30, 0, 0)
	a, err := civil.Resolve(i.zone, simclock.Earlier)
	if err != nil {
		return nil
	} // Spring gap is ineligible.
	b, err := civil.Resolve(i.zone, simclock.Later)
	if err != nil {
		return err
	}
	if a.Equal(b) || !i.selectFault(DSTOverlap) {
		return nil
	}
	for _, t := range []time.Time{a, b} {
		if err := i.clock.AdvanceTo(t); err != nil {
			return err
		}
		if err := i.single(DSTOverlap, gbp); err != nil {
			return err
		}
	}
	return nil
}

func (i *Injector) queue() error {
	t, err := i.open(gbp, i.amount(Ordering))
	if err != nil {
		return err
	}
	if err := i.reject(Ordering, i.stamp(i.booking(t)), nil, gbp.payer, gbp.payee); err != nil {
		return err
	}
	if _, err := i.apply(i.auth(t)); err != nil {
		return err
	}
	i.delayed = &t
	return nil
}

func (i *Injector) duplicate() error {
	t, err := i.open(usd, i.amount(Duplicate))
	if err != nil {
		return err
	}
	if _, err := i.apply(i.auth(t)); err != nil {
		return err
	}
	c := i.stamp(i.booking(t))
	r, err := i.book.Apply(c)
	if err != nil {
		return fmt.Errorf("original booking failed: %w", err)
	}
	if r.Entry == 0 {
		return fmt.Errorf("original booking returned no journal")
	}
	i.stats[Duplicate].Journals++
	before := i.observe([]domain.AccountID{usd.payer, usd.payee})
	retry, err := i.book.Apply(c)
	if err != nil || retry != r {
		return fmt.Errorf("identical retry changed receipt: %v %v", retry, err)
	}
	after := i.observe([]domain.AccountID{usd.payer, usd.payee})
	for idx := range before {
		if before[idx] != after[idx] {
			return fmt.Errorf("idempotent retry changed USD balances")
		}
	}
	i.stats[Duplicate].IdenticalRetries++
	changed := *c.ChangeTransaction
	j := *changed.Journal
	j.Postings = append([]domain.Posting(nil), j.Postings...)
	for k := range j.Postings {
		j.Postings[k].Amount.Minor++
	}
	changed.Journal = &j
	conflict := c
	conflict.ChangeTransaction = &changed
	if err := i.reject(Duplicate, conflict, ledger.ErrConflict, usd.payer, usd.payee); err != nil {
		return err
	}
	return i.reject(Duplicate, i.stamp(i.booking(t)), nil, usd.payer, usd.payee)
}

func (i *Injector) overdraft() error {
	l := lane{currency: "GBP", payer: overdraftAccount, payee: gbp.payee}
	t, err := i.open(l, 500)
	if err != nil {
		return err
	}
	r, err := i.settle(Overdraft, t)
	if err != nil {
		return err
	}
	view, _ := i.book.Account(l.payer)
	if view.AvailableNormal != "-500" {
		return fmt.Errorf("overdraft missed exact floor")
	}
	excess, err := i.open(l, 1)
	if err != nil {
		return err
	}
	if err := i.reject(Overdraft, i.stamp(i.auth(excess)), nil, l.payer, l.payee); err != nil {
		return err
	}
	if _, err := i.apply(move(excess, domain.Pending, domain.Declined, 0, nil)); err != nil {
		return err
	}
	return i.reverse(Overdraft, t, r.Entry)
}

func (i *Injector) chargeback() error {
	t, err := i.open(gbp, i.amount(Chargeback))
	if err != nil {
		return err
	}
	r, err := i.settle(Chargeback, t)
	if err != nil {
		return err
	}
	partial := t.amount
	partial.Minor /= 2
	adjust := func(from, to domain.TransactionState, m domain.Money) ledger.Command {
		return i.draft(t, from, to, domain.Adjustment, r.Entry, []domain.Posting{
			leg(gbp.payer, domain.Credit, m), leg(gbp.payee, domain.Debit, m),
		})
	}
	if _, err := i.accept(Chargeback, adjust(domain.Cleared, domain.Cleared, partial)); err != nil {
		return err
	}
	if _, err := i.apply(move(t, domain.Cleared, domain.Disputed, 0, nil)); err != nil {
		return err
	}
	remainder := t.amount
	remainder.Minor -= partial.Minor
	over := remainder
	over.Minor++
	if err := i.reject(Chargeback, i.stamp(adjust(domain.Disputed, domain.Reversed, over)), nil, gbp.payer, gbp.payee); err != nil {
		return err
	}
	_, err = i.accept(Chargeback, adjust(domain.Disputed, domain.Reversed, remainder))
	return err
}

func (i *Injector) freeze() error {
	t, err := i.open(gbp, i.amount(Freeze))
	if err != nil {
		return err
	}
	if _, err := i.apply(i.auth(t)); err != nil {
		return err
	}
	for _, change := range []ledger.Command{
		{ChangeHold: &ledger.ChangeHold{Account: gbp.payer, Hold: 1, Place: true}},
		{ChangeAccount: &ledger.ChangeAccount{ID: gbp.payer, From: domain.Active, To: domain.Frozen}},
	} {
		if _, err := i.apply(change); err != nil {
			return err
		}
	}
	if err := i.reject(Freeze, i.stamp(i.booking(t)), nil, gbp.payer, gbp.payee); err != nil {
		return err
	}
	for _, change := range []ledger.Command{
		{ChangeAccount: &ledger.ChangeAccount{ID: gbp.payer, From: domain.Frozen, To: domain.Active}},
		{ChangeHold: &ledger.ChangeHold{Account: gbp.payer, Hold: 1}},
	} {
		if _, err := i.apply(change); err != nil {
			return err
		}
	}
	r, err := i.accept(Freeze, i.booking(t))
	if err != nil {
		return err
	}
	return i.reverse(Freeze, t, r.Entry)
}

func (i *Injector) reverse(f Fault, t intent, original domain.EntryID) error {
	inverse := make([]domain.Posting, len(t.postings))
	for k, p := range t.postings {
		switch p.Side {
		case domain.Debit:
			p.Side = domain.Credit
		case domain.Credit:
			p.Side = domain.Debit
		default:
			return fmt.Errorf("unrecognized posting side")
		}
		inverse[k] = p
	}
	_, err := i.accept(f, i.draft(t, domain.Cleared, domain.Reversed, domain.Reversal, original, inverse))
	return err
}
