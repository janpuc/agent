package watch

import (
	"time"

	"github.com/janpuc/agent/internal/project"
	"github.com/janpuc/agent/internal/requests"
	"github.com/janpuc/agent/internal/schedule"
)

// runSchedules delivers every due schedule into its project's inbox. A schedule runs once per
// slot and records that slot, so a machine that was off for days catches up with one delivery. A
// schedule whose project this host no longer runs is skipped and consumes its slot, so a daily
// timer does not fill the log on every pass.
func runSchedules(h project.Host, logger *log, dryRun bool) error {
	schedules, err := schedule.Load(h.Home)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, s := range schedules {
		slot, due, err := s.Due(now)
		if err != nil {
			if err := logger.line("schedule %s: %v", s.Name, err); err != nil {
				return err
			}
			continue
		}
		if !due {
			continue
		}
		if !runsHere(h, s.Project) {
			if dryRun {
				if err := logger.line("schedule %s: project %s is not active here, would skip", s.Name, s.Project); err != nil {
					return err
				}
				continue
			}
			s.LastRun = slot.UTC().Format(time.RFC3339)
			if err := schedule.Save(h.Home, s); err != nil {
				return err
			}
			if err := logger.line("schedule %s: project %s is not active here, skipped", s.Name, s.Project); err != nil {
				return err
			}
			continue
		}
		if dryRun {
			if err := logger.line("schedule %s: would ask %s %q", s.Name, s.Project, s.Text); err != nil {
				return err
			}
			continue
		}
		if err := deliver(h, s); err != nil {
			if err := logger.line("schedule %s: could not ask %s: %v", s.Name, s.Project, err); err != nil {
				return err
			}
			continue
		}
		s.LastRun = slot.UTC().Format(time.RFC3339)
		if err := schedule.Save(h.Home, s); err != nil {
			return err
		}
		if err := logger.line("schedule %s: asked %s", s.Name, s.Project); err != nil {
			return err
		}
	}
	return nil
}

// runsHere reports whether name is an active project of this host.
func runsHere(h project.Host, name string) bool {
	if _, ok := h.Registry.Projects[name]; !ok || h.Registry.ProjectHost(name) != h.Name {
		return false
	}
	lifecycle, err := h.Registry.Lifecycle(h.Home, name)
	return err == nil && lifecycle == "active"
}

// deliver puts the schedule's text in the project's inbox as a request, which the project's agent
// answers with agent reply.
func deliver(h project.Host, s schedule.Schedule) error {
	id, err := requests.NewID()
	if err != nil {
		return err
	}
	item := requests.Request{ID: id, From: requests.ScheduleSender, To: s.Project, Text: s.Text,
		Created: requests.Timestamp(), Status: "new"}
	return requests.Store(h.Registry, h.Home, h.Name, h.Root, s.Project, id, item)
}
