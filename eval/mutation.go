package eval

import (
	"fmt"
	"math/big"
	"slices"
	"sort"
	"time"

	"ssfd-engine/agents"
	"ssfd-engine/chaos"
	"ssfd-engine/domain"
	"ssfd-engine/prng"
	"ssfd-engine/simclock"
)

type Mutation string

const (
	HalfUpFX           Mutation = "fx_ties_round_up"
	DuplicateDebit     Mutation = "missing_idempotency"
	SkipLeapInterest   Mutation = "leap_day_interest_omitted"
	CollapseDSTFold    Mutation = "dst_fold_collision"
	FloorOffByOne      Mutation = "overdraft_floor_exclusive"
	RefundCap          Mutation = "refund_cap_missing"
	CrossCurrencyDebit Mutation = "currency_mismatch_accepted"
	FrozenDebit        Mutation = "frozen_debit_bypass"
	EarlyClearing      Mutation = "clearing_without_authorization"
	ReversalCredit     Mutation = "reversal_credit_omitted"
	CutoffRound        Mutation = "cutoff_rounded_to_second"
	ShortCredit        Mutation = "settlement_credit_short_one"
	LostUpdate         Mutation = "stale_authorization_balance_write"
)

var mutations = [...]Mutation{HalfUpFX, DuplicateDebit, SkipLeapInterest, CollapseDSTFold, FloorOffByOne, RefundCap,
	CrossCurrencyDebit, FrozenDebit, EarlyClearing, ReversalCredit, CutoffRound, ShortCredit, LostUpdate}

func Mutations() []Mutation { return append([]Mutation(nil), mutations[:]...) }

type Strategy string

const (
	Naive      Strategy = "naive"
	Happy      Strategy = "ssfd"
	Aggressive Strategy = "ssfd-chaos"
)

var strategies = [...]Strategy{Naive, Happy, Aggressive}

type opKind uint8

const (
	transfer opKind = iota + 1
	authorize
	clear
	freeze
	refund
	reverse
	fx
	accrue
	timestamp
)

type operation struct {
	kind           opKind
	key            uint64
	amount         int64
	source, target domain.Currency
	at             time.Time
}

type scenario struct {
	name         string
	sequence     []operation
	funds, floor int64
}

const ample int64 = 10_000_000

func ordinary(name string, ops ...operation) scenario {
	return scenario{name: name, sequence: ops, funds: ample}
}

type outcome struct {
	Status                                                  string
	BookDate                                                string
	SourceBalance, TargetBalance, Remaining, Accrued, Quote int64
	Postings                                                int
}

// sut is a two-account control ledger with explicit authorization, idempotency
// and refundable principal. It is separate from the canonical SSFD ledger.
type authorization struct{ amount, balance int64 }

type sut struct {
	fault                                           Mutation
	funds, target, floor, remaining, accrued, quote int64
	postings                                        int
	frozen                                          bool
	authorized                                      map[uint64]authorization
	receipts                                        map[uint64]operation
	wallSeconds                                     map[string]time.Time
}

func newSUT(sc scenario, fault Mutation) *sut {
	return &sut{fault: fault, funds: sc.funds, floor: sc.floor, authorized: make(map[uint64]authorization),
		receipts: make(map[uint64]operation), wallSeconds: make(map[string]time.Time)}
}

