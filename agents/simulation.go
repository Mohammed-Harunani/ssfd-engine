package agents

import (
	"fmt"
	"strconv"
	"time"

	"ssfd-engine/chaos"
	"ssfd-engine/domain"
	"ssfd-engine/ledger"
	"ssfd-engine/prng"
	"ssfd-engine/simclock"
)

type date = simclock.Date

const Version = "ssfd-agents-v1"

type Config struct {
	Seed      uint64
	StartYear int
	Years     int
}

type Activity string

const (
	OpeningFunds    Activity = "opening_funds"
	ExternalSales   Activity = "external_sales"
	LoanOrigination Activity = "loan_origination"
	Payroll         Activity = "payroll"
	Rent            Activity = "rent_direct_debit"
	Mortgage        Activity = "mortgage_direct_debit"
	BusinessLoan    Activity = "business_loan_direct_debit"
	LoanAllocation  Activity = "loan_allocation"
	Utility         Activity = "utility_direct_debit"
	Subscription    Activity = "subscription_direct_debit"
	CashSweep       Activity = "cash_sweep"
	Card            Activity = "merchant_card"
	PeerToPeer      Activity = "p2p"
	CrossBorder     Activity = "cross_border_fx"
)

type Event struct {
	Activity     Activity
	At           time.Time
	Transaction  domain.TransactionID
	Entry        domain.EntryID
	Outcome      domain.TransactionState
	Debtor       domain.CounterpartyID
	Creditor     domain.CounterpartyID
	Principal    domain.Money
	Quote        *domain.FXQuote
	LoanPosition uint32
	LoanPayment  *Installment
}

type LoanView struct {
	Borrower    domain.CounterpartyID
	Asset       domain.AccountID
	Terms       LoanTerms
	Outstanding int64
	Paid        uint32
}

type Result struct {
	Version             string
	Seed                uint64
	Algorithm           string
	Calendar            simclock.Market
	Start, EndExclusive date
	Actors              []Actor
	Events              []Event
	Loans               []LoanView
	Snapshot            ledger.Snapshot
	TrialBalance        []ledger.TrialRow
}

type loan struct {
	borrower    Actor
	asset       domain.AccountID
	terms       LoanTerms
	outstanding int64
	paid        uint32
	last        date
	kind        Activity
}

type engine struct {
	p           population
	book        *ledger.Ledger
	clock       *simclock.Clock
	calendar    *simclock.Calendar
	zone        *time.Location
	rate        domain.FXRate
	streams     []prng.RNG
	states      []spendState
	request     domain.RequestID
	transaction domain.TransactionID
	entry       domain.EntryID
	events      []Event
	injection   *chaos.Injector
}

// Run simulates a fresh synthetic book for complete calendar years. The input
// horizon is bounded by the Phase 2 England and Wales banking calendar.
func Run(cfg Config) (Result, error) {
	out, err := run(cfg, nil)
	return out.Result, err
}

// ChaosResult adds per-fault counters to the normal simulation result.
type ChaosResult struct {
	Result
	ChaosVersion string
	ChaosSeed    uint64
	Chaos        []chaos.FaultStats
}

// RunWithChaos interleaves controlled fault scenarios with the behavioral
// schedule. Both streams commit into the same book.
func RunWithChaos(cfg Config, faults chaos.Config) (ChaosResult, error) {
	return run(cfg, &faults)
}

func leapInterest(start, end date) (int64, error) {
	t := LoanTerms{Principal: 365_000, AnnualBasisPoints: 500, Installments: 12, Convention: Actual365Fixed}
	p, err := t.Payment(t.Principal, 1, start, end)
	return p.Interest, err
}

