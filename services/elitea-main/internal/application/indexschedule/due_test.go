package indexschedule

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// elitea_issues: #5142 — a schedule configured to run every hour must
// actually become due once an hour, not silently never fire. Two cases in
// the fixture ("hourly schedule due on the hour" / "... not yet due before
// the hour") pin the "0 * * * *" cron specifically.
func TestDueOccurrenceMatchesFrozenCurrentContract(t *testing.T) {
	type dueCase struct {
		Name       string `json:"name"`
		Cron       string `json:"cron"`
		Timezone   string `json:"timezone"`
		LastRun    string `json:"last_run"`
		Now        string `json:"now"`
		Enabled    bool   `json:"enabled"`
		Due        bool   `json:"due"`
		Occurrence string `json:"occurrence"`
	}
	var fixture struct {
		Cases []dueCase `json:"cases"`
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "current_python_schedule_due_contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) < 8 {
		t.Fatalf("due fixture has only %d cases", len(fixture.Cases))
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339Nano, test.Now)
			if err != nil {
				t.Fatal(err)
			}
			occurrence, due, err := DueOccurrence(Schedule{
				Cron: test.Cron, Enabled: test.Enabled, CreatedBy: 11,
				Timezone: test.Timezone, LastRun: test.LastRun,
			}, now)
			if err != nil {
				t.Fatalf("DueOccurrence() error=%v", err)
			}
			if due != test.Due {
				t.Fatalf("DueOccurrence() due=%v, want %v", due, test.Due)
			}
			if !due {
				if !occurrence.IsZero() {
					t.Fatalf("not-due occurrence=%v", occurrence)
				}
				return
			}
			want, err := time.Parse(time.RFC3339Nano, test.Occurrence)
			if err != nil {
				t.Fatal(err)
			}
			if !occurrence.Equal(want) {
				t.Fatalf("occurrence=%v, want %v", occurrence, want)
			}
		})
	}
}

func TestDueOccurrenceRejectsInvalidStoredRows(t *testing.T) {
	now := time.Date(2026, 7, 28, 4, 0, 0, 0, time.UTC)
	valid := Schedule{
		Cron: "0 3 * * *", Enabled: true, CreatedBy: 11,
		Timezone: "UTC", LastRun: "2026-07-27T03:00:00+00:00",
	}
	tests := map[string]func(*Schedule){
		"creator":   func(value *Schedule) { value.CreatedBy = 0 },
		"cron":      func(value *Schedule) { value.Cron = "invalid" },
		"timezone":  func(value *Schedule) { value.Timezone = "Local" },
		"last run":  func(value *Schedule) { value.LastRun = "invalid" },
		"null byte": func(value *Schedule) { value.Cron += "\x00" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := valid
			mutate(&value)
			if _, _, err := DueOccurrence(value, now); err != ErrInvalidStoredSchedule {
				t.Fatalf("DueOccurrence() error=%v", err)
			}
		})
	}
}

func TestDueOccurrenceAcceptsNaiveLastRunAsUTC(t *testing.T) {
	occurrence, due, err := DueOccurrence(Schedule{
		Cron: "0 3 * * *", Enabled: true, CreatedBy: 11,
		Timezone: "UTC", LastRun: "2026-07-27 03:00:00",
	}, time.Date(2026, 7, 28, 3, 0, 0, 0, time.UTC))
	if err != nil || !due ||
		!occurrence.Equal(time.Date(2026, 7, 28, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("DueOccurrence() occurrence=%v due=%v error=%v", occurrence, due, err)
	}
}