func (s *sut) step(op operation) outcome {
	status, date := "accepted", ""
	switch op.kind {
	case authorize:
		s.authorized[op.key] = authorization{op.amount, s.funds}
	case freeze:
		s.frozen = true
	case clear, transfer:
		existing, seen := s.receipts[op.key]
		switch {
		case s.fault != DuplicateDebit && seen && existing == op:
			status = "same receipt"
		case s.fault != DuplicateDebit && seen:
			status = "key conflict"
		case op.kind == clear && s.authorized[op.key].amount != op.amount && s.fault != EarlyClearing:
			status = "unauthorized"
		case op.source != op.target && s.fault != CrossCurrencyDebit:
			status = "currency mismatch"
		case s.frozen && s.fault != FrozenDebit:
			status = "frozen"
		case s.funds-op.amount < s.floor || (s.fault == FloorOffByOne && s.funds-op.amount == s.floor):
			status = "insufficient funds"
		default:
			if op.kind == clear && s.fault == LostUpdate {
				s.funds = s.authorized[op.key].balance - op.amount
			} else {
				s.funds -= op.amount
			}
			s.target += op.amount
			if s.fault == ShortCredit {
				s.target--
			}
			s.remaining += op.amount
			s.postings += 2
			s.receipts[op.key] = op
		}
	case refund, reverse:
		if op.amount > s.remaining && s.fault != RefundCap {
			status = "refund exceeds remaining principal"
		} else {
			s.remaining -= op.amount
			s.target -= op.amount
			if op.kind != reverse || s.fault != ReversalCredit {
				s.funds += op.amount
			}
			s.postings += 2
		}
	case fx:
		n := op.amount * 5
		s.quote = n / 4
		if r := n % 4; r > 2 || (r == 2 && (s.fault == HalfUpFX || s.quote%2 != 0)) {
			s.quote++
		}
	case accrue:
		if s.fault != SkipLeapInterest || op.at.Month() != time.February || op.at.Day() != 29 {
			s.accrued += op.amount
			s.postings += 2
		}
	case timestamp:
		wall := op.at.Format("2006-01-02T15:04:05")
		previous, seen := s.wallSeconds[wall]
		if s.fault == CollapseDSTFold && seen && !previous.Equal(op.at) {
			status = "local timestamp collision"
		} else {
			s.wallSeconds[wall] = op.at
			at := op.at
			if s.fault == CutoffRound {
				at = at.Round(time.Second)
			}
			date = at.Format(time.DateOnly)
			s.postings += 2
		}
	}
	return outcome{status, date, s.funds, s.target, s.remaining, s.accrued, s.quote, s.postings}
}

type StrategyResult struct {
	Strategy          Strategy
	Cases, Operations int
	Killed            []Mutation
	FirstWitness      map[Mutation]string
}

type SeedResult struct {
	Seed       uint64
	Strategies []StrategyResult
}

type MutationSummary struct {
	Strategy                 Strategy
	Seeds, MutantRuns, Kills int
	ScorePercent             float64
	KillsByMutation          map[Mutation]int
}

type PairedComparison struct {
	First, Second                   Strategy
	FirstHigher, SecondHigher, Ties int
	MeanDifferencePP                float64
	TwoSidedExactP                  float64
}

type MutationReport struct {
	CasesPerSeed int
	Mutants      []Mutation
	Runs         []SeedResult
	Summary      []MutationSummary
	Paired       []PairedComparison
}

// Evaluate compares the clean SUT with each independently enabled mutation
// on the same cases, and pairs strategy scores by scenario seed.
func Evaluate(seeds []uint64, budget int) (MutationReport, error) {
	return EvaluateSelected(seeds, budget, Mutations())
}

