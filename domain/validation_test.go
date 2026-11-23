package domain

import (
	"math"
	"testing"
	"time"
)

func TestJournalBalancePartitions(t *testing.T) {
	now := time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC)
	catalog := CurrencyCatalog{"GBP": {"GBP", 2}, "USD": {"USD", 2}}
	accounts := map[AccountID]Account{
		1: {Book: 1, ID: 1, Class: Asset, Purpose: General, Currency: "GBP", OpenedAt: now},
		2: {Book: 1, ID: 2, Class: Liability, Purpose: General, Currency: "GBP", OpenedAt: now},
		3: {Book: 1, ID: 3, Class: Liability, Purpose: General, Currency: "USD", OpenedAt: now},
	}
	entry := JournalEntry{Book: 1, ID: 1, Transaction: 1, Kind: Booking, EffectiveAt: now,
		RecordedAt: now, CommitSequence: 1, Postings: []Posting{
			{1, Debit, Money{"GBP", math.MaxInt64}}, {1, Debit, Money{"GBP", math.MaxInt64}},
			{2, Credit, Money{"GBP", math.MaxInt64}}, {2, Credit, Money{"GBP", math.MaxInt64}},
		}}
	if err := entry.Validate(catalog, accounts); err != nil {
		t.Fatal(err)
	}
	entry.Postings[3].Account = 3
	entry.Postings[3].Amount.Currency = "USD"
	if err := entry.Validate(catalog, accounts); err == nil {
		t.Fatal("accepted cross-currency cancellation")
	}
}

func TestFXMinorUnitRounding(t *testing.T) {
	now := time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC)
	catalog := CurrencyCatalog{"GBP": {"GBP", 2}, "JPY": {"JPY", 0}}
	rate := FXRate{1, "GBP", "JPY", 100, 2, now, now.Add(time.Hour)}
	for _, tc := range []struct{ input, want int64 }{{3, 2}, {5, 2}, {7, 4}} {
		got, err := rate.Convert(catalog, Money{"GBP", tc.input}, now)
		if err != nil || got != (Money{"JPY", tc.want}) {
			t.Fatalf("source %d: got %v, error %v; want %d JPY", tc.input, got, err, tc.want)
		}
	}
	if _, err := rate.Convert(catalog, Money{"GBP", 1}, now); err == nil {
		t.Fatal("accepted quote that rounds to zero")
	}
	if _, err := rate.Convert(catalog, Money{"GBP", 3}, rate.ValidUntil); err == nil {
		t.Fatal("accepted expired quote")
	}
}

func TestLifecycleBoundaries(t *testing.T) {
	if Closed.CanTransitionTo(Active) || Frozen.CanTransitionTo(Closed) ||
		Pending.CanTransitionTo(Cleared) || Reversed.CanTransitionTo(Disputed) {
		t.Fatal("accepted a prohibited transition")
	}
	if !Dormant.CanTransitionTo(Frozen) || !Authorized.CanTransitionTo(Cleared) ||
		!Disputed.CanTransitionTo(Cleared) || !Disputed.CanTransitionTo(Reversed) {
		t.Fatal("rejected a permitted transition")
	}
}
