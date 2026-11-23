package agents

import (
	"fmt"
	"time"

	"ssfd-engine/simclock"
)

type monthlyRule struct {
	day    int
	action func(date) error
}

func (e *engine) monthlyRules() []monthlyRule {
	return []monthlyRule{
		{1, e.collectRent},
		{5, e.collectBills},
		{22, e.runPayroll},
		{24, func(d date) error { return e.repay(&e.p.loans[0], d) }},
		{25, func(d date) error { return e.repay(&e.p.loans[1], d) }},
		{26, e.sweepAccounts},
	}
}

// simulate enumerates local civil days. Monthly actions sharing a settlement
// date are executed in nominal month and rule order, before daily activity.
func (e *engine) simulate(start, end date) error {
	plan := make(map[date][]monthlyRule)
	for year := start.Year; year < end.Year; year++ {
		for month := time.January; month <= time.December; month++ {
			for _, rule := range e.monthlyRules() {
				day, err := e.calendar.Following(date{Year: year, Month: month, Day: rule.day})
				if err != nil {
					return err
				}
				plan[day] = append(plan[day], rule)
			}
		}
	}
	a, err := civilUTC(start)
	if err != nil {
		return err
	}
	b, err := civilUTC(end)
	if err != nil {
		return err
	}
	for t := a; t.Before(b); t = t.AddDate(0, 0, 1) {
		day := simclock.DateAt(t, time.UTC)
		if e.injection != nil {
			if err := e.injection.Day(day, t.AddDate(0, 0, 1).Before(b)); err != nil {
				return fmt.Errorf("chaos injection on %v: %w", day, err)
			}
		}
		if err := e.at(day, 9); err != nil {
			return err
		}
		for _, rule := range plan[day] {
			if err := rule.action(day); err != nil {
				return fmt.Errorf("monthly rule for day %d at %v: %w", rule.day, day, err)
			}
		}
		if err := e.at(day, 18); err != nil {
			return err
		}
		if err := e.daily(); err != nil {
			return err
		}
	}
	return e.at(end, 0)
}

func (e *engine) collectRent(_ date) error {
	for i, renter := range e.p.retail[:4] {
		if _, err := e.transfer(Rent, renter, e.p.landlord, renter.Accounts[0], e.p.landlord.Accounts[0], 85_000+int64(i)*10_000); err != nil {
			return err
		}
	}
	return nil
}

func (e *engine) collectBills(_ date) error {
	for i, r := range e.p.retail {
		if _, err := e.transfer(Utility, r, e.p.utility, r.Accounts[0], e.p.utility.Accounts[0], 9_950+int64(i)*430); err != nil {
			return err
		}
		if _, err := e.transfer(Subscription, r, e.p.subscription, r.Accounts[0], e.p.subscription.Accounts[0], 1_299); err != nil {
			return err
		}
	}
	return nil
}

func (e *engine) runPayroll(_ date) error {
	for i, r := range e.p.retail {
		employer := e.p.business[i%2]
		amount := int64(255_000 + i*22_500)
		// External turnover from the wider synthetic economy funds payroll.
		// The transfer to staff then debits the employer's operating account.
		if err := e.internal(ExternalSales, e.p.cash, employer.Accounts[0].ID, employer.ID, amount+60_000); err != nil {
			return err
		}
		cleared, err := e.transfer(Payroll, employer, r, employer.Accounts[0], r.Accounts[0], amount)
		if err != nil {
			return err
		}
		if !cleared {
			return fmt.Errorf("funded payroll declined for actor %d", r.ID)
		}
	}
	return nil
}

func (e *engine) sweepAccounts(_ date) error {
	for _, a := range e.p.actors {
		n, err := e.available(a.Accounts[0].ID)
		if err != nil {
			return err
		}
		// Retain a working balance and move 10% of the excess each month.
		const operatingBuffer = 350_000
		amount := (n - operatingBuffer) / 10
		if amount > 0 {
			if _, err := e.transfer(CashSweep, a, a, a.Accounts[0], a.Accounts[1], amount); err != nil {
				return err
			}
		}
	}
	return nil
}
