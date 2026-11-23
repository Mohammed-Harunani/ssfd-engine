// Package eval measures deterministic SSFD workloads and controlled ledger mutations.
package eval

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"runtime"
	"slices"
	"sort"
	"time"

	"ssfd-engine/agents"
	"ssfd-engine/domain"
	"ssfd-engine/ledger"
	"ssfd-engine/prng"
	"ssfd-engine/serializers"
)

type Workload string

const (
	Year   Workload = "year"
	Decade Workload = "decade"
	Stress Workload = "stress"
)

type BenchmarkConfig struct {
	Workloads      []Workload
	Seed           uint64
	StressJournals int
	Repeats        int
	GenerationOnly bool
}

type Stage struct {
	Name                                                  string
	Nanoseconds                                           int64
	BaselineHeapBytes, PeakHeapBytes, TotalAllocatedBytes uint64
	OutputBytes                                           int
	SHA256                                                string
}

type Measurement struct {
	Workload                             Workload
	Repeat                               int
	Transactions, Journals, Postings     int
	Generation                           Stage
	Audit                                Stage
	Serialization                        []Stage
	JournalsPerSecond, PostingsPerSecond float64
	TrialBalance                         []ledger.TrialRow
	CanonicalSHA256                      string
}

type BenchmarkReport struct {
	GoVersion, OS, Arch     string
	GOMAXPROCS, LogicalCPUs int
	Config                  BenchmarkConfig
	Deterministic           bool
	Runs                    []Measurement
}

type fixture struct {
	Snapshot ledger.Snapshot
	Trial    []ledger.TrialRow
}

// Benchmarks executes repetitions sequentially and checks every output digest.
func Benchmarks(cfg BenchmarkConfig) (BenchmarkReport, error) {
	if cfg.Repeats < 2 || len(cfg.Workloads) == 0 {
		return BenchmarkReport{}, fmt.Errorf("at least two repetitions and one workload required")
	}
	report := BenchmarkReport{GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		GOMAXPROCS: runtime.GOMAXPROCS(0), LogicalCPUs: runtime.NumCPU(), Config: cfg}
	for _, w := range cfg.Workloads {
		var expected []string
		for repeat := 1; repeat <= cfg.Repeats; repeat++ {
			result, digests, err := benchmarkOne(cfg, w, repeat)
			if err != nil {
				return report, fmt.Errorf("%s repetition %d: %w", w, repeat, err)
			}
			if repeat == 1 {
				expected = digests
			} else if !slices.Equal(expected, digests) {
				return report, fmt.Errorf("%s repetition %d failed deterministic digest comparison", w, repeat)
			}
			report.Runs = append(report.Runs, result)
		}
	}
	report.Deterministic = true
	return report, nil
}

func benchmarkOne(cfg BenchmarkConfig, w Workload, repeat int) (Measurement, []string, error) {
	var f fixture
	var err error
	result := Measurement{Workload: w, Repeat: repeat}
	result.Generation, err = measure("generate", func() error { f, err = generate(w, cfg.Seed, cfg.StressJournals); return err })
	if err != nil {
		return result, nil, err
	}
	result.Transactions, result.Journals = len(f.Snapshot.Transactions), len(f.Snapshot.Journals)
	result.Audit, err = measure("audit", func() error {
		result.TrialBalance, result.Postings, err = independentAudit(f.Snapshot)
		if err == nil && !slices.Equal(f.Trial, result.TrialBalance) {
			return fmt.Errorf("independent audit differs from ledger audit")
		}
		return err
	})
	if err != nil {
		return result, nil, err
	}
	result.JournalsPerSecond = float64(result.Journals) * 1e9 / float64(result.Generation.Nanoseconds)
	result.PostingsPerSecond = float64(result.Postings) * 1e9 / float64(result.Generation.Nanoseconds)
	if cfg.GenerationOnly {
		result.CanonicalSHA256, err = canonicalDigest(f.Snapshot)
		return result, []string{result.CanonicalSHA256}, err
	}
	digests := make([]string, 0, 3)
	for _, name := range []string{"canonical-json", "camt", "flat-cp037"} {
		var raw []byte
		s, err := measure(name, func() error { raw, err = encode(name, f.Snapshot); return err })
		if err != nil {
			return result, nil, err
		}
		s.OutputBytes, s.SHA256 = len(raw), fmt.Sprintf("%x", sha256.Sum256(raw))
		result.Serialization = append(result.Serialization, s)
		digests = append(digests, s.SHA256)
	}
	return result, digests, nil
}

