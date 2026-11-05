package cron

import (
	"reflect"
	"testing"
	"time"
)

// Calendar (UTC): 2026-02-02 Mon, 2026-02-07 Sat, 2026-03-07 Sat,
// 2026-03-09 Mon, 2026-03-27 Fri, 2026-04-13 Mon, 2026-07-05 Sun, 2026-07-10 Fri.
func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func mustParse(t *testing.T, spec string) *Schedule {
	t.Helper()
	s, err := Parse(spec)
	if err != nil {
		t.Fatalf("Parse(%q): %v", spec, err)
	}
	return s
}

func TestParseFieldSyntax(t *testing.T) {
	cases := []struct {
		expr     string
		min, max int
		want     []int
	}{
		{"*", 0, 5, []int{0, 1, 2, 3, 4, 5}},
		{"1,3,5", 0, 59, []int{1, 3, 5}},
		{"1-4", 0, 59, []int{1, 2, 3, 4}},
		{"*/15", 0, 59, []int{0, 15, 30, 45}},
		{"*/10", 1, 31, []int{1, 11, 21, 31}},
		{"10-30/10", 0, 59, []int{10, 20, 30}},
		{"5/20", 0, 59, []int{5, 25, 45}},
		{"3,1,3,2-3", 0, 59, []int{1, 2, 3}},
	}
	for _, c := range cases {
		got, err := ParseField(c.expr, c.min, c.max)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseField(%q, %d, %d) = %v, %v; want %v", c.expr, c.min, c.max, got, err, c.want)
		}
	}
}

func TestParseFieldErrors(t *testing.T) {
	for _, expr := range []string{"", "60", "5-1", "*/0", "1-", "a", "1,,2", "-1", "+5", "1/2/3", "*/", "1-5/x"} {
		if got, err := ParseField(expr, 0, 59); err == nil {
			t.Errorf("ParseField(%q) = %v, want error", expr, got)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, spec := range []string{"* * * *", "* * * * * *", "0 24 * * *", "0 0 0 * *", "0 0 * 13 *", "0 0 * * 8", "0 0 * FOO *"} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) should fail", spec)
		}
	}
}

func TestNamesAreCaseInsensitiveAndWorkInRanges(t *testing.T) {
	s := mustParse(t, "0 9 * JAN-mar mon-FRI")
	if !s.Matches(at("2026-02-02 09:00")) {
		t.Error("should match Monday 2 Feb 09:00")
	}
	if s.Matches(at("2026-02-07 09:00")) {
		t.Error("should not match Saturday")
	}
	if s.Matches(at("2026-04-06 09:00")) {
		t.Error("should not match April")
	}
}

func TestSevenIsSunday(t *testing.T) {
	s := mustParse(t, "0 0 * * 7")
	if !s.Matches(at("2026-07-05 00:00")) {
		t.Error("7 should mean Sunday")
	}
	if s.Matches(at("2026-07-06 00:00")) {
		t.Error("Monday must not match")
	}
}

func TestDomDowOrWhenBothRestricted(t *testing.T) {
	s := mustParse(t, "0 12 13 * FRI")
	if !s.Matches(at("2026-03-27 12:00")) {
		t.Error("a Friday that is not the 13th should match (OR rule)")
	}
	if !s.Matches(at("2026-04-13 12:00")) {
		t.Error("the 13th that is not a Friday should match (OR rule)")
	}
}

func TestDomDowAndWhenOneIsStar(t *testing.T) {
	s := mustParse(t, "0 12 */2 * FRI")
	if !s.Matches(at("2026-03-27 12:00")) {
		t.Error("Friday the 27th (odd day) should match")
	}
	if s.Matches(at("2026-07-10 12:00")) {
		t.Error("Friday the 10th (even day) must not match: */2 is star-prefixed, so AND applies")
	}
}

func TestNext(t *testing.T) {
	cases := []struct {
		spec, after, want string
	}{
		{"*/15 * * * *", "2026-03-02 10:07", "2026-03-02 10:15"},
		{"*/15 * * * *", "2026-03-02 10:15", "2026-03-02 10:30"},
		{"30 2 * * *", "2026-12-31 23:59", "2027-01-01 02:30"},
		{"0 0 29 2 *", "2026-03-01 00:00", "2028-02-29 00:00"},
		{"0 9 * * MON-FRI", "2026-03-27 10:00", "2026-03-30 09:00"},
		{"0 0 13 * MON", "2026-03-07 00:00", "2026-03-09 00:00"},
	}
	for _, c := range cases {
		s := mustParse(t, c.spec)
		got, ok := s.Next(at(c.after))
		if !ok || !got.Equal(at(c.want)) {
			t.Errorf("Parse(%q).Next(%s) = %v, %v; want %s", c.spec, c.after, got, ok, c.want)
		}
	}
}

func TestNextStrictlyAfterAndZeroSeconds(t *testing.T) {
	s := mustParse(t, "* * * * *")
	after := time.Date(2026, 3, 2, 10, 7, 42, 500, time.UTC)
	got, ok := s.Next(after)
	if !ok || !got.Equal(time.Date(2026, 3, 2, 10, 8, 0, 0, time.UTC)) {
		t.Errorf("Next(%v) = %v, %v", after, got, ok)
	}
}

func TestNextImpossible(t *testing.T) {
	s := mustParse(t, "0 0 30 2 *")
	if got, ok := s.Next(at("2026-01-01 00:00")); ok {
		t.Errorf("Feb 30 should never match, got %v", got)
	}
}
