package eval

import (
	"reflect"
	"testing"
	"time"

	"ssfd-engine/simclock"
)

func TestMutantsHaveIndependentExpectedWitnesses(t *testing.T) {
	london, err := simclock.London.Location()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)
	fold := time.Date(2024, 10, 27, 0, 30, 0, 0, time.UTC)
	pay := operation{kind: transfer, key: 1, amount: 50, source: "GBP", target: "GBP"}
	auth := pay
	auth.kind = authorize
	clearing := pay
	clearing.kind = clear
	interleaved := pay
	interleaved.key = 2
	other := pay
	other.target = "USD"
	undo := pay
	undo.kind = reverse
	partial := undo
	partial.kind, partial.amount = refund, 20
	over := partial
	over.amount = 31
	testCases := []struct {
		mutant   Mutation
		scenario scenario
		verify   func(outcome) bool
	}{
		{HalfUpFX, ordinary("tie", operation{kind: fx, amount: 2}), func(o outcome) bool { return o.Quote == 2 }},
		{DuplicateDebit, ordinary("duplicate", pay, pay), func(o outcome) bool {
			return o.Status == "same receipt" && o.SourceBalance == ample-50 && o.Postings == 2
		}},
		{SkipLeapInterest, ordinary("leap", operation{kind: accrue, amount: 50, at: at}), func(o outcome) bool { return o.Accrued == 50 && o.Postings == 2 }},
		{CollapseDSTFold, ordinary("fold", operation{kind: timestamp, at: fold.In(london)}, operation{kind: timestamp, at: fold.Add(time.Hour).In(london)}), func(o outcome) bool { return o.Postings == 4 }},
		{FloorOffByOne, scenario{name: "floor", sequence: []operation{pay}, funds: 0, floor: -50}, func(o outcome) bool { return o.Status == "accepted" && o.SourceBalance == -50 }},
		{RefundCap, ordinary("over refund", pay, partial, over), func(o outcome) bool { return o.Status == "refund exceeds remaining principal" && o.Remaining == 30 }},
		{CrossCurrencyDebit, ordinary("wrong currency", other), func(o outcome) bool { return o.Status == "currency mismatch" && o.Postings == 0 }},
		{FrozenDebit, ordinary("freeze", auth, operation{kind: freeze}, clearing), func(o outcome) bool { return o.Status == "frozen" && o.Postings == 0 }},
		{EarlyClearing, ordinary("early", clearing), func(o outcome) bool { return o.Status == "unauthorized" && o.Postings == 0 }},
		{ReversalCredit, ordinary("reverse", pay, undo), func(o outcome) bool { return o.SourceBalance == ample && o.TargetBalance == 0 }},
		{CutoffRound, ordinary("midnight", operation{kind: timestamp, at: at.Add(-time.Nanosecond)}), func(o outcome) bool { return o.BookDate == "2024-02-28" }},
		{ShortCredit, ordinary("credit balance", pay), func(o outcome) bool { return o.TargetBalance == 50 && o.SourceBalance == ample-50 }},
		{LostUpdate, ordinary("interleaved", auth, interleaved, clearing), func(o outcome) bool { return o.SourceBalance == ample-100 && o.TargetBalance == 100 }},
	}
	for _, tc := range testCases {
		t.Run(string(tc.mutant), func(t *testing.T) {
			clean, mutant := newSUT(tc.scenario, ""), newSUT(tc.scenario, tc.mutant)
			var a, b outcome
			for _, step := range tc.scenario.sequence {
				a, b = clean.step(step), mutant.step(step)
			}
			if !tc.verify(a) || a == b {
				t.Fatalf("clean %+v mutant %+v", a, b)
			}
		})
	}
	if len(testCases) != len(Mutations()) {
		t.Fatal("untested mutant")
	}
}

func TestSeededStrategies(t *testing.T) {
	seeds := []uint64{42, 43}
	a, err := Evaluate(seeds, 192)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Evaluate(seeds, 192)
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("experiment was not repeatable: %v", err)
	}
	for _, r := range a.Runs {
		for _, s := range r.Strategies {
			if s.Cases != 192 {
				t.Fatalf("unmatched case budget %+v", s)
			}
		}
	}
	if a.Summary[2].KillsByMutation[DuplicateDebit] != 2 || a.Summary[0].KillsByMutation[CrossCurrencyDebit] != 2 ||
		a.Summary[2].KillsByMutation[CrossCurrencyDebit] != 0 || a.Summary[2].Kills <= a.Summary[0].Kills {
		t.Fatalf("unexpected study control results %+v", a.Summary)
	}
	subset, err := EvaluateSelected(seeds, 32, []Mutation{DuplicateDebit})
	if err != nil || subset.Summary[2].MutantRuns != 2 || subset.Summary[2].ScorePercent != 100 {
		t.Fatalf("selected mutant run %+v %v", subset, err)
	}
	if _, err := EvaluateSelected(seeds, 32, []Mutation{"typo"}); err == nil {
		t.Fatal("accepted an unknown mutant")
	}
	if signP(12, 0) != 0.00048828125 || signP(0, 0) != 1 || signP(2, 2) != 1 {
		t.Fatal("miscomputed sign test")
	}
}
