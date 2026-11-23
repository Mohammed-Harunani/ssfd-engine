package simclock

import (
	"testing"
	"time"
)

func TestDSTRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		zone      Zone
		gap, fold Civil
		want      time.Time
	}{
		{London, Civil{2024, 3, 31, 1, 30, 0, 0}, Civil{2024, 10, 27, 1, 30, 0, 0}, time.Date(2024, 10, 27, 0, 30, 0, 0, time.UTC)},
		{NewYork, Civil{2024, 3, 10, 2, 30, 0, 0}, Civil{2024, 11, 3, 1, 30, 0, 0}, time.Date(2024, 11, 3, 5, 30, 0, 0, time.UTC)},
	} {
		loc, err := tc.zone.Location()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tc.gap.Resolve(loc, Earlier); err == nil {
			t.Fatalf("%s accepted gap", tc.zone)
		}
		if _, err := tc.fold.Resolve(loc, RejectAmbiguous); err == nil {
			t.Fatalf("%s accepted ambiguous time", tc.zone)
		}
		a, errA := tc.fold.Resolve(loc, Earlier)
		b, errB := tc.fold.Resolve(loc, Later)
		if errA != nil || errB != nil || !a.Equal(tc.want) || b.Sub(a) != time.Hour {
			t.Fatalf("%s unexpected fold %v %v %v %v", tc.zone, a, b, errA, errB)
		}
	}
}

func TestHorizonAndLeapDay(t *testing.T) {
	start := time.Date(2023, 12, 31, 23, 59, 59, 0, time.UTC)
	end := time.Date(2028, 3, 1, 0, 0, 0, 0, time.UTC)
	clock, err := New(start, end)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (Civil{2023, 2, 29, 12, 0, 0, 0}).Resolve(time.UTC, Earlier); err == nil {
		t.Fatal("accepted non-leap day")
	}
	leap, err := (Civil{2024, 2, 29, 23, 0, 0, 0}).Resolve(time.UTC, Earlier)
	if err != nil || clock.AdvanceTo(leap) != nil || clock.AdvanceElapsed(time.Hour) != nil ||
		clock.Now().Month() != time.March || clock.AdvanceTo(end) != nil {
		t.Fatal("could not advance across Gregorian dates")
	}
	if clock.AdvanceTo(leap) == nil || clock.AdvanceElapsed(time.Nanosecond) == nil || clock.Now() != end {
		t.Fatal("clock escaped horizon")
	}
}

func TestHolidayObservance(t *testing.T) {
	for _, tc := range []struct {
		market Market
		day    Date
		want   bool
	}{
		{EnglandWales, Date{2022, 9, 19}, false},  // State Funeral
		{EnglandWales, Date{2022, 12, 27}, false}, // Christmas substitute
		{EnglandWales, Date{2023, 5, 8}, false},   // Coronation
		{Scotland, Date{2024, 1, 2}, false},
		{EnglandWales, Date{2024, 1, 2}, true},
		{NorthernIreland, Date{2024, 7, 12}, false},
		{USFederalReserve, Date{2026, 7, 3}, true},  // Saturday holiday
		{USFederalReserve, Date{2027, 7, 5}, false}, // Sunday observed Monday
		{USFederalReserve, Date{2028, 6, 19}, false},
		{USFederalReserve, Date{2028, 11, 23}, false},
	} {
		cal, err := NewCalendar(tc.market)
		if err != nil {
			t.Fatal(err)
		}
		got, err := cal.IsBusinessDay(tc.day)
		if err != nil || got != tc.want {
			t.Errorf("%s %v: got %t, %v", tc.market, tc.day, got, err)
		}
	}
	c, _ := NewCalendar(EnglandWales)
	got, err := c.Following(Date{2022, 12, 24})
	if err != nil || got != (Date{2022, 12, 28}) {
		t.Fatalf("following holiday: %v %v", got, err)
	}
	if _, err := c.IsBusinessDay(Date{2029, 1, 1}); err == nil {
		t.Fatal("accepted date beyond dataset")
	}
}
