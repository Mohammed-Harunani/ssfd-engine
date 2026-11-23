// Command ssfd-demo runs the seeded SSFD agent simulation.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"ssfd-engine/agents"
	"ssfd-engine/chaos"
	"ssfd-engine/domain"
	"ssfd-engine/ledger"
	"ssfd-engine/prng"
	"ssfd-engine/serializers"
	"ssfd-engine/simclock"
)

type fixture struct {
	Version         string
	Seed            uint64
	RandomAlgorithm string
	Calendar        simclock.Market
	End             time.Time
	Snapshot        ledger.Snapshot
	TrialBalance    []ledger.TrialRow
}

func main() {
	log.SetFlags(0)
	seed := flag.Uint64("seed", 42, "unsigned numeric simulation seed")
	start := flag.Int("start-year", 2024, "first complete calendar year")
	years := flag.Int("years", 1, "number of complete calendar years")
	full := flag.Bool("snapshot", false, "include events, actor metadata, loan positions and ledger snapshot")
	previous := flag.Bool("phase2-fixture", false, "emit the original Phase 2 regression fixture")
	faults := flag.String("chaos-profile", "", "enable deterministic chaos: standard or aggressive")
	exportDir := flag.String("export-dir", "", "write all protocol exports to a new directory")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatal("unexpected positional arguments")
	}
	var result any
	var export agents.Result
	var metadata *serializers.FaultMetadata
	var err error
	if *previous {
		if *faults != "" || *exportDir != "" {
			log.Fatal("-phase2-fixture cannot be combined with -chaos-profile or -export-dir")
		}
		result, err = run(*seed)
	} else if *faults != "" {
		var output agents.ChaosResult
		var spec chaos.Config
		spec, err = chaos.Profile(*faults, *seed)
		if err == nil {
			output, err = agents.RunWithChaos(agents.Config{Seed: *seed, StartYear: *start, Years: *years}, spec)
			export = output.Result
			metadata = &serializers.FaultMetadata{Version: output.ChaosVersion, Seed: output.ChaosSeed, Stats: output.Chaos}
			if *full {
				result = output
			} else {
				result = summarizeChaos(output)
			}
		}
	} else {
		var output agents.Result
		output, err = agents.Run(agents.Config{Seed: *seed, StartYear: *start, Years: *years})
		export = output
		result = summarize(output)
		if *full {
			result = output
		}
	}
	if err != nil {
		log.Fatal(err)
	}
	if *exportDir != "" {
		if _, err := serializers.ExportRun(*exportDir, export, metadata, serializers.SimulationCatalog()); err != nil {
			log.Fatal(err)
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		log.Fatal(err)
	}
}

func run(seed uint64) (fixture, error) {
	const market = simclock.EnglandWales
	calendar, err := simclock.NewCalendar(market)
	if err != nil {
		return fixture{}, err
	}
	zone, err := simclock.London.Location()
	if err != nil {
		return fixture{}, err
	}
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock, err := simclock.New(start, start.AddDate(5, 0, 0))
	if err != nil {
		return fixture{}, err
	}
	s := prng.New(seed)
	book, err := ledger.New(ledger.Config{
		Book:              1,
		Currencies:        domain.CurrencyCatalog{"GBP": {Code: "GBP", MinorDigits: 2}},
		Parties:           []domain.Counterparty{{ID: 1, Kind: domain.Institution}, {ID: 2, Kind: domain.Individual}},
		EarliestEffective: start,
	})
	if err != nil {
		return fixture{}, err
	}
	var request domain.RequestID
	apply := func(c ledger.Command) error {
		request++
		c.Key = domain.RequestKey{Book: 1, Source: 1, Request: request}
		c.RecordedAt = clock.Now()
		_, err := book.Apply(c)
		if err != nil {
			return fmt.Errorf("fixture command %d: %w", request, err)
		}
		return nil
	}
	for _, a := range []ledger.OpenAccount{
		{ID: 1, Class: domain.Asset, Purpose: domain.General, Currency: "GBP"},
		{ID: 2, Owner: 2, Class: domain.Liability, Purpose: domain.General, Currency: "GBP", Policy: ledger.AccountPolicy{EnforceFloor: true}},
	} {
		if err := apply(ledger.Command{OpenAccount: &a}); err != nil {
			return fixture{}, err
		}
	}
	for year := 2024; year <= 2028; year++ {
		d, err := calendar.Following(simclock.Date{Year: year, Month: time.January, Day: 1})
		if err != nil {
			return fixture{}, err
		}
		at, err := (simclock.Civil{Year: d.Year, Month: d.Month, Day: d.Day, Hour: 9}).Resolve(zone, simclock.RejectAmbiguous)
		if err != nil {
			return fixture{}, err
		}
		if err := clock.AdvanceTo(at); err != nil {
			return fixture{}, err
		}
		n, err := s.Below(9000)
		if err != nil {
			return fixture{}, err
		}
		principal := domain.Money{Currency: "GBP", Minor: int64(n) + 1000}
		id := domain.TransactionID(year - 2023)
		actions := []ledger.Command{
			{OpenTransaction: &ledger.OpenTransaction{ID: id, Kind: domain.SingleCurrency, Debtor: 1, Creditor: 2, Principal: principal}},
			{ChangeTransaction: &ledger.ChangeTransaction{ID: id, From: domain.Pending, To: domain.Authorized}},
			{ChangeTransaction: &ledger.ChangeTransaction{ID: id, From: domain.Authorized, To: domain.Cleared, Journal: &ledger.Journal{
				ID: domain.EntryID(id), Kind: domain.Booking, EffectiveAt: at,
				Postings: []domain.Posting{{Account: 1, Side: domain.Debit, Amount: principal}, {Account: 2, Side: domain.Credit, Amount: principal}},
			}}},
		}
		for _, c := range actions {
			if err := apply(c); err != nil {
				return fixture{}, err
			}
		}
	}
	trial, err := book.Audit()
	if err != nil {
		return fixture{}, err
	}
	return fixture{"ssfd-phase-2-demo-v1", seed, prng.Version, market, clock.Now(), book.Snapshot(), trial}, nil
}
