package schedule

import (
	"path/filepath"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func mustParse(t *testing.T, s string) Schedule {
	t.Helper()
	sch, err := Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return sch
}

func TestParse(t *testing.T) {
	for _, ok := range []string{"hourly", "daily", "weekly", "0 9 * * 1", "*/30 * * * *", "@daily"} {
		if _, err := Parse(ok); err != nil {
			t.Errorf("Parse(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "monthly", "0 9 * *", "61 * * * *", "0 0 30 2 *"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q): want error", bad)
		}
	}
}

func TestDue(t *testing.T) {
	daily := mustParse(t, "daily")
	mondays := mustParse(t, "0 9 * * 1") // 2026-10-19 is a Monday
	cases := []struct {
		name string
		s    Schedule
		last time.Time
		now  time.Time
		due  bool
	}{
		{"never ran", daily, time.Time{}, at("2026-10-20 10:00"), true},
		{"ran 23 hours ago", daily, at("2026-10-19 11:00"), at("2026-10-20 10:00"), false},
		{"ran 24 hours ago", daily, at("2026-10-19 10:00"), at("2026-10-20 10:00"), true},
		{"timer fires a little early", daily, at("2026-10-19 10:00"), at("2026-10-20 09:59"), true},
		{"well before due time", daily, at("2026-10-19 10:00"), at("2026-10-20 09:45"), false},
		{"missed several days runs once", daily, at("2026-10-15 10:00"), at("2026-10-20 10:00"), true},
		{"cron before its time", mondays, at("2026-10-12 09:00"), at("2026-10-19 08:30"), false},
		{"cron at its time", mondays, at("2026-10-12 09:00"), at("2026-10-19 09:00"), true},
		{"cron after a missed slot", mondays, at("2026-10-05 09:00"), at("2026-10-20 12:00"), true},
		{"cron just ran", mondays, at("2026-10-19 09:00"), at("2026-10-19 09:15"), false},
		{"clock moved back", daily, at("2026-10-21 10:00"), at("2026-10-20 10:00"), true},
		{"small clock jitter is not a move back", daily, at("2026-10-20 10:01"), at("2026-10-20 10:00"), false},
	}
	for _, c := range cases {
		if got := Due(c.s, c.last, c.now); got != c.due {
			t.Errorf("%s: Due = %v, want %v", c.name, got, c.due)
		}
	}
}

func TestNoDriftWithFifteenMinuteTimer(t *testing.T) {
	daily := mustParse(t, "daily")
	// A run that started a few seconds after a timer tick stays on that tick.
	last := at("2026-10-19 09:15").Add(4 * time.Second)
	if !Due(daily, last, at("2026-10-20 09:15")) {
		t.Fatal("a daily task should run at the same timer tick the next day")
	}
}

func TestMissedRunsDoNotCatchUp(t *testing.T) {
	hourly := mustParse(t, "hourly")
	now := at("2026-10-20 12:00")
	last := at("2026-10-20 06:00")
	if !Due(hourly, last, now) {
		t.Fatal("want due after missed runs")
	}
	// After one run, the task is not due again at the next check.
	if Due(hourly, now, now.Add(15*time.Minute)) {
		t.Fatal("missed runs must not run again one after another")
	}
}

func TestNextRun(t *testing.T) {
	daily := mustParse(t, "daily")
	now := at("2026-10-20 10:00")
	if got := NextRun(daily, time.Time{}, now); !got.Equal(now) {
		t.Errorf("new task: next run %v, want now", got)
	}
	if got := NextRun(daily, at("2026-10-20 08:00"), now); !got.Equal(at("2026-10-21 08:00")) {
		t.Errorf("next run %v, want tomorrow 08:00", got)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedule.json")
	st, err := LoadState(path)
	if err != nil || len(st.Tasks) != 0 {
		t.Fatalf("missing file: %v, %v", st, err)
	}
	when := at("2026-10-20 10:00")
	st.Tasks["notes"] = TaskState{LastRun: when, LastStatus: "completed"}
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Tasks["notes"].LastRun.Equal(when) || back.Tasks["notes"].LastStatus != "completed" {
		t.Fatalf("round trip: %+v", back)
	}
}

func TestCronRunEarlyByGraceDoesNotRunTwice(t *testing.T) {
	s := mustParse(t, "16 9 * * *")
	// The 09:15 check runs the 09:16 slot early.
	if !Due(s, at("2026-10-19 09:16"), at("2026-10-20 09:15")) {
		t.Fatal("want due at the tick before the slot")
	}
	ranAt := at("2026-10-20 09:15").Add(40 * time.Second)
	if Due(s, ranAt, at("2026-10-20 09:30")) {
		t.Fatal("the early run covered the 09:16 slot; it must not run again at 09:30")
	}
	if !Due(s, ranAt, at("2026-10-21 09:15")) {
		t.Fatal("want due again the next day")
	}
}
