package agents

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"ssfd-engine/domain"
	"ssfd-engine/simclock"
)

func TestDayCountsAndExactRounding(t *testing.T) {
	for _, tc := range []struct {
		convention DayCount
		start, end date
		n, d       int64
	}{
		{Actual365Fixed, date{Year: 2024, Month: 2, Day: 28}, date{Year: 2024, Month: 3, Day: 1}, 2, 365},
		{Thirty360ISDA, date{Year: 2024, Month: 1, Day: 31}, date{Year: 2024, Month: 2, Day: 29}, 29, 360},
		{Thirty360ISDA, date{Year: 2024, Month: 2, Day: 29}, date{Year: 2024, Month: 3, Day: 31}, 32, 360},
		{Thirty360ISDA, date{Year: 2024, Month: 1, Day: 30}, date{Year: 2024, Month: 1, Day: 31}, 0, 360},
	} {
		n, d, err := tc.convention.Fraction(tc.start, tc.end)
		if err != nil || n != tc.n || d != tc.d {
			t.Fatalf("%+v: got %d/%d: %v", tc, n, d, err)
		}
	}
	start := date{Year: 2024, Month: 3, Day: 1}
	end := date{Year: 2024, Month: 3, Day: 2}
	for _, tc := range []struct{ principal, interest int64 }{{365, 0}, {1095, 2}} {
		terms := LoanTerms{tc.principal, 5000, 1, Actual365Fixed}
		got, err := terms.Payment(tc.principal, 1, start, end)
		if err != nil || got != (Installment{tc.principal, tc.interest, tc.principal + tc.interest}) {
			t.Fatalf("half-minor-unit rounding got %+v: %v", got, err)
		}
	}
	terms := LoanTerms{10, 0, 3, Thirty360ISDA}
	got, err := terms.Payment(4, 3, start, end)
	if err != nil || got != (Installment{4, 0, 4}) {
		t.Fatalf("final payment lost minor units: %+v %v", got, err)
	}
	if _, err := terms.Payment(3, 3, start, end); err == nil {
		t.Fatal("accepted mismatched balance")
	}
	if _, _, err := Actual365Fixed.Fraction(end, start); err == nil {
		t.Fatal("accepted backward day count")
	}
}

func TestAnnualReplayAndAudit(t *testing.T) {
	cfg := Config{Seed: 42, StartYear: 2024, Years: 1}
	r, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a := encodeRun(t, r)
	again, err := Run(cfg)
	if err != nil || !bytes.Equal(a, encodeRun(t, again)) {
		t.Fatalf("same seed changed full snapshot: %v", err)
	}
	cfg.Seed++
	other, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(other.Events)
	if err != nil {
		t.Fatal(err)
	}
	c, err := json.Marshal(r.Events)
	if err != nil || bytes.Equal(b, c) {
		t.Fatal("changing the seed did not change the event stream")
	}

	loc, err := simclock.London.Location()
	if err != nil {
		t.Fatal(err)
	}
	cal, err := simclock.NewCalendar(simclock.EnglandWales)
	if err != nil {
		t.Fatal(err)
	}
	count := make(map[Activity]int)
	var previous time.Time
	var lastEntry domain.EntryID
	for _, e := range r.Events {
		if e.At.Before(previous) || e.Outcome != domain.Cleared {
			t.Fatalf("unsorted or unexpectedly declined event: %+v", e)
		}
		previous = e.At
		count[e.Activity]++
		if e.Entry <= lastEntry {
			t.Fatal("non-increasing journal sequence")
		}
		lastEntry = e.Entry
		switch e.Activity {
		case Payroll, Rent, Mortgage, BusinessLoan, Utility, Subscription, CashSweep:
			open, err := cal.IsBusinessDay(cal.DateAt(e.At))
			if err != nil || !open || e.At.In(loc).Hour() != 9 {
				t.Fatalf("recurring payment did not settle at local banking time: %+v", e)
			}
		case Card, PeerToPeer, CrossBorder:
			if e.At.In(loc).Hour() != 18 || e.Debtor == e.Creditor {
				t.Fatalf("invalid daily payment time or parties: %+v", e)
			}
		}
		if e.Activity == CrossBorder && (e.Quote == nil || e.Quote.Target.Currency != "USD") {
			t.Fatalf("invalid FX attribution: %+v", e)
		}
	}
	for kind, want := range map[Activity]int{Payroll: 72, Rent: 48, Mortgage: 12, BusinessLoan: 12, Utility: 72, Subscription: 72, LoanAllocation: 24} {
		if count[kind] != want {
			t.Errorf("%s: got %d, want %d", kind, count[kind], want)
		}
	}
	if count[Card] == 0 || count[PeerToPeer] == 0 || count[CrossBorder] == 0 {
		t.Fatal("missing stochastic channels")
	}
	if len(r.Actors) != 12 || len(r.Snapshot.Accounts) != 32 || len(r.Snapshot.Journals) != len(r.Events) {
		t.Fatal("population or simulation bookkeeping is incomplete")
	}
	for _, a := range r.Actors {
		if len(a.Accounts) < 2 {
			t.Fatalf("actor has no secondary account: %+v", a)
		}
	}
	requireBalanced(t, r)
}

func TestLoanMaturityAndHorizon(t *testing.T) {
	r, err := Run(Config{Seed: 7, StartYear: 2027, Years: 2})
	if err != nil {
		t.Fatal(err)
	}
	if r.Loans[0].Paid != 24 || r.Loans[0].Outstanding != 21_600_000 ||
		r.Loans[1].Paid != 18 || r.Loans[1].Outstanding != 0 {
		t.Fatalf("incorrect loan maturity results: %+v", r.Loans)
	}
	requireBalanced(t, r)
	if _, err := Run(Config{Seed: 7, StartYear: 2028, Years: 2}); err == nil {
		t.Fatal("accepted unsupported calendar horizon")
	}
}

func TestInsufficientFundsRecordsDeclineWithoutJournal(t *testing.T) {
	e, err := newEngine(1, date{Year: 2024, Month: 1, Day: 1}, date{Year: 2025, Month: 1, Day: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.initialize(); err != nil {
		t.Fatal(err)
	}
	actor, peer := e.p.retail[0], e.p.retail[1]
	n, err := e.available(actor.Accounts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	before := e.book.Snapshot()
	cleared, err := e.transfer(PeerToPeer, actor, peer, actor.Accounts[0], peer.Accounts[0], n+1)
	after := e.book.Snapshot()
	last := e.events[len(e.events)-1]
	if err != nil || cleared || last.Outcome != domain.Declined || last.Entry != 0 ||
		len(before.Journals) != len(after.Journals) || after.Transactions[len(after.Transactions)-1].State != domain.Declined {
		t.Fatalf("invalid decline: cleared %t, event %+v, error %v", cleared, last, err)
	}
}

func encodeRun(t *testing.T, r Result) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func requireBalanced(t *testing.T, r Result) {
	t.Helper()
	if len(r.TrialBalance) != 2 || r.TrialBalance[0].Currency != "GBP" || r.TrialBalance[1].Currency != "USD" {
		t.Fatalf("missing currency audit: %+v", r.TrialBalance)
	}
	for _, row := range r.TrialBalance {
		if row.Difference != "0" || row.Debits != row.Credits || row.Debits == "0" {
			t.Fatalf("unexpected trial balance: %+v", row)
		}
	}
}
