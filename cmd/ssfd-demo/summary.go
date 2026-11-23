package main

import (
	"ssfd-engine/agents"
	"ssfd-engine/chaos"
	"ssfd-engine/domain"
	"ssfd-engine/ledger"
	"ssfd-engine/simclock"
)

type summary struct {
	Version                                  string
	Seed                                     uint64
	Start, EndExclusive                      simclock.Date
	Actors, Accounts, Transactions, Journals int
	Cleared, Declined                        map[agents.Activity]int
	Loans                                    []agents.LoanView
	TrialBalance                             []ledger.TrialRow
}

func summarize(result agents.Result) summary {
	out := summary{Version: result.Version, Seed: result.Seed, Start: result.Start, EndExclusive: result.EndExclusive,
		Actors: len(result.Actors), Accounts: len(result.Snapshot.Accounts), Transactions: len(result.Snapshot.Transactions),
		Journals: len(result.Snapshot.Journals), Cleared: make(map[agents.Activity]int), Declined: make(map[agents.Activity]int),
		Loans: result.Loans, TrialBalance: result.TrialBalance}
	for _, event := range result.Events {
		switch event.Outcome {
		case domain.Cleared:
			out.Cleared[event.Activity]++
		case domain.Declined:
			out.Declined[event.Activity]++
		}
	}
	return out
}

// Activity maps in summary count behavioral events. Chaos publishes separate
// counters while Accounts, Transactions, Journals and the audit span the book.
func summarizeChaos(result agents.ChaosResult) any {
	return struct {
		summary
		ChaosVersion string
		ChaosSeed    uint64
		Chaos        []chaos.FaultStats
	}{summarize(result.Result), result.ChaosVersion, result.ChaosSeed, result.Chaos}
}
