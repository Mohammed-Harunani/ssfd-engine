// Package agents generates deterministic synthetic activity through the SSFD ledger.
package agents

import (
	"fmt"
	"math/big"
	"time"

	"ssfd-engine/simclock"
)

type DayCount string

const (
	Actual365Fixed DayCount = "ACT/365F"
	Thirty360ISDA  DayCount = "30/360 ISDA"
)

func civilUTC(d simclock.Date) (time.Time, error) {
	t := time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
	if d.Year < 1 || d.Year > 9999 || simclock.DateAt(t, time.UTC) != d {
		return time.Time{}, fmt.Errorf("invalid date %v", d)
	}
	return t, nil
}

// Fraction returns the exact numerator and denominator for a civil-date interval.
func (c DayCount) Fraction(start, end simclock.Date) (int64, int64, error) {
	a, err := civilUTC(start)
	if err != nil {
		return 0, 0, err
	}
	b, err := civilUTC(end)
	if err != nil {
		return 0, 0, err
	}
	if b.Before(a) {
		return 0, 0, fmt.Errorf("accrual date regressed")
	}
	switch c {
	case Actual365Fixed:
		return (b.Unix() - a.Unix()) / 86400, 365, nil
	case Thirty360ISDA:
		d1, d2 := start.Day, end.Day
		if d1 == 31 {
			d1 = 30
		}
		if d1 == 30 && d2 == 31 {
			d2 = 30
		}
		days := 360*(end.Year-start.Year) + 30*int(end.Month-start.Month) + d2 - d1
		return int64(days), 360, nil
	default:
		return 0, 0, fmt.Errorf("unknown day count %q", c)
	}
}

// LoanTerms define equal-principal amortization with an annual rate in basis points.
type LoanTerms struct {
	Principal         int64
	AnnualBasisPoints uint32
	Installments      uint32
	Convention        DayCount
}

type Installment struct {
	Principal int64
	Interest  int64
	Total     int64
}

// Payment calculates one installment for the supplied outstanding principal.
// Interest rounds once to the nearest minor unit, ties to even. The final
// installment pays the remaining principal exactly.
func (t LoanTerms) Payment(outstanding int64, position uint32, start, end simclock.Date) (Installment, error) {
	if t.Principal <= 0 || t.Installments == 0 || t.Principal < int64(t.Installments) ||
		position == 0 || position > t.Installments {
		return Installment{}, fmt.Errorf("invalid loan terms or installment position")
	}
	regular := t.Principal / int64(t.Installments)
	expected := t.Principal - int64(position-1)*regular
	if outstanding != expected {
		return Installment{}, fmt.Errorf("outstanding principal does not match installment position")
	}
	n, d, err := t.Convention.Fraction(start, end)
	if err != nil {
		return Installment{}, err
	}
	amount := big.NewInt(outstanding)
	amount.Mul(amount, big.NewInt(int64(t.AnnualBasisPoints)))
	amount.Mul(amount, big.NewInt(n))
	denom := big.NewInt(d * 10000)
	var interest, rem big.Int
	interest.QuoRem(amount, denom, &rem)
	cmp := rem.Lsh(&rem, 1).Cmp(denom)
	if cmp > 0 || (cmp == 0 && interest.Bit(0) == 1) {
		interest.Add(&interest, big.NewInt(1))
	}
	if position == t.Installments {
		regular = outstanding
	}
	total := new(big.Int).Add(&interest, big.NewInt(regular))
	if !total.IsInt64() {
		return Installment{}, fmt.Errorf("installment exceeds minor-unit range")
	}
	return Installment{regular, interest.Int64(), total.Int64()}, nil
}
