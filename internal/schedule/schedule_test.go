package schedule

import (
	"testing"
	"time"
)

// at is a wall clock moment in Warsaw.
func at(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, Warsaw)
}

// instant is stamp, the way a schedule record holds it.
func instant(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// TestDailyRunsOncePerSlot checks the guard that keeps a pass from delivering the same slot twice,
// and the one that keeps it from skipping the next.
func TestDailyRunsOncePerSlot(t *testing.T) {
	s := Schedule{When: "daily 09:00", Added: instant(at(2026, 11, 20, 12, 0))}
	now := at(2026, 11, 26, 10, 30)
	slot, due, err := s.Due(now)
	if err != nil || !due || !slot.Equal(at(2026, 11, 26, 9, 0)) {
		t.Fatalf("first slot %v due=%v err=%v", slot, due, err)
	}
	s.LastRun = instant(slot)
	if _, due, _ := s.Due(now.Add(10 * time.Minute)); due {
		t.Fatal("delivered slot ran twice")
	}
	next, due, err := s.Due(at(2026, 11, 27, 9, 1))
	if err != nil || !due || !next.Equal(at(2026, 11, 27, 9, 0)) {
		t.Fatalf("next slot %v due=%v err=%v", next, due, err)
	}
}

// TestMissedSlotsRunOnce checks that a machine that was off for days catches up with one delivery
// at the current slot, not one for every slot it slept through.
func TestMissedSlotsRunOnce(t *testing.T) {
	s := Schedule{When: "daily 09:00", Added: instant(at(2026, 11, 1, 8, 0)),
		LastRun: instant(at(2026, 11, 23, 9, 0))}
	slot, due, err := s.Due(at(2026, 11, 26, 15, 0))
	if err != nil || !due || !slot.Equal(at(2026, 11, 26, 9, 0)) {
		t.Fatalf("missed slots %v due=%v err=%v", slot, due, err)
	}
	s.LastRun = instant(slot)
	if _, due, _ := s.Due(at(2026, 11, 26, 15, 10)); due {
		t.Fatal("a caught up slot ran again")
	}
}

// TestScheduleWaitsForItsNextSlot checks that adding a schedule after today's slot does not run it
// at once, which would deliver a slot that started before the schedule existed.
func TestScheduleWaitsForItsNextSlot(t *testing.T) {
	s := Schedule{When: "daily 09:00", Added: instant(at(2026, 11, 26, 10, 0))}
	if _, due, _ := s.Due(at(2026, 11, 26, 10, 30)); due {
		t.Fatal("a new schedule ran for a slot before it was added")
	}
	slot, due, err := s.Due(at(2026, 11, 27, 9, 5))
	if err != nil || !due || !slot.Equal(at(2026, 11, 27, 9, 0)) {
		t.Fatalf("next day %v due=%v err=%v", slot, due, err)
	}
}

// TestWeeklySlotIsOnItsWeekday checks that the slot lands on the named weekday at its time.
func TestWeeklySlotIsOnItsWeekday(t *testing.T) {
	s := Schedule{When: "weekly mon 09:00", Added: instant(at(2026, 11, 1, 8, 0))}
	now := at(2026, 11, 26, 10, 30)
	slot, due, err := s.Due(now)
	if err != nil || !due {
		t.Fatalf("weekly due=%v err=%v", due, err)
	}
	local := slot.In(Warsaw)
	if local.Weekday() != time.Monday || local.Hour() != 9 || local.Minute() != 0 || local.After(now) {
		t.Fatalf("weekly slot %v", local)
	}
	if now.Sub(slot) >= 7*24*time.Hour {
		t.Fatalf("weekly slot %v is not the most recent Monday", local)
	}
}

// TestEveryCountsFromLocalMidnight checks the slots of an hourly period, including the hour before
// the day's first slot, which is yesterday's last one.
func TestEveryCountsFromLocalMidnight(t *testing.T) {
	s := Schedule{When: "every 6h", Added: instant(at(2026, 11, 1, 8, 0))}
	for now, want := range map[time.Time]time.Time{
		at(2026, 11, 26, 13, 20): at(2026, 11, 26, 12, 0),
		at(2026, 11, 26, 3, 0):   at(2026, 11, 26, 0, 0),
		at(2026, 11, 26, 0, 0):   at(2026, 11, 26, 0, 0),
	} {
		slot, due, err := s.Due(now)
		if err != nil || !due || !slot.Equal(want) {
			t.Fatalf("every 6h at %v: %v due=%v err=%v", now, slot, due, err)
		}
	}
	five := Schedule{When: "every 5h", Added: instant(at(2026, 11, 1, 8, 0))}
	slot, due, err := five.Due(at(2026, 11, 26, 23, 30))
	if err != nil || !due || !slot.Equal(at(2026, 11, 26, 20, 0)) {
		t.Fatalf("every 5h at 23:30: %v due=%v err=%v", slot, due, err)
	}
}

// TestDailySlotKeepsWallClockAcrossDST checks that a daily time does not shift by an hour when
// Warsaw changes to summer time.
func TestDailySlotKeepsWallClockAcrossDST(t *testing.T) {
	var dst time.Time
	for day := at(2026, 3, 25, 12, 0); day.Month() == time.March; day = day.AddDate(0, 0, 1) {
		if day.Weekday() == time.Sunday {
			dst = day
		}
	}
	s := Schedule{When: "daily 09:00", Added: instant(at(2026, 3, 20, 8, 0)),
		LastRun: instant(at(dst.Year(), dst.Month(), dst.Day()-1, 9, 0))}
	slot, due, err := s.Due(at(dst.Year(), dst.Month(), dst.Day(), 10, 0))
	if err != nil || !due {
		t.Fatalf("dst due=%v err=%v", due, err)
	}
	local := slot.In(Warsaw)
	if local.Hour() != 9 || local.Minute() != 0 || local.Day() != dst.Day() {
		t.Fatalf("dst day slot %v", local)
	}
	if _, offset := local.Zone(); offset != 2*60*60 {
		t.Fatalf("summer time offset %d", offset)
	}
}

// TestInvalidCadences checks that a typo is refused instead of stored as a schedule that never
// runs.
func TestInvalidCadences(t *testing.T) {
	for _, text := range []string{"", "daily", "daily 9:00", "daily 24:00", "weekly funday 09:00",
		"weekly mon", "every 0h", "every 25h", "every 6", "sometimes 09:00", "daily 09:00 2026"} {
		if ValidWhen(text) {
			t.Fatalf("%q accepted", text)
		}
	}
	for _, text := range []string{"daily 00:00", "daily 23:59", "weekly sun 09:00", "every 1h", "every 24h"} {
		if !ValidWhen(text) {
			t.Fatalf("%q refused", text)
		}
	}
}