// EvaluateSelected enables the listed mutants one at a time.
func EvaluateSelected(seeds []uint64, budget int, selected []Mutation) (MutationReport, error) {
	if len(seeds) == 0 || budget < 16 || budget > 1000 {
		return MutationReport{}, fmt.Errorf("seeds and a 16–1000 case budget required")
	}
	if len(selected) == 0 {
		return MutationReport{}, fmt.Errorf("at least one selected mutant required")
	}
	seenMutant := make(map[Mutation]bool)
	for _, m := range selected {
		if !slices.Contains(mutations[:], m) || seenMutant[m] {
			return MutationReport{}, fmt.Errorf("unknown or repeated mutant %q", m)
		}
		seenMutant[m] = true
	}
	report := MutationReport{CasesPerSeed: budget, Mutants: append([]Mutation(nil), selected...)}
	seen := make(map[uint64]bool)
	for _, seed := range seeds {
		if seen[seed] {
			return report, fmt.Errorf("repeated experiment seed %d", seed)
		}
		seen[seed] = true
		cfg := agents.Config{Seed: seed, StartYear: 2024, Years: 1}
		a, err := agents.Run(cfg)
		if err != nil {
			return report, err
		}
		spec, err := chaos.Profile("aggressive", seed)
		if err != nil {
			return report, err
		}
		b, err := agents.RunWithChaos(cfg, spec)
		if err != nil {
			return report, err
		}
		groups, err := fromChaos(b)
		if err != nil {
			return report, err
		}
		happy := fromAgents(a)
		datasets := [][]scenario{naiveCases(seed, budget), sample(happy, seed, budget), sample(append(happy, groups...), seed, budget)}
		row := SeedResult{Seed: seed}
		for i, cs := range datasets {
			if len(cs) != budget {
				return report, fmt.Errorf("%s seed %d has %d cases; want %d", strategies[i], seed, len(cs), budget)
			}
			row.Strategies = append(row.Strategies, score(strategies[i], cs, report.Mutants))
		}
		report.Runs = append(report.Runs, row)
	}
	report.summarize()
	return report, nil
}

func score(strategy Strategy, cases []scenario, selected []Mutation) StrategyResult {
	r := StrategyResult{Strategy: strategy, Cases: len(cases), FirstWitness: make(map[Mutation]string)}
	for _, sc := range cases {
		r.Operations += len(sc.sequence)
	}
	for _, fault := range selected {
		for _, sc := range cases {
			clean, mutant := newSUT(sc, ""), newSUT(sc, fault)
			for _, op := range sc.sequence {
				expected, actual := clean.step(op), mutant.step(op)
				if expected != actual {
					r.Killed = append(r.Killed, fault)
					r.FirstWitness[fault] = sc.name
					break
				}
			}
			if r.FirstWitness[fault] != "" {
				break
			}
		}
	}
	return r
}

func (r *MutationReport) summarize() {
	for i, strategy := range strategies {
		s := MutationSummary{Strategy: strategy, Seeds: len(r.Runs), MutantRuns: len(r.Runs) * len(r.Mutants), KillsByMutation: make(map[Mutation]int)}
		for _, m := range r.Mutants {
			s.KillsByMutation[m] = 0
		}
		for _, rep := range r.Runs {
			for _, fault := range rep.Strategies[i].Killed {
				s.KillsByMutation[fault]++
				s.Kills++
			}
		}
		s.ScorePercent = 100 * float64(s.Kills) / float64(s.MutantRuns)
		r.Summary = append(r.Summary, s)
	}
	for _, p := range [][2]int{{1, 0}, {2, 0}, {2, 1}} {
		v := PairedComparison{First: strategies[p[0]], Second: strategies[p[1]]}
		net := 0
		for _, row := range r.Runs {
			d := len(row.Strategies[p[0]].Killed) - len(row.Strategies[p[1]].Killed)
			net += d
			switch {
			case d > 0:
				v.FirstHigher++
			case d < 0:
				v.SecondHigher++
			default:
				v.Ties++
			}
		}
		v.MeanDifferencePP = 100 * float64(net) / float64(len(r.Runs)*len(r.Mutants))
		v.TwoSidedExactP = signP(v.FirstHigher, v.SecondHigher)
		r.Paired = append(r.Paired, v)
	}
}

func signP(a, b int) float64 {
	n := a + b
	if n == 0 {
		return 1
	}
	var sum big.Int
	for k := 0; k <= min(a, b); k++ {
		sum.Add(&sum, new(big.Int).Binomial(int64(n), int64(k)))
	}
	sum.Lsh(&sum, 1)
	den := new(big.Int).Lsh(big.NewInt(1), uint(n))
	if sum.Cmp(den) >= 0 {
		return 1
	}
	p, _ := new(big.Rat).SetFrac(&sum, den).Float64()
	return p
}

