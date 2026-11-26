// Package schedule stores the timers that ask a project something on a fixed cadence, one JSON
// file per schedule, and works out which of them is due.
package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/janpuc/agent/internal/registry"
	"github.com/janpuc/agent/internal/workspace"
)

// Warsaw is the zone daily and weekly times are written in. The zone database is embedded in the
// binary, so a host or container without tzdata still runs its schedules.
var Warsaw = func() *time.Location {
	zone, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		panic(err)
	}
	return zone
}()

// Schedule is one timer: a text delivered to a project's inbox on a cadence. The name is the file
// name, so it is the same in every record.
type Schedule struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	// When is the cadence as typed, for example "weekly mon 09:00".
	When string `json:"when"`
	Text string `json:"text"`
	// Added is when the schedule was created. No slot that started before it is due, so a new
	// schedule waits for its next slot instead of running at once.
	Added string `json:"added"`
	// LastRun is the start of the slot last delivered for. A slot is delivered once, however many
	// passes see it.
	LastRun string `json:"lastRun,omitempty"`
}

// when is a parsed cadence: daily or weekly at a wall clock time, or every whole number of hours
// from local midnight.
type when struct {
	kind    string
	weekday time.Weekday
	hour    int
	minute  int
	period  time.Duration
}

// days names the weekdays in the order a person reads them.
var days = map[string]time.Weekday{
	"mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday,
	"fri": time.Friday, "sat": time.Saturday, "sun": time.Sunday,
}

// ValidWhen reports whether text is a cadence: daily HH:MM, weekly mon HH:MM or every Nh.
func ValidWhen(text string) bool {
	_, err := parseWhen(text)
	return err == nil
}

// parseWhen reads a cadence.
func parseWhen(text string) (when, error) {
	fields := strings.Fields(text)
	if len(fields) == 2 && fields[0] == "daily" {
		hour, minute, err := clock(fields[1])
		return when{kind: "daily", hour: hour, minute: minute}, err
	}
	if len(fields) == 3 && fields[0] == "weekly" {
		weekday, ok := days[strings.ToLower(fields[1])]
		if !ok {
			return when{}, fmt.Errorf("Invalid weekday %q, write mon to sun", fields[1])
		}
		hour, minute, err := clock(fields[2])
		return when{kind: "weekly", weekday: weekday, hour: hour, minute: minute}, err
	}
	if len(fields) == 2 && fields[0] == "every" {
		hours, err := strconv.Atoi(strings.TrimSuffix(fields[1], "h"))
		if err != nil || hours < 1 || hours > 24 || !strings.HasSuffix(fields[1], "h") {
			return when{}, fmt.Errorf("Invalid period %q, write it as every Nh with N from 1 to 24", fields[1])
		}
		return when{kind: "every", period: time.Duration(hours) * time.Hour}, nil
	}
	return when{}, errors.New("Invalid schedule time, write daily HH:MM, weekly mon HH:MM or every Nh")
}

// clock reads a wall clock time written as HH:MM.
func clock(text string) (int, int, error) {
	parts := strings.Split(text, ":")
	if len(parts) == 2 && len(parts[0]) == 2 && len(parts[1]) == 2 {
		hour, hourErr := strconv.Atoi(parts[0])
		minute, minuteErr := strconv.Atoi(parts[1])
		if hourErr == nil && minuteErr == nil && hour < 24 && minute < 60 {
			return hour, minute, nil
		}
	}
	return 0, 0, fmt.Errorf("Invalid time %q, write it as HH:MM", text)
}

// slot returns the start of the most recent slot at or before now. The slots of a day are fixed in
// Warsaw wall clock time, so a daily 09:00 stays at 09:00 across the summer time change.
func (w when) slot(now time.Time) time.Time {
	local := now.In(Warsaw)
	for day := local; ; day = day.AddDate(0, 0, -1) {
		if w.kind == "weekly" && day.Weekday() != w.weekday {
			continue
		}
		var slots []time.Time
		switch w.kind {
		case "daily", "weekly":
			slots = []time.Time{time.Date(day.Year(), day.Month(), day.Day(), w.hour, w.minute, 0, 0, Warsaw)}
		default:
			for hour := 0; hour < 24; hour += int(w.period.Hours()) {
				slots = append(slots, time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, Warsaw))
			}
		}
		for i := len(slots) - 1; i >= 0; i-- {
			if !slots[i].After(now) {
				return slots[i]
			}
		}
	}
}

// Due reports whether the schedule has a slot that started after it was added and after its last
// run, and returns that slot's start. A slot missed while the machine was off is one due slot, so
// it delivers once, never once per missed slot.
func (s Schedule) Due(now time.Time) (time.Time, bool, error) {
	cadence, err := parseWhen(s.When)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("%s: %w", s.When, err)
	}
	start := cadence.slot(now)
	for _, stamp := range []string{s.LastRun, s.Added} {
		if stamp == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("invalid time %q", stamp)
		}
		if !start.After(parsed) {
			return time.Time{}, false, nil
		}
	}
	return start, true, nil
}

// Dir is the directory that holds one file per schedule.
func Dir(home string) string { return filepath.Join(registry.StateDir(home), "schedules") }

// Path is one schedule's file.
func Path(home, name string) string { return filepath.Join(Dir(home), name+".json") }

// Load reads every schedule in name order, none when the directory does not exist yet.
func Load(home string) ([]Schedule, error) {
	entries, err := os.ReadDir(Dir(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var schedules []Schedule
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(Dir(home), entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var s Schedule
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("invalid schedule %s: %w", path, err)
		}
		schedules = append(schedules, s)
	}
	sort.Slice(schedules, func(i, j int) bool { return schedules[i].Name < schedules[j].Name })
	return schedules, nil
}

// Save writes one schedule, replacing the file of the same name.
func Save(home string, s Schedule) error {
	if err := os.MkdirAll(Dir(home), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return workspace.AtomicWrite(Path(home, s.Name), append(data, '\n'))
}

// Remove deletes one schedule.
func Remove(home, name string) error {
	err := os.Remove(Path(home, name))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no schedule %s", name)
	}
	return err
}