func run(cfg Config, faults *chaos.Config) (ChaosResult, error) {
	if cfg.StartYear < 2019 || cfg.StartYear > 2028 || cfg.Years < 1 || cfg.Years > 2029-cfg.StartYear {
		return ChaosResult{}, fmt.Errorf("simulation must cover complete calendar years within 2019–2028")
	}
	start := date{Year: cfg.StartYear, Month: time.January, Day: 1}
	end := date{Year: cfg.StartYear + cfg.Years, Month: time.January, Day: 1}
	var parties []domain.Counterparty
	if faults != nil {
		parties = chaos.Counterparties()
	}
	e, err := newEngine(cfg.Seed, start, end, parties...)
	if err != nil {
		return ChaosResult{}, err
	}
	if err := e.initialize(); err != nil {
		return ChaosResult{}, err
	}
	if faults != nil {
		e.injection, err = chaos.New(*faults, 1, e.book, e.clock, leapInterest)
		if err != nil {
			return ChaosResult{}, err
		}
	}
	if err := e.simulate(start, end); err != nil {
		return ChaosResult{}, err
	}
	audit, err := e.book.Audit()
	if err != nil {
		return ChaosResult{}, err
	}
	result := Result{Version: Version, Seed: cfg.Seed, Algorithm: prng.Version, Calendar: simclock.EnglandWales, Start: start,
		EndExclusive: end, Actors: e.p.actors, Events: e.events, Snapshot: e.book.Snapshot(), TrialBalance: audit}
	for _, l := range e.p.loans {
		view, ok := e.book.Account(l.asset)
		if !ok || view.DebitPositive != strconv.FormatInt(l.outstanding, 10) {
			return ChaosResult{}, fmt.Errorf("loan subledger divergence at account %d", l.asset)
		}
		result.Loans = append(result.Loans, LoanView{l.borrower.ID, l.asset, l.terms, l.outstanding, l.paid})
	}
	collection, ok := e.book.Account(e.p.loanCollection)
	if !ok || collection.DebitPositive != "0" {
		return ChaosResult{}, fmt.Errorf("loan collection account failed to reconcile")
	}
	out := ChaosResult{Result: result}
	if e.injection != nil {
		if err := e.injection.AssertDrained(); err != nil {
			return ChaosResult{}, err
		}
		out.ChaosVersion, out.ChaosSeed = chaos.Version, faults.Seed
		out.Chaos = e.injection.Stats()
	}
	return out, nil
}

func newEngine(seed uint64, start, end date, extra ...domain.Counterparty) (*engine, error) {
	e := &engine{p: buildPopulation(start)}
	if err := e.p.verify(); err != nil {
		return nil, err
	}
	var err error
	e.calendar, err = simclock.NewCalendar(simclock.EnglandWales)
	if err != nil {
		return nil, err
	}
	e.zone, err = simclock.London.Location()
	if err != nil {
		return nil, err
	}
	first, err := e.resolve(start, 0)
	if err != nil {
		return nil, err
	}
	last, err := e.resolve(end, 0)
	if err != nil {
		return nil, err
	}
	e.clock, err = simclock.New(first, last)
	if err != nil {
		return nil, err
	}
	e.rate = domain.FXRate{ID: 1, Base: "GBP", Quote: "USD", Numerator: 5, Denominator: 4, ValidFrom: first, ValidUntil: last}
	e.book, err = ledger.New(ledger.Config{Book: 1, Currencies: currencies(), Parties: append(e.p.parties, extra...),
		Rates: []domain.FXRate{e.rate}, EarliestEffective: first})
	if err != nil {
		return nil, err
	}
	root := prng.New(seed)
	for range e.p.retail {
		e.streams = append(e.streams, *prng.New(root.Uint64()))
		e.states = append(e.states, quiet)
	}
	return e, nil
}

func currencies() domain.CurrencyCatalog {
	return domain.CurrencyCatalog{"GBP": {Code: "GBP", MinorDigits: 2}, "USD": {Code: "USD", MinorDigits: 2}}
}

func (e *engine) resolve(d date, hour int) (time.Time, error) {
	return (simclock.Civil{Year: d.Year, Month: d.Month, Day: d.Day, Hour: hour}).Resolve(e.zone, simclock.RejectAmbiguous)
}

func (e *engine) at(d date, hour int) error {
	t, err := e.resolve(d, hour)
	if err != nil {
		return err
	}
	return e.clock.AdvanceTo(t)
}

func (e *engine) apply(c ledger.Command) (ledger.Receipt, error) {
	e.request++
	c.Key = domain.RequestKey{Book: 1, Source: 1, Request: e.request}
	c.RecordedAt = e.clock.Now()
	r, err := e.book.Apply(c)
	if err != nil {
		return ledger.Receipt{}, fmt.Errorf("request %d at %s: %w", e.request, c.RecordedAt, err)
	}
	return r, nil
}

func (e *engine) initialize() error {
	for _, account := range e.p.accounts {
		if _, err := e.apply(ledger.Command{OpenAccount: &account}); err != nil {
			return err
		}
	}
	for _, actor := range e.p.actors {
		opening := int64(450_000)
		if actor.Archetype == SmallBusiness {
			opening = 25_000_000
		}
		if err := e.internal(OpeningFunds, e.p.cash, actor.Accounts[0].ID, actor.ID, opening); err != nil {
			return err
		}
	}
	for _, l := range e.p.loans {
		recipient := l.borrower
		if l.kind == Mortgage {
			recipient = e.p.landlord // Seller settlement. The borrower owes the mortgage.
		}
		if err := e.internal(LoanOrigination, l.asset, recipient.Accounts[0].ID, recipient.ID, l.outstanding); err != nil {
			return err
		}
	}
	return nil
}