func sample(groups [][]scenario, seed uint64, budget int) []scenario {
	rng := prng.New(seed)
	for i, g := range groups {
		groups[i] = append([]scenario(nil), g...)
		for k := len(g) - 1; k > 0; k-- {
			j, _ := rng.Below(uint64(k + 1))
			groups[i][k], groups[i][j] = groups[i][j], groups[i][k]
		}
	}
	out := make([]scenario, 0, budget)
	for row := 0; len(out) < budget; row++ {
		before := len(out)
		for _, g := range groups {
			if row < len(g) {
				out = append(out, g[row])
			}
			if len(out) == budget {
				break
			}
		}
		if before == len(out) {
			break
		}
	}
	return out
}

func naiveCases(seed uint64, n int) []scenario {
	rng := prng.New(seed)
	draw := func(bound uint64) int64 { v, _ := rng.Below(bound); return int64(v) }
	codes := [2]domain.Currency{"GBP", "USD"}
	date := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	out := make([]scenario, 0, n)
	for i := range n {
		at := date.AddDate(0, 0, int(draw(366)))
		op := operation{kind: transfer, key: uint64(i + 1), amount: 1 + draw(10000), source: codes[draw(2)], target: codes[draw(2)], at: at}
		switch draw(5) {
		case 0:
			op.kind = fx
		case 1:
			op.kind = accrue
		case 2:
			op.kind = timestamp
		}
		out = append(out, ordinary(fmt.Sprintf("independent row %d", i+1), op))
	}
	return out
}

