// Package schedule decides when tasks are due and remembers when they last ran.
package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Grace lets a task run at a check that comes slightly before its due time.
// Without it, a daily task started at 09:15:02 would not be due at 09:15:00
// the next day and would drift by one timer interval every day.
const Grace = 2 * time.Minute

// Schedule gives the next run time after a previous run.
type Schedule interface {
	Next(last time.Time) time.Time
	String() string
}

type interval struct {
	name string
	d    time.Duration
}

func (i interval) Next(last time.Time) time.Time { return last.Add(i.d) }
func (i interval) String() string                { return i.name }

type cronSchedule struct {
	spec  string
	sched cron.Schedule
}

func (c cronSchedule) Next(last time.Time) time.Time { return c.sched.Next(last) }
func (c cronSchedule) String() string                { return c.spec }

// Parse accepts hourly, daily, weekly or a five-field cron expression.
func Parse(s string) (Schedule, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "hourly":
		return interval{s, time.Hour}, nil
	case "daily":
		return interval{s, 24 * time.Hour}, nil
	case "weekly":
		return interval{s, 7 * 24 * time.Hour}, nil
	case "":
		return nil, errors.New("schedule is empty")
	}
	sched, err := cron.ParseStandard(s)
	if err != nil {
		return nil, fmt.Errorf("schedule %q is not hourly, daily, weekly or a valid cron expression: %w", s, err)
	}
	if sched.Next(time.Now()).IsZero() {
		return nil, fmt.Errorf("schedule %q never matches a date", s)
	}
	return cronSchedule{s, sched}, nil
}

// next returns when a task that last ran at last is next due. A cron run
// that started up to Grace early covered its slot, so the slot is skipped.
func next(s Schedule, last time.Time) time.Time {
	if _, ok := s.(cronSchedule); ok {
		return s.Next(last.Add(Grace))
	}
	return s.Next(last)
}

// Due reports whether a task last run at last should run at now. A task
// that never ran is due. A last run in the future means the clock moved
// back, so the task is due rather than stuck until the clock catches up.
func Due(s Schedule, last, now time.Time) bool {
	if last.IsZero() || last.After(now.Add(Grace)) {
		return true
	}
	n := next(s, last)
	return !n.IsZero() && !n.After(now.Add(Grace))
}

// NextRun returns when a task will next be due.
func NextRun(s Schedule, last, now time.Time) time.Time {
	if Due(s, last, now) {
		return now
	}
	return next(s, last)
}

// TaskState is what autobot remembers about one task between runs.
type TaskState struct {
	LastRun    time.Time `json:"last_run"`
	LastStatus string    `json:"last_status,omitempty"`
}

// State is the content of schedule.json.
type State struct {
	Tasks map[string]TaskState `json:"tasks"`
}

// LoadState reads schedule.json. A missing file is an empty state.
func LoadState(path string) (State, error) {
	st := State{Tasks: map[string]TaskState{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("%s: %w", path, err)
	}
	if st.Tasks == nil {
		st.Tasks = map[string]TaskState{}
	}
	return st, nil
}

// Save writes schedule.json atomically.
func (s State) Save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".schedule-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
