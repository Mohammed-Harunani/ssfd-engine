// Package domain defines the canonical, protocol-independent SSFD ledger schema.
package domain

import "time"

// Identifiers are deterministic synthetic surrogates. Zero means absent.
type (
	BookID         uint64
	AccountID      uint64
	EntryID        uint64
	TransactionID  uint64
	CounterpartyID uint64
	RateID         uint64
	SourceID       uint64
	RequestID      uint64
)

// RequestKey is scoped by book and submitting synthetic source.
type RequestKey struct {
	Book    BookID
	Source  SourceID
	Request RequestID
}

// Currency is an ISO 4217 alphabetic code. A run admits only codes in its
// pinned catalog. A code's minor-unit scale is constant throughout the run.
type Currency string

type CurrencySpec struct {
	Code        Currency
	MinorDigits uint8
}

type CurrencyCatalog map[Currency]CurrencySpec

// Money carries a signed minor-unit count. Posted magnitudes must be positive.
type Money struct {
	Currency Currency
	Minor    int64
}

type AccountClass uint8

const (
	Asset AccountClass = iota + 1
	Liability
	Equity
	Revenue
	Expense
)

type AccountPurpose uint8

const (
	General AccountPurpose = iota + 1
	FXPosition
)

type Account struct {
	Book     BookID
	ID       AccountID
	Owner    CounterpartyID
	Class    AccountClass
	Purpose  AccountPurpose
	Currency Currency
	OpenedAt time.Time
	State    AccountState
}

type PartyKind uint8

const (
	Individual PartyKind = iota + 1
	Business
	Institution
)

// Counterparty contains exclusively synthetic categorical fields.
type Counterparty struct {
	ID   CounterpartyID
	Kind PartyKind
}

type Side uint8

const (
	Debit Side = iota + 1
	Credit
)

// Posting identity is (entry ID, one-based position in Postings).
type Posting struct {
	Account AccountID
	Side    Side
	Amount  Money
}

type EntryKind uint8

const (
	Booking EntryKind = iota + 1
	Reversal
	Adjustment
)

// JournalEntry is an immutable fact once committed. EffectiveAt selects the
// accounting period. CommitSequence selects the replay and knowledge order.
type JournalEntry struct {
	Book           BookID
	ID             EntryID
	Transaction    TransactionID
	Kind           EntryKind
	Original       EntryID
	EffectiveAt    time.Time
	RecordedAt     time.Time
	CommitSequence uint64
	Postings       []Posting
}

type IntentKind uint8

const (
	SingleCurrency IntentKind = iota + 1
	Exchange
)

// Transaction is the current workflow projection. Its ordered state history
// is represented by TransactionEvent values.
type Transaction struct {
	Book      BookID
	ID        TransactionID
	Request   RequestKey
	Kind      IntentKind
	State     TransactionState
	Debtor    CounterpartyID
	Creditor  CounterpartyID
	Principal Money
	Quote     *FXQuote
	CreatedAt time.Time
}

type FXQuote struct {
	Rate   RateID
	Target Money
}

// FXRate gives an exact ratio of quote major units per base major unit.
// Validity is the half-open interval [ValidFrom, ValidUntil).
type FXRate struct {
	ID          RateID
	Base        Currency
	Quote       Currency
	Numerator   uint64
	Denominator uint64
	ValidFrom   time.Time
	ValidUntil  time.Time
}
