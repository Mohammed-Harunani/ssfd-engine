package simclock

import (
	"fmt"
	"strings"
	"time"
)

type Market string

const (
	EnglandWales     Market = "england-and-wales"
	Scotland         Market = "scotland"
	NorthernIreland  Market = "northern-ireland"
	USFederalReserve Market = "us-federal-reserve-banks"
)

// Date is a Gregorian civil date. Calendar operations use the market date.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

func DateAt(t time.Time, loc *time.Location) Date {
	y, m, d := t.In(loc).Date()
	return Date{y, m, d}
}

func (d Date) utc() time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}

// Calendar covers a bounded interval of complete years.
type Calendar struct {
	first, last int
	closed      map[Date]bool
	location    *time.Location
}

// NewCalendar builds a calendar from the embedded snapshot or versioned rules.
func NewCalendar(market Market) (*Calendar, error) {
	c := &Calendar{closed: make(map[Date]bool)}
	switch market {
	case EnglandWales, Scotland, NorthernIreland:
		c.first, c.last = 2019, 2028
		loc, err := London.Location()
		if err != nil {
			return nil, err
		}
		c.location = loc
		raw, err := data.ReadFile("data/" + string(market) + ".txt")
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Fields(string(raw)) {
			day, err := time.Parse(time.DateOnly, line)
			if err != nil {
				return nil, fmt.Errorf("invalid embedded holiday %q: %w", line, err)
			}
			c.closed[DateAt(day, time.UTC)] = true
		}
	case USFederalReserve:
		c.first, c.last = 2022, 2030
		loc, err := NewYork.Location()
		if err != nil {
			return nil, err
		}
		c.location = loc
		for y := c.first; y <= c.last; y++ {
			c.addFederalReserveYear(y)
		}
	default:
		return nil, fmt.Errorf("unknown holiday calendar %q", market)
	}
	return c, nil
}

func (c *Calendar) addFederalReserveYear(y int) {
	for _, d := range []Date{{y, 1, 1}, {y, 6, 19}, {y, 7, 4}, {y, 11, 11}, {y, 12, 25}} {
		t := d.utc()
		if t.Weekday() == time.Sunday {
			t = t.AddDate(0, 0, 1)
		}
		c.closed[DateAt(t, time.UTC)] = true
	}
	for _, rule := range []struct {
		month   time.Month
		weekday time.Weekday
		ordinal int
	}{
		{1, time.Monday, 3}, {2, time.Monday, 3}, {9, time.Monday, 1},
		{10, time.Monday, 2}, {11, time.Thursday, 4},
	} {
		t := (Date{y, rule.month, 1}).utc()
		offset := (int(rule.weekday)-int(t.Weekday())+7)%7 + (rule.ordinal-1)*7
		c.closed[DateAt(t.AddDate(0, 0, offset), time.UTC)] = true
	}
	lastMay := (Date{y, 5, 31}).utc()
	offset := (int(lastMay.Weekday()) - int(time.Monday) + 7) % 7
	c.closed[DateAt(lastMay.AddDate(0, 0, -offset), time.UTC)] = true
}

func (c *Calendar) DateAt(t time.Time) Date { return DateAt(t, c.location) }

func (c *Calendar) IsBusinessDay(d Date) (bool, error) {
	t := d.utc()
	if d.Year < c.first || d.Year > c.last || DateAt(t, time.UTC) != d {
		return false, fmt.Errorf("date outside calendar or invalid: %v", d)
	}
	return t.Weekday() != time.Saturday && t.Weekday() != time.Sunday && !c.closed[d], nil
}

// Following returns d for a business day, otherwise the next business day.
func (c *Calendar) Following(d Date) (Date, error) {
	for {
		open, err := c.IsBusinessDay(d)
		if err != nil || open {
			return d, err
		}
		d = DateAt(d.utc().AddDate(0, 0, 1), time.UTC)
	}
}