func measure(name string, fn func() error) (Stage, error) {
	runtime.GC()
	var start, end runtime.MemStats
	runtime.ReadMemStats(&start)
	stop := make(chan struct{})
	done := make(chan uint64, 1)
	go func() {
		peak := start.HeapAlloc
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				var s runtime.MemStats
				runtime.ReadMemStats(&s)
				peak = max(peak, s.HeapAlloc)
			case <-stop:
				done <- peak
				return
			}
		}
	}()
	clock := time.Now()
	err := fn()
	elapsed := time.Since(clock)
	runtime.ReadMemStats(&end)
	close(stop)
	peak := max(<-done, end.HeapAlloc)
	return Stage{Name: name, Nanoseconds: elapsed.Nanoseconds(), BaselineHeapBytes: start.HeapAlloc,
		PeakHeapBytes: peak, TotalAllocatedBytes: end.TotalAlloc - start.TotalAlloc}, err
}

func generate(w Workload, seed uint64, stress int) (fixture, error) {
	switch w {
	case Year, Decade:
		cfg := agents.Config{Seed: seed, StartYear: 2024, Years: 1}
		if w == Decade {
			cfg.StartYear, cfg.Years = 2019, 10
		}
		r, err := agents.Run(cfg)
		return fixture{r.Snapshot, r.TrialBalance}, err
	case Stress:
		return stressBook(seed, stress)
	default:
		return fixture{}, fmt.Errorf("unknown workload %q", w)
	}
}

func encode(name string, s ledger.Snapshot) ([]byte, error) {
	switch name {
	case "canonical-json":
		return json.Marshal(s)
	case "camt":
		return serializers.EncodeCamt(s, serializers.SimulationCatalog())
	case "flat-cp037":
		return serializers.EncodeFlat(s, serializers.SimulationCatalog(), serializers.CP037)
	default:
		return nil, fmt.Errorf("unknown encoder %q", name)
	}
}

