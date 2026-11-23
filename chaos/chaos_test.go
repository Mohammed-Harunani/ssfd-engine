package chaos_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"testing"
	"time"

	"ssfd-engine/agents"
	"ssfd-engine/chaos"
	"ssfd-engine/domain"
	"ssfd-engine/ledger"
	"ssfd-engine/simclock"
)

func TestAggressiveFullYear(t *testing.T) {
	cfg := agents.Config{Seed: 42, StartYear: 2024, Years: 1}
	spec, err := chaos.Profile("aggressive", cfg.Seed)
	if err != nil {
		t.Fatal(err)
	}
	r, err := agents.RunWithChaos(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	want := map[chaos.Fault]chaos.FaultStats{
		chaos.Midnight:   {Eligible: 366, Injected: 366, Journals: 1097},
		chaos.LeapDay:    {Eligible: 1, Injected: 1, Journals: 2},
		chaos.DSTOverlap: {Eligible: 1, Injected: 1, Journals: 2},
		chaos.Ordering:   {Eligible: 365, Injected: 365, Journals: 365, DelayedSettlements: 365, Rejected: 365},
		chaos.Duplicate:  {Eligible: 366, Injected: 366, Journals: 366, IdenticalRetries: 366, Rejected: 732},
		chaos.Overdraft:  {Eligible: 366, Injected: 366, Journals: 732, Rejected: 366},
		chaos.Chargeback: {Eligible: 366, Injected: 366, Journals: 1098, Rejected: 366},
		chaos.Freeze:     {Eligible: 366, Injected: 366, Journals: 732, Rejected: 366},
	}
	var injectedJournals uint64
	for _, row := range r.Chaos {
		expected, ok := want[row.Fault]
		expected.Fault, expected.WeightBPS = row.Fault, 10000
		if !ok || row != expected {
			t.Errorf("unexpected fault counters: %+v, want %+v", row, expected)
		}
		injectedJournals += row.Journals
		delete(want, row.Fault)
	}
	if len(want) != 0 {
		t.Fatalf("missing scenarios: %v", want)
	}
	if injectedJournals != 4394 || len(r.Snapshot.Journals) != len(r.Events)+int(injectedJournals)+2 {
		t.Fatalf("unexpected journal count %d with %d fault journals", len(r.Snapshot.Journals), injectedJournals)
	}
	checkInvariants(t, r)
	checkBoundaryHistory(t, r)
	checkWorkflowHistory(t, r)

	clean, err := agents.Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(clean.Events, r.Events) || !reflect.DeepEqual(clean.Loans, r.Loans) {
		t.Fatal("chaos changed the normal behavioral draw stream")
	}
	buf, err := json.Marshal(clean)
	if err != nil {
		t.Fatal(err)
	}
	const baseline = "902af1261de3309b0ea47c639545b6a16ca6fcf3cdf88979767aa085b7e5628e"
	if digest := fmt.Sprintf("%x", sha256.Sum256(append(buf, '\n'))); digest != baseline {
		t.Fatalf("baseline bytes changed: %s", digest)
	}
}

func TestWeightedSelectionReplayAndValidation(t *testing.T) {
	cfg := agents.Config{Seed: 13, StartYear: 2025, Years: 1}
	spec := chaos.Config{Seed: 19, Weights: map[chaos.Fault]uint16{chaos.Duplicate: 5000}}
	a, err := agents.RunWithChaos(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := agents.RunWithChaos(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(v any) []byte {
		t.Helper()
		buf, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return buf
	}
	if !bytes.Equal(encode(a), encode(b)) {
		t.Fatal("weighted fault run was not deterministic")
	}
	spec.Seed++
	c, err := agents.RunWithChaos(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(encode(a.Chaos), encode(c.Chaos)) && reflect.DeepEqual(a.Snapshot.Journals, c.Snapshot.Journals) {
		t.Fatal("changing fault seed did not affect weighted scenarios")
	}
	for _, row := range a.Chaos {
		if row.Fault == chaos.Duplicate {
			if row.Injected == 0 || row.Injected >= row.Eligible || row.IdenticalRetries != row.Injected {
				t.Fatalf("bad weighted replay counters: %+v", row)
			}
		} else if row.Injected != 0 {
			t.Fatalf("disabled scenario activated: %+v", row)
		}
	}
	checkInvariants(t, a)
	for _, weights := range []map[chaos.Fault]uint16{{chaos.Duplicate: 10001}, {"unknown": 0}} {
		if _, err := agents.RunWithChaos(cfg, chaos.Config{Weights: weights}); err == nil {
			t.Fatalf("accepted invalid fault weights: %v", weights)
		}
	}
}

func checkInvariants(t *testing.T, r agents.ChaosResult) {
	t.Helper()
	seen := make(map[domain.EntryID]bool)
	var seq uint64
	for _, e := range r.Snapshot.Journals {
		if seen[e.ID] || e.CommitSequence <= seq || len(e.Postings) < 2 {
			t.Fatalf("invalid journal order or identity at %d", e.ID)
		}
		seen[e.ID], seq = true, e.CommitSequence
		totals := make(map[domain.Currency]*big.Int)
		for _, p := range e.Postings {
			if totals[p.Amount.Currency] == nil {
				totals[p.Amount.Currency] = new(big.Int)
			}
			m := big.NewInt(p.Amount.Minor)
			switch p.Side {
			case domain.Credit:
				totals[p.Amount.Currency].Sub(totals[p.Amount.Currency], m)
			case domain.Debit:
				totals[p.Amount.Currency].Add(totals[p.Amount.Currency], m)
			default:
				t.Fatalf("invalid posting side in journal %d", e.ID)
			}
		}
		for code, total := range totals {
			if total.Sign() != 0 {
				t.Fatalf("journal %d in %s differs by %s", e.ID, code, total)
			}
		}
	}
	if len(r.TrialBalance) != 2 || r.TrialBalance[0].Currency != "GBP" || r.TrialBalance[1].Currency != "USD" {
		t.Fatalf("expected GBP and USD trial balances, got %v", r.TrialBalance)
	}
	for _, row := range r.TrialBalance {
		if row.Difference != "0" || row.Debits != row.Credits || row.Debits == "0" {
			t.Fatalf("invalid trial balance: %+v", row)
		}
	}
	for _, a := range r.Snapshot.Accounts {
		if a.Reserved != "0" || a.HoldCount != 0 || a.Account.State == domain.Frozen {
			t.Fatalf("scenario left reserved or held funds: %+v", a)
		}
	}
}

func checkBoundaryHistory(t *testing.T, r agents.ChaosResult) {
	t.Helper()
	loc, err := simclock.London.Location()
	if err != nil {
		t.Fatal(err)
	}
	before := time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC).Add(-time.Nanosecond)
	midnight := before.Add(time.Nanosecond)
	localOverlap := time.Date(2024, 10, 27, 0, 30, 0, 0, time.UTC)
	var pre, at, interest, early, late int
	classes := make(map[domain.AccountID]domain.AccountClass)
	for _, a := range r.Snapshot.Accounts {
		classes[a.Account.ID] = a.Account.Class
	}
	for _, e := range r.Snapshot.Journals {
		switch {
		case e.RecordedAt.Equal(before):
			pre++
		case e.RecordedAt.Equal(midnight):
			at++
			if len(e.Postings) == 2 && classes[e.Postings[0].Account] == domain.Asset &&
				classes[e.Postings[1].Account] == domain.Revenue && e.Postings[0].Amount.Minor == 50 {
				interest++
			}
		case e.RecordedAt.Equal(localOverlap), e.RecordedAt.Equal(localOverlap.Add(time.Hour)):
			if e.RecordedAt.In(loc).Hour() != 1 || e.RecordedAt.In(loc).Minute() != 30 {
				t.Fatal("overlap journals did not share exact local time")
			}
			if e.RecordedAt.Equal(localOverlap) {
				early++
			} else {
				late++
			}
		}
	}
	if pre != 1 || at != 4 || interest != 1 || early != 1 || late != 1 {
		t.Fatalf("boundary counts before %d at %d interest %d folds %d/%d", pre, at, interest, early, late)
	}
}

func checkWorkflowHistory(t *testing.T, r agents.ChaosResult) {
	t.Helper()
	entries := make(map[domain.EntryID]domain.JournalEntry)
	byTransaction := make(map[domain.TransactionID][]domain.JournalEntry)
	for _, j := range r.Snapshot.Journals {
		entries[j.ID] = j
		byTransaction[j.Transaction] = append(byTransaction[j.Transaction], j)
	}
	auth := make(map[domain.TransactionID]time.Time)
	delayed, disputed, declined, reversed := 0, 0, 0, 0
	for _, e := range r.Snapshot.TransactionEvents {
		switch e.To {
		case domain.Authorized:
			auth[e.Transaction] = e.RecordedAt
		case domain.Cleared:
			if first := auth[e.Transaction]; !first.IsZero() && e.RecordedAt.Sub(first) >= 23*time.Hour {
				delayed++
			}
		case domain.Disputed:
			disputed++
		case domain.Declined:
			declined++
			if len(byTransaction[e.Transaction]) != 0 {
				t.Fatalf("declined transaction %d posted", e.Transaction)
			}
		case domain.Reversed:
			reversed++
			last := entries[e.Journal]
			original := entries[last.Original]
			if last.Kind == domain.Adjustment {
				js := byTransaction[e.Transaction]
				if len(js) != 3 || js[1].Kind != domain.Adjustment || js[1].Original != original.ID ||
					js[1].Postings[0].Amount.Minor+last.Postings[0].Amount.Minor != original.Postings[0].Amount.Minor {
					t.Fatalf("refund and chargeback fail to unwind original transaction %d", e.Transaction)
				}
			} else if last.Kind != domain.Reversal || original.ID == 0 {
				t.Fatalf("missing original booking on reversal %d", e.Transaction)
			}
		}
	}
	if delayed != 365 || disputed != 366 || reversed != 1098 || declined != 366 || len(r.Snapshot.HoldEvents) != 732 {
		t.Fatalf("unexpected workflow counts: %d %d %d %d holds %d", delayed, disputed, reversed, declined, len(r.Snapshot.HoldEvents))
	}
	for idx, e := range r.Snapshot.HoldEvents {
		if e.Place != (idx%2 == 0) || e.Hold != 1 || (e.ChangeHold == (ledger.ChangeHold{})) {
			t.Fatalf("invalid hold history: %+v", e)
		}
	}
}
