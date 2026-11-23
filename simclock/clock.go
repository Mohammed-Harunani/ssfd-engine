// Package simclock provides a forward-only virtual clock and pinned calendars.
package simclock

import (
	"embed"
	"fmt"
	"time"
)

//go:embed data/*
var data embed.FS

type Zone string

const (
	London  Zone = "Europe/London"
	NewYork Zone = "America/New_York"
	UTC     Zone = "UTC"
)

// Location loads bundled TZif bytes, independently of the host tzdata.
func (z Zone) Location() (*time.Location, error) {
	var file string
	switch z {
	case London:
		file = "London"
	case NewYork:
		file = "New_York"
	case UTC:
		return time.UTC, nil
	default:
		return nil, fmt.Errorf("unsupported time zone %q", z)
	}
	raw, err := data.ReadFile("data/" + file)
	if err != nil {
		return nil, err
	}
	return time.LoadLocationFromTZData(string(z), raw)
}

// Civil specifies a local wall-clock instant, including its fractional second.
type Civil struct {
	Year                                  int
	Month                                 time.Month
	Day, Hour, Minute, Second, Nanosecond int
}

type Fold uint8

const (
	RejectAmbiguous Fold = iota
	Earlier
	Later
)

// Resolve identifies local-time candidates using the offsets on either side
// of nearby transitions. A missing wall time is rejected.
func (c Civil) Resolve(loc *time.Location, fold Fold) (time.Time, error) {
	wall := time.Date(c.Year, c.Month, c.Day, c.Hour, c.Minute, c.Second, c.Nanosecond, time.UTC)
	if loc == nil || fold > Later || !validInstant(wall) || wall.Year() != c.Year ||
		wall.Month() != c.Month || wall.Day() != c.Day || wall.Hour() != c.Hour ||
		wall.Minute() != c.Minute || wall.Second() != c.Second || wall.Nanosecond() != c.Nanosecond {
		return time.Time{}, fmt.Errorf("invalid civil time")
	}
	lo, hi := wall.Add(-48*time.Hour), wall.Add(48*time.Hour)
	var first, last time.Time
	for probe := lo; !probe.After(hi); {
		local := probe.In(loc)
		_, offset := local.Zone()
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		_, actual := candidate.In(loc).Zone()
		if offset == actual && validInstant(candidate) {
			if first.IsZero() || candidate.Before(first) {
				first = candidate
			}
			if last.IsZero() || candidate.After(last) {
				last = candidate
			}
		}
		_, end := local.ZoneBounds()
		if !end.After(probe) {
			break
		}
		probe = end
	}
	if first.IsZero() {
		return time.Time{}, fmt.Errorf("nonexistent civil time")
	}
	if !first.Equal(last) && fold == RejectAmbiguous {
		return time.Time{}, fmt.Errorf("ambiguous civil time")
	}
	if fold == Later {
		return last, nil
	}
	return first, nil
}

func validInstant(t time.Time) bool {
	return !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999
}

// Clock holds the current and inclusive end instants in normalized UTC.
type Clock struct{ now, end time.Time }

func New(start, end time.Time) (*Clock, error) {
	start, end = start.UTC(), end.UTC()
	if !validInstant(start) || !validInstant(end) || end.Before(start) {
		return nil, fmt.Errorf("invalid simulation horizon")
	}
	return &Clock{now: start, end: end}, nil
}

func (c *Clock) Now() time.Time { return c.now }

func (c *Clock) AdvanceTo(next time.Time) error {
	next = next.UTC()
	if next.Before(c.now) || next.After(c.end) {
		return fmt.Errorf("instant outside remaining simulation horizon")
	}
	c.now = next
	return nil
}

// AdvanceElapsed advances real elapsed time. Use Civil for local scheduling.
func (c *Clock) AdvanceElapsed(elapsed time.Duration) error {
	return c.AdvanceTo(c.now.Add(elapsed))
}
