package domain

import (
	"fmt"
	"math/big"
	"time"
)

// Lookup checks membership in the caller's pinned ISO 4217 snapshot.
// The run loader is responsible for verifying the snapshot's provenance.
func (catalog CurrencyCatalog) Lookup(code Currency) (CurrencySpec, error) {
	spec, ok := catalog[code]
	if len(code) == 3 && ok && spec.Code == code && spec.MinorDigits <= 9 {
		for i := range code {
			if code[i] < 'A' || code[i] > 'Z' {
				return CurrencySpec{}, fmt.Errorf("invalid ISO currency syntax %q", code)
			}
		}
		return spec, nil
	}
	return CurrencySpec{}, fmt.Errorf("unsupported currency %q", code)
}

// Validate checks entry shape, account bindings and exact per-currency balance.
// The supplied accounts are the entry's book-scoped account directory.
func (e JournalEntry) Validate(catalog CurrencyCatalog, accounts map[AccountID]Account) error {
	if e.Book == 0 || e.ID == 0 || e.Transaction == 0 || e.CommitSequence == 0 {
		return fmt.Errorf("missing journal identity or sequence")
	}
	if e.EffectiveAt.IsZero() || e.RecordedAt.IsZero() || e.EffectiveAt.After(e.RecordedAt) {
		return fmt.Errorf("invalid journal timestamps")
	}
	switch e.Kind {
	case Booking:
		if e.Original != 0 {
			return fmt.Errorf("booking cannot reference an original entry")
		}
	case Reversal, Adjustment:
		if e.Original == 0 || e.Original == e.ID {
			return fmt.Errorf("invalid original entry reference")
		}
	default:
		return fmt.Errorf("invalid entry kind %d", e.Kind)
	}
	if len(e.Postings) < 2 {
		return fmt.Errorf("journal requires at least two postings")
	}

	sums := make(map[Currency]*big.Int)
	currencies := make([]Currency, 0)
	for i, posting := range e.Postings {
		account, ok := accounts[posting.Account]
		if !ok || posting.Account == 0 || account.ID != posting.Account ||
			account.Book != e.Book || account.Currency != posting.Amount.Currency ||
			account.Class < Asset || account.Class > Expense ||
			account.Purpose < General || account.Purpose > FXPosition ||
			account.OpenedAt.IsZero() || e.EffectiveAt.Before(account.OpenedAt) {
			return fmt.Errorf("posting %d has invalid account binding", i+1)
		}
		if posting.Amount.Minor <= 0 || (posting.Side != Debit && posting.Side != Credit) {
			return fmt.Errorf("posting %d has invalid amount or side", i+1)
		}
		code := posting.Amount.Currency
		if sums[code] == nil {
			if _, err := catalog.Lookup(code); err != nil {
				return err
			}
			sums[code] = new(big.Int)
			currencies = append(currencies, code)
		}
		var amount big.Int
		amount.SetInt64(posting.Amount.Minor)
		if posting.Side == Credit {
			amount.Neg(&amount)
		}
		sums[code].Add(sums[code], &amount)
	}
	for _, code := range currencies {
		if sums[code].Sign() != 0 {
			return fmt.Errorf("journal imbalance in %s: %s minor units", code, sums[code])
		}
	}
	return nil
}

// Convert applies the exact quote ratio and rounds the target minor units
// once, to nearest with ties to even. Sub-minor results that round to zero
// and results outside the positive int64 range are rejected.
func (r FXRate) Convert(catalog CurrencyCatalog, source Money, at time.Time) (Money, error) {
	if r.ID == 0 || r.Base == r.Quote || source.Currency != r.Base || source.Minor <= 0 ||
		r.Numerator == 0 || r.Denominator == 0 || r.ValidFrom.IsZero() || r.ValidUntil.IsZero() ||
		!r.ValidFrom.Before(r.ValidUntil) || at.Before(r.ValidFrom) || !at.Before(r.ValidUntil) {
		return Money{}, fmt.Errorf("invalid rate, source amount, or quote instant")
	}
	base, err := catalog.Lookup(r.Base)
	if err != nil {
		return Money{}, err
	}
	quote, err := catalog.Lookup(r.Quote)
	if err != nil {
		return Money{}, err
	}
	var factor big.Int
	numerator := big.NewInt(source.Minor)
	numerator.Mul(numerator, factor.SetUint64(r.Numerator))
	numerator.Mul(numerator, factor.Exp(big.NewInt(10), big.NewInt(int64(quote.MinorDigits)), nil))
	denominator := new(big.Int).SetUint64(r.Denominator)
	denominator.Mul(denominator, factor.Exp(big.NewInt(10), big.NewInt(int64(base.MinorDigits)), nil))

	var units, remainder big.Int
	units.QuoRem(numerator, denominator, &remainder)
	comparison := remainder.Lsh(&remainder, 1).Cmp(denominator)
	if comparison > 0 || (comparison == 0 && units.Bit(0) == 1) {
		units.Add(&units, big.NewInt(1))
	}
	if !units.IsInt64() || units.Sign() <= 0 {
		return Money{}, fmt.Errorf("rounded quote outside positive int64 range")
	}
	return Money{Currency: r.Quote, Minor: units.Int64()}, nil
}

// Validate checks the transaction projection and its fixed creation-time quote.
// The commit boundary resolves party IDs and enforces lifecycle and book state.
func (t Transaction) Validate(catalog CurrencyCatalog, rates map[RateID]FXRate) error {
	if t.Book == 0 || t.ID == 0 || t.Request.Book != t.Book ||
		t.Request.Source == 0 || t.Request.Request == 0 ||
		t.Debtor == 0 || t.Creditor == 0 || t.CreatedAt.IsZero() ||
		t.State < Pending || t.State > Declined || t.Principal.Minor <= 0 {
		return fmt.Errorf("invalid transaction identity, state, or principal")
	}
	if _, err := catalog.Lookup(t.Principal.Currency); err != nil {
		return err
	}
	switch t.Kind {
	case SingleCurrency:
		if t.Quote != nil {
			return fmt.Errorf("single-currency transaction has an FX quote")
		}
	case Exchange:
		if t.Quote == nil {
			return fmt.Errorf("missing exchange quote")
		}
		rate, ok := rates[t.Quote.Rate]
		if !ok || rate.ID != t.Quote.Rate {
			return fmt.Errorf("unknown exchange rate")
		}
		target, err := rate.Convert(catalog, t.Principal, t.CreatedAt)
		if err != nil {
			return err
		}
		if target != t.Quote.Target {
			return fmt.Errorf("target differs from exact rounded exchange quote")
		}
	default:
		return fmt.Errorf("invalid transaction kind %d", t.Kind)
	}
	return nil
}
