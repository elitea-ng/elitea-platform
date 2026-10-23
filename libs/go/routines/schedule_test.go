package routines

import (
	"errors"
	"testing"
	"time"
)

var noon = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func TestParseScheduleAcceptsIntervalsAndCron(t *testing.T) {
	interval, err := ParseSchedule("30m")
	if err != nil {
		t.Fatal(err)
	}
	if got := interval.Next(noon); !got.Equal(noon.Add(30 * time.Minute)) {
		t.Fatalf("interval next = %v", got)
	}
	cronSchedule, err := ParseSchedule("*/15 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	if got := cronSchedule.Next(noon); !got.Equal(noon.Add(15 * time.Minute)) {
		t.Fatalf("cron next = %v", got)
	}
}

func TestParseScheduleRefusesGarbage(t *testing.T) {
	for _, text := range []string{"", "   ", "often", "* * *", "500ms", "-1h", "0s"} {
		if _, err := ParseSchedule(text); !errors.Is(err, ErrInvalidSchedule) {
			t.Errorf("%q: err = %v, want ErrInvalidSchedule", text, err)
		}
	}
}

func TestValidateScheduleEnforcesTheMinimumGap(t *testing.T) {
	cases := []struct {
		text    string
		min     time.Duration
		wantErr error
	}{
		{"1h", time.Hour, nil},
		{"59m", time.Hour, ErrBelowMinimum},
		{"*/5 * * * *", time.Hour, ErrBelowMinimum},
		{"0 * * * *", time.Hour, nil},
		// Two firings an hour apart, then 23 hours: the SHORTEST gap counts.
		{"0 9,10 * * *", 2 * time.Hour, ErrBelowMinimum},
		{"0 3 * * *", 12 * time.Hour, nil},
		{"1m", 0, nil},
	}
	for _, c := range cases {
		_, err := ValidateSchedule(c.text, c.min, noon)
		if !errors.Is(err, c.wantErr) && (err != nil || c.wantErr != nil) {
			t.Errorf("%q min %s: err = %v, want %v", c.text, c.min, err, c.wantErr)
		}
	}
}
