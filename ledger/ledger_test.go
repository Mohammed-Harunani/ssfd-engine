package ledger

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"ssfd-engine/domain"
)

var baseTime = time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC)

func command(id uint64) Command {
	return Command{Key: domain.RequestKey{Book: 1, Source: 1, Request: domain.RequestID(id)}, RecordedAt: baseTime}
}

func apply(t *testing.T, l *Ledger, c Command) Receipt {
	t.Helper()
	r, err := l.Apply(c)
	if err != nil {
		t.Fatalf("request %d: %v", c.Key.Request, err)
	}
	return r
}

func mustReject(t *testing.T, l *Ledger, c Command) {
	t.Helper()
	before := l.Snapshot()
	if _, err := l.Apply(c); err == nil {
		t.Fatalf("request %d unexpectedly succeeded", c.Key.Request)
	}
	if !reflect.DeepEqual(before, l.Snapshot()) {
		t.Fatalf("rejected request %d changed snapshot", c.Key.Request)
	}
}

func fixture(t *testing.T) *Ledger {
	t.Helper()
	l, err := New(Config{
		Book:       1,
		Currencies: domain.CurrencyCatalog{"GBP": {Code: "GBP", MinorDigits: 2}, "USD": {Code: "USD", MinorDigits: 2}},
		Parties:    []domain.Counterparty{{ID: 1, Kind: domain.Institution}, {ID: 2, Kind: domain.Individual}, {ID: 3, Kind: domain.Individual}},
		Rates:      []domain.FXRate{{ID: 1, Base: "GBP", Quote: "USD", Numerator: 5, Denominator: 4, ValidFrom: baseTime, ValidUntil: baseTime.Add(time.Hour)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := AccountPolicy{EnforceFloor: true, DormantAfter: time.Hour}
	for i, account := range []OpenAccount{
		{1, 0, domain.Asset, domain.General, "GBP", AccountPolicy{}},
		{2, 2, domain.Liability, domain.General, "GBP", policy},
		{3, 3, domain.Liability, domain.General, "GBP", policy},
		{4, 0, domain.Asset, domain.FXPosition, "GBP", AccountPolicy{}},
		{5, 0, domain.Asset, domain.FXPosition, "USD", AccountPolicy{}},
		{6, 3, domain.Liability, domain.General, "USD", policy},
	} {
		c := command(uint64(i) + 1)
		c.OpenAccount = &account
		apply(t, l, c)
	}
	return l
}

func open(id uint64, tx domain.TransactionID, a, b domain.CounterpartyID, minor int64) Command {
	c := command(id)
	c.OpenTransaction = &OpenTransaction{ID: tx, Kind: domain.SingleCurrency, Debtor: a, Creditor: b, Principal: domain.Money{Currency: "GBP", Minor: minor}}
	return c
}

func move(id uint64, tx domain.TransactionID, from, to domain.TransactionState, reserve domain.AccountID, j *Journal) Command {
	c := command(id)
	c.ChangeTransaction = &ChangeTransaction{ID: tx, From: from, To: to, ReserveAccount: reserve, Journal: j}
	return c
}

func posting(account domain.AccountID, side domain.Side, c domain.Currency, minor int64) domain.Posting {
	return domain.Posting{Account: account, Side: side, Amount: domain.Money{Currency: c, Minor: minor}}
}

func journal(id, orig domain.EntryID, kind domain.EntryKind, postings ...domain.Posting) *Journal {
	return &Journal{ID: id, Original: orig, Kind: kind, EffectiveAt: baseTime, Postings: postings}
}

func fund(t *testing.T, l *Ledger, request uint64, tx domain.TransactionID, entry domain.EntryID, amount int64) {
	t.Helper()
	apply(t, l, open(request, tx, 1, 2, amount))
	apply(t, l, move(request+1, tx, domain.Pending, domain.Authorized, 0, nil))
	apply(t, l, move(request+2, tx, domain.Authorized, domain.Cleared, 0, journal(entry, 0, domain.Booking,
		posting(1, domain.Debit, "GBP", amount), posting(2, domain.Credit, "GBP", amount))))
}

func TestClearingAndPartialUnwind(t *testing.T) {
	l := fixture(t)
	fund(t, l, 10, 1, 1, 100)
	apply(t, l, open(20, 2, 2, 3, 60))
	booking := journal(2, 0, domain.Booking, posting(2, domain.Debit, "GBP", 60), posting(3, domain.Credit, "GBP", 60))
	clear := move(23, 2, domain.Authorized, domain.Cleared, 0, booking)
	mustReject(t, l, clear)
	apply(t, l, move(21, 2, domain.Pending, domain.Authorized, 2, nil))
	a, _ := l.Account(2)
	if a.DebitPositive != "-100" || a.Reserved != "60" || a.AvailableNormal != "40" {
		t.Fatalf("authorization altered ledger or reserve incorrectly: %+v", a)
	}
	apply(t, l, open(24, 3, 2, 3, 41))
	mustReject(t, l, move(25, 3, domain.Pending, domain.Authorized, 2, nil))
	bad := journal(2, 0, domain.Booking, posting(2, domain.Debit, "GBP", 60), posting(3, domain.Credit, "GBP", 59))
	mustReject(t, l, move(23, 2, domain.Authorized, domain.Cleared, 0, bad))
	h := command(26)
	h.ChangeHold = &ChangeHold{Account: 2, Hold: 1, Place: true}
	apply(t, l, h)
	mustReject(t, l, clear)
	h = command(27)
	h.ChangeHold = &ChangeHold{Account: 2, Hold: 1}
	apply(t, l, h)
	receipt := apply(t, l, clear)
	clear.RecordedAt = clear.RecordedAt.In(time.FixedZone("equivalent UTC offset", -5*60*60))
	if retry := apply(t, l, clear); retry != receipt {
		t.Fatalf("retry changed receipt")
	}
	booking.Postings[0].Amount.Minor++
	if _, err := l.Apply(clear); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected collision, got %v", err)
	}
	s := l.Snapshot()
	s.Journals[1].Postings[0].Amount.Minor = 1
	if got := l.Snapshot().Journals[1].Postings[0].Amount.Minor; got != 60 {
		t.Fatalf("external mutation changed ledger: %d", got)
	}
	apply(t, l, move(28, 2, domain.Cleared, domain.Disputed, 0, nil))
	partial := journal(3, 2, domain.Adjustment, posting(2, domain.Credit, "GBP", 20), posting(3, domain.Debit, "GBP", 20))
	apply(t, l, move(29, 2, domain.Disputed, domain.Disputed, 0, partial))
	over := journal(4, 2, domain.Adjustment, posting(2, domain.Credit, "GBP", 41), posting(3, domain.Debit, "GBP", 41))
	mustReject(t, l, move(30, 2, domain.Disputed, domain.Reversed, 0, over))
	full := journal(4, 2, domain.Adjustment, posting(2, domain.Credit, "GBP", 40), posting(3, domain.Debit, "GBP", 40))
	apply(t, l, move(30, 2, domain.Disputed, domain.Reversed, 0, full))
	want := []TrialRow{{"GBP", "100", "100", "0"}, {"USD", "0", "0", "0"}}
	rows, err := l.Audit()
	if err != nil || !reflect.DeepEqual(rows, want) {
		t.Fatalf("audit %v %v", rows, err)
	}
	if got := l.Edges(2); len(got) != 4 || got[3] != (PostingRef{4, 1, domain.Credit}) {
		t.Fatalf("unexpected adjacency %v", got)
	}
}

func TestFXBookingAndOriginalAmountReversal(t *testing.T) {
	l := fixture(t)
	fund(t, l, 10, 1, 1, 200)
	c := open(20, 2, 2, 3, 100)
	c.OpenTransaction.Kind = domain.Exchange
	c.OpenTransaction.Quote = &domain.FXQuote{Rate: 1, Target: domain.Money{Currency: "USD", Minor: 125}}
	apply(t, l, c)
	c.OpenTransaction.Quote.Target.Minor = 1
	apply(t, l, move(21, 2, domain.Pending, domain.Authorized, 2, nil))
	p := []domain.Posting{posting(2, domain.Debit, "GBP", 100), posting(4, domain.Credit, "GBP", 100),
		posting(5, domain.Debit, "USD", 125), posting(6, domain.Credit, "USD", 125)}
	p[2].Amount.Minor, p[3].Amount.Minor = 124, 124
	mustReject(t, l, move(22, 2, domain.Authorized, domain.Cleared, 0, journal(2, 0, domain.Booking, p...)))
	p[2].Amount.Minor, p[3].Amount.Minor = 125, 125
	c = move(22, 2, domain.Authorized, domain.Cleared, 0, journal(2, 0, domain.Booking, p...))
	c.RecordedAt = baseTime.Add(2 * time.Hour)
	apply(t, l, c)
	want := []TrialRow{{"GBP", "200", "200", "0"}, {"USD", "125", "125", "0"}}
	rows, err := l.Audit()
	if err != nil || !reflect.DeepEqual(rows, want) {
		t.Fatalf("FX trial balance %v %v", rows, err)
	}
	for i := range p {
		p[i].Side = opposite(p[i].Side)
	}
	c = move(23, 2, domain.Cleared, domain.Reversed, 0, journal(3, 2, domain.Reversal, p...))
	c.RecordedAt = baseTime.Add(24 * time.Hour)
	apply(t, l, c)
	want[1] = TrialRow{"USD", "0", "0", "0"}
	rows, err = l.Audit()
	if err != nil || !reflect.DeepEqual(rows, want) {
		t.Fatalf("reversed FX trial balance %v %v", rows, err)
	}
}

func TestConcurrentRetriesAndExactLargeBalances(t *testing.T) {
	l := fixture(t)
	fund(t, l, 10, 1, 1, math.MaxInt64)
	fund(t, l, 20, 2, 2, math.MaxInt64)
	c := open(30, 3, 2, 3, 1)
	c.CheckSequence = true
	c.ExpectedSequence = l.Snapshot().Sequence
	var wg sync.WaitGroup
	results := make(chan error, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := l.Apply(c)
			if err == nil && r.Sequence != c.ExpectedSequence+1 {
				err = fmt.Errorf("unexpected sequence %d", r.Sequence)
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	stale := open(31, 4, 2, 3, 1)
	stale.CheckSequence, stale.ExpectedSequence = true, c.ExpectedSequence
	if _, err := l.Apply(stale); !errors.Is(err, ErrSequence) {
		t.Fatalf("stale command: %v", err)
	}
	if l.Snapshot().Sequence != c.ExpectedSequence+1 {
		t.Fatal("duplicate commit")
	}
	rows, err := l.Audit()
	if err != nil || rows[0] != (TrialRow{"GBP", "18446744073709551614", "18446744073709551614", "0"}) {
		t.Fatalf("overflowed audit %v %v", rows, err)
	}
}

func TestAccountLifecycleAndReserveRelease(t *testing.T) {
	l := fixture(t)
	fund(t, l, 10, 1, 1, 60)
	apply(t, l, open(20, 2, 2, 3, 60))
	apply(t, l, move(21, 2, domain.Pending, domain.Authorized, 2, nil))
	freeze := command(22)
	freeze.ChangeAccount = &ChangeAccount{2, domain.Active, domain.Frozen}
	apply(t, l, freeze)
	mustReject(t, l, move(23, 2, domain.Authorized, domain.Cleared, 0, journal(2, 0, domain.Booking,
		posting(2, domain.Debit, "GBP", 60), posting(3, domain.Credit, "GBP", 60))))
	apply(t, l, move(24, 2, domain.Authorized, domain.Cancelled, 0, nil))
	a, _ := l.Account(2)
	if a.Reserved != "0" {
		t.Fatal("cancellation did not release reserve")
	}
	dormant := command(25)
	dormant.ChangeAccount = &ChangeAccount{2, domain.Frozen, domain.Dormant}
	mustReject(t, l, dormant)
	dormant.RecordedAt = baseTime.Add(time.Hour)
	apply(t, l, dormant)
	closeAccount := command(26)
	closeAccount.RecordedAt = dormant.RecordedAt
	closeAccount.ChangeAccount = &ChangeAccount{2, domain.Dormant, domain.Closed}
	mustReject(t, l, closeAccount)
	closeAccount.ChangeAccount = &ChangeAccount{3, domain.Active, domain.Closed}
	apply(t, l, closeAccount)
}