// Hash a standard-library streamed array of records in generation-only mode.
func canonicalDigest(s ledger.Snapshot) (string, error) {
	h := sha256.New()
	enc := json.NewEncoder(h)
	if err := enc.Encode(struct {
		Book     domain.BookID
		Sequence uint64
	}{s.Book, s.Sequence}); err != nil {
		return "", err
	}
	if err := hashRecords(enc, s.Accounts); err != nil {
		return "", err
	}
	if err := hashRecords(enc, s.Transactions); err != nil {
		return "", err
	}
	if err := hashRecords(enc, s.Journals); err != nil {
		return "", err
	}
	if err := hashRecords(enc, s.AccountEvents); err != nil {
		return "", err
	}
	if err := hashRecords(enc, s.TransactionEvents); err != nil {
		return "", err
	}
	if err := hashRecords(enc, s.HoldEvents); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func hashRecords[T any](enc *json.Encoder, rs []T) error {
	if err := enc.Encode(len(rs)); err != nil {
		return err
	}
	for _, r := range rs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

func independentAudit(s ledger.Snapshot) ([]ledger.TrialRow, int, error) {
	type amount struct {
		currency domain.Currency
		n        big.Int
	}
	accounts := make(map[domain.AccountID]*amount, len(s.Accounts))
	for _, a := range s.Accounts {
		if a.Account.Book != s.Book || a.Account.ID == 0 || accounts[a.Account.ID] != nil {
			return nil, 0, fmt.Errorf("invalid account directory")
		}
		accounts[a.Account.ID] = &amount{currency: a.Account.Currency}
	}
	count := 0
	var seq uint64
	for _, j := range s.Journals {
		if j.Book != s.Book || j.CommitSequence <= seq || j.CommitSequence > s.Sequence || len(j.Postings) < 2 {
			return nil, count, fmt.Errorf("invalid journal identity or order")
		}
		seq = j.CommitSequence
		parts := make(map[domain.Currency]*big.Int)
		for _, p := range j.Postings {
			a := accounts[p.Account]
			if a == nil || a.currency != p.Amount.Currency || p.Amount.Minor <= 0 || (p.Side != domain.Debit && p.Side != domain.Credit) {
				return nil, count, fmt.Errorf("invalid posting in journal %d", j.ID)
			}
			n := big.NewInt(p.Amount.Minor)
			if p.Side == domain.Credit {
				n.Neg(n)
			}
			a.n.Add(&a.n, n)
			if parts[a.currency] == nil {
				parts[a.currency] = new(big.Int)
			}
			parts[a.currency].Add(parts[a.currency], n)
			count++
		}
		for c, n := range parts {
			if n.Sign() != 0 {
				return nil, count, fmt.Errorf("journal %d imbalance in %s", j.ID, c)
			}
		}
	}
	type sum struct{ debit, credit big.Int }
	totals := make(map[domain.Currency]*sum)
	for _, v := range s.Accounts {
		a := accounts[v.Account.ID]
		if a.n.String() != v.DebitPositive {
			return nil, count, fmt.Errorf("replay disagrees on account %d", v.Account.ID)
		}
		if totals[a.currency] == nil {
			totals[a.currency] = new(sum)
		}
		t := totals[a.currency]
		if a.n.Sign() < 0 {
			t.credit.Sub(&t.credit, &a.n)
		} else {
			t.debit.Add(&t.debit, &a.n)
		}
	}
	out := make([]ledger.TrialRow, 0, len(totals))
	for c, v := range totals {
		diff := new(big.Int).Sub(&v.debit, &v.credit)
		if diff.Sign() != 0 {
			return nil, count, fmt.Errorf("trial balance mismatch in %s", c)
		}
		out = append(out, ledger.TrialRow{Currency: c, Debits: v.debit.String(), Credits: v.credit.String(), Difference: "0"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out, count, nil
}

func stressBook(seed uint64, n int) (fixture, error) {
	if n < 2 || n > 10_000_000 {
		return fixture{}, fmt.Errorf("stress size must be between 2 and 10000000")
	}
	b, err := ledger.New(ledger.Config{Book: 1, Currencies: domain.CurrencyCatalog{"GBP": {Code: "GBP", MinorDigits: 2}, "USD": {Code: "USD", MinorDigits: 2}},
		Parties: []domain.Counterparty{{ID: 1, Kind: domain.Institution}, {ID: 2, Kind: domain.Business}}})
	if err != nil {
		return fixture{}, err
	}
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	at := start
	var request domain.RequestID
	apply := func(c ledger.Command) error {
		request++
		c.Key = domain.RequestKey{Book: 1, Source: 1, Request: request}
		c.RecordedAt = at
		_, err := b.Apply(c)
		return err
	}
	currencies := [2]domain.Currency{"GBP", "USD"}
	for i, code := range currencies {
		id := domain.AccountID(i*2 + 1)
		if err := apply(ledger.Command{OpenAccount: &ledger.OpenAccount{ID: id, Class: domain.Asset, Purpose: domain.General, Currency: code}}); err != nil {
			return fixture{}, err
		}
		if err := apply(ledger.Command{OpenAccount: &ledger.OpenAccount{ID: id + 1, Owner: 2, Class: domain.Liability, Purpose: domain.General, Currency: code}}); err != nil {
			return fixture{}, err
		}
	}
	rng := prng.New(seed)
	for i := range n {
		at = start.Add(time.Duration(i) * time.Second)
		id := domain.TransactionID(i + 1)
		account := domain.AccountID(i%2*2 + 1)
		draw, _ := rng.Below(100_000)
		money := domain.Money{Currency: currencies[i%2], Minor: 1 + int64(draw)}
		for _, c := range []ledger.Command{
			{OpenTransaction: &ledger.OpenTransaction{ID: id, Kind: domain.SingleCurrency, Debtor: 1, Creditor: 2, Principal: money}},
			{ChangeTransaction: &ledger.ChangeTransaction{ID: id, From: domain.Pending, To: domain.Authorized}},
			{ChangeTransaction: &ledger.ChangeTransaction{ID: id, From: domain.Authorized, To: domain.Cleared, Journal: &ledger.Journal{
				ID: domain.EntryID(id), Kind: domain.Booking, EffectiveAt: at, Postings: []domain.Posting{
					{Account: account, Side: domain.Debit, Amount: money}, {Account: account + 1, Side: domain.Credit, Amount: money},
				},
			}}},
		} {
			if err := apply(c); err != nil {
				return fixture{}, err
			}
		}
	}
	rows, err := b.Audit()
	if err != nil {
		return fixture{}, err
	}
	return fixture{b.Snapshot(), rows}, nil
}