func fromAgents(r agents.Result) [][]scenario {
	groups := make(map[agents.Activity][]scenario)
	for _, e := range r.Events {
		if e.Outcome != domain.Cleared {
			continue
		}
		name := fmt.Sprintf("%s transaction %d", e.Activity, e.Transaction)
		op := operation{key: uint64(e.Transaction), amount: e.Principal.Minor, source: e.Principal.Currency, target: e.Principal.Currency, at: e.At}
		switch {
		case e.Quote != nil:
			op.kind = fx
			groups[e.Activity] = append(groups[e.Activity], ordinary(name, op))
		case e.Activity == agents.Mortgage || e.Activity == agents.BusinessLoan:
			if e.LoanPayment != nil && e.LoanPayment.Interest > 0 {
				op.kind, op.amount = accrue, e.LoanPayment.Interest
				groups[e.Activity] = append(groups[e.Activity], ordinary(name, op))
			}
		case e.Activity == agents.Card:
			op.kind = authorize
			c := op
			c.kind = clear
			groups[e.Activity] = append(groups[e.Activity], ordinary(name, op, c))
		case e.Activity == agents.Payroll || e.Activity == agents.PeerToPeer || e.Activity == agents.Rent ||
			e.Activity == agents.Utility || e.Activity == agents.Subscription || e.Activity == agents.CashSweep:
			op.kind = transfer
			groups[e.Activity] = append(groups[e.Activity], ordinary(name, op))
		}
	}
	keys := make([]agents.Activity, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([][]scenario, 0, len(keys))
	for _, k := range keys {
		out = append(out, groups[k])
	}
	return out
}

func fromChaos(r agents.ChaosResult) ([][]scenario, error) {
	loc, err := simclock.London.Location()
	if err != nil {
		return nil, err
	}
	tx := make(map[domain.TransactionID]domain.Transaction)
	bookings := make(map[domain.TransactionID]domain.JournalEntry)
	auth := make(map[domain.TransactionID]time.Time)
	classes := make(map[domain.AccountID]domain.AccountClass)
	for _, v := range r.Snapshot.Accounts {
		classes[v.Account.ID] = v.Account.Class
	}
	for _, v := range r.Snapshot.Transactions {
		tx[v.ID] = v
	}
	for _, v := range r.Snapshot.TransactionEvents {
		if v.To == domain.Authorized {
			auth[v.Transaction] = v.RecordedAt
		}
	}
	groups := make([][]scenario, 9)
	recent := make(map[domain.AccountID]operation)
	walls := make(map[string]operation)
	for _, j := range r.Snapshot.Journals {
		t := tx[j.Transaction]
		if t.Debtor != chaos.Payer && t.Debtor != chaos.Institution {
			continue
		}
		at := j.RecordedAt.In(loc)
		op := operation{key: uint64(t.ID), amount: t.Principal.Minor, source: t.Principal.Currency, target: t.Principal.Currency, at: at}
		name := fmt.Sprintf("chaos transaction %d", t.ID)
		switch j.Kind {
		case domain.Booking:
			bookings[t.ID] = j
			if t.Debtor == chaos.Institution {
				if at.Month() == 2 && at.Day() == 29 && classes[j.Postings[1].Account] == domain.Revenue {
					op.kind = accrue
					groups[0] = append(groups[0], ordinary("leap accrual "+name, op))
				}
				continue
			}
			if t.Principal.Currency == "USD" {
				op.kind = transfer
				groups[1] = append(groups[1], ordinary("same-key retry "+name, op, op))
			}
			if at.Nanosecond() == 999999999 && at.Hour() == 23 && at.Minute() == 59 && at.Second() == 59 {
				op.kind = timestamp
				groups[2] = append(groups[2], ordinary("midnight cutoff "+name, op))
			}
			if at.Hour() == 1 && at.Minute() == 30 {
				op.kind = timestamp
				wall := at.Format("2006-01-02T15:04:05")
				prior, ok := walls[wall]
				if !ok {
					walls[wall] = op
				} else if !prior.at.Equal(op.at) {
					groups[3] = append(groups[3], ordinary("DST fold pair "+name, prior, op))
				}
			}
			account := j.Postings[0].Account
			earlier, exists := recent[account]
			remember := op
			remember.kind = transfer
			recent[account] = remember
			if start := auth[t.ID]; !start.IsZero() && j.RecordedAt.Sub(start) >= 23*time.Hour {
				op.kind = clear
				a := op
				a.kind = authorize
				groups[4] = append(groups[4], ordinary("clearing-before-auth "+name, op, a, op))
				if exists && !earlier.at.Before(start) {
					groups[8] = append(groups[8], ordinary("interleaved settle "+name, a, earlier, op))
				}
			}
		case domain.Adjustment:
			if t.State != domain.Reversed || j.ID == 0 {
				continue
			}
			original := bookings[t.ID]
			if j.Original != original.ID {
				continue
			}
			// The first adjustment is smaller than the original payment and is
			// followed in the actual scenario by an over-cap attempt.
			if len(j.Postings) > 0 && j.Postings[0].Amount.Minor == t.Principal.Minor/2 && original.ID != 0 {
				if j.Postings[0].Amount.Minor == 0 {
					continue
				}
				initial := op
				initial.kind = transfer
				part := op
				part.kind, part.amount = refund, j.Postings[0].Amount.Minor
				over := part
				over.amount = t.Principal.Minor - part.amount + 1
				groups[5] = append(groups[5], ordinary("refund cap "+name, initial, part, over))
				delete(bookings, t.ID)
			}
		case domain.Reversal:
			initial := op
			initial.kind = transfer
			undo := op
			undo.kind = reverse
			if t.Principal.Minor == 500 {
				extra := initial
				extra.key++
				extra.amount = 1
				groups[6] = append(groups[6], scenario{"overdraft edge " + name, []operation{initial, extra, undo}, 0, -500})
			} else {
				a := op
				a.kind = authorize
				stop := op
				stop.kind = freeze
				c := op
				c.kind = clear
				groups[7] = append(groups[7], ordinary("frozen clearing "+name, a, stop, c), ordinary("reversal "+name, initial, undo))
			}
		}
	}
	for i, g := range groups {
		if len(g) == 0 {
			return nil, fmt.Errorf("aggressive trace omitted scenario group %d", i)
		}
	}
	return groups, nil
}
