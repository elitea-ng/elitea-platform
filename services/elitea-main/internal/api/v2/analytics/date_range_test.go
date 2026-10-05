package analytics

// A reversed date window is the caller's mistake (legacy issue 6738). It used to
// reach the queries as it was, every half-open predicate matched nothing, and
// the page painted an empty dashboard that read as "no traffic".

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

var dateRangeNow = time.Date(2026, 9, 24, 19, 7, 0, 0, time.UTC)

func fixedClock() time.Time { return dateRangeNow }

func TestDateWindowRefusesAReversedRange(t *testing.T) {
	for name, query := range map[string]url.Values{
		"from after to":            {"date_from": {"2026-09-26T19:10:00Z"}, "date_to": {"2026-09-24T19:07:00Z"}},
		"date-only, from after to": {"date_from": {"2026-09-26"}, "date_to": {"2026-09-24"}},
		"future from, no to":       {"date_from": {"2026-10-30T00:00:00Z"}},
		"three months reversed":    {"date_from": {"2026-09-26"}, "date_to": {"2026-06-26"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := dateWindow(query, fixedClock); !errors.Is(err, errInvalidDateRange) {
				t.Fatalf("err = %v, want errInvalidDateRange", err)
			}
		})
	}
}

func TestDateWindowAcceptsOrderedAndEmptyRanges(t *testing.T) {
	for name, query := range map[string]url.Values{
		"ordered":      {"date_from": {"2026-06-26T19:10:00Z"}, "date_to": {"2026-09-24T19:07:00Z"}},
		"equal bounds": {"date_from": {"2026-09-24T19:07:00Z"}, "date_to": {"2026-09-24T19:07:00Z"}},
		"only to":      {"date_to": {"2026-09-24T19:07:00Z"}},
		"neither":      {},
	} {
		t.Run(name, func(t *testing.T) {
			from, to, err := dateWindow(query, fixedClock)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if from.After(to) {
				t.Fatalf("window %s .. %s is reversed", from, to)
			}
		})
	}
}

// Every analytics route answers 400 invalid_date_range, and the repository is
// never asked: the stub would answer 500 if it were.
func TestAnalyticsRoutesAnswerInvalidDateRange(t *testing.T) {
	t.Parallel()
	const reversed = "date_from=2026-09-26T19:10:00Z&date_to=2026-09-24T19:07:00Z"
	for _, route := range []string{
		"/?" + reversed,
		"/agents?" + reversed,
		"/agents?application_id=7&" + reversed,
		"/tools?" + reversed,
		"/users?" + reversed,
		// The start_date/end_date pair is normalised onto the same check.
		"/users?start_date=2026-09-26&end_date=2026-09-24",
	} {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			rec, body := do(t, stubRepo{err: errors.New("the repository must not be reached")}, route)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %v)", rec.Code, body)
			}
			if body["code"] != "invalid_date_range" {
				t.Fatalf("code = %v, want invalid_date_range", body["code"])
			}
		})
	}
}

func TestCostsAnswersInvalidDateRange(t *testing.T) {
	// No pool: the window is checked before any read, so a nil pool proves
	// the database is never reached.
	costs := NewCostsHandler(nil).WithClock(fixedClock)
	router := chi.NewRouter()
	router.Get("/analytics_costs/prompt_lib/{projectID}", costs.Costs)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/analytics_costs/prompt_lib/3?date_from=2026-09-26&date_to=2026-09-24", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"code":"invalid_date_range"`) {
		t.Fatalf("body = %s, want code invalid_date_range", body)
	}
}
