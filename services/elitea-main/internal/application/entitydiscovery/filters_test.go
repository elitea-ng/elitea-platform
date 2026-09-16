package entitydiscovery

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseBoundsAndFilters(t *testing.T) {
	for _, values := range []url.Values{
		{"entity_coverage": {"unknown"}}, {"limit": {"1001"}}, {"offset": {"100001"}},
		{"author_id": {"-1"}}, {"tags[]": {"bad"}}, {"query": {strings.Repeat("q", 1025)}},
		{"my_liked": {"sometimes"}}, {"trend_start_period": {"yesterday"}},
		{"trend_end_period": {"2026-01-01T00:00:00"}},
		{"trend_start_period": {"2026-02-01T00:00:00"}, "trend_end_period": {"2026-01-01T00:00:00"}},
	} {
		if _, err := Parse(values); err == nil {
			t.Fatalf("accepted invalid values: %v", values)
		}
	}
	f, err := Parse(url.Values{"entity_coverage": {"pipeline"}, "tags[]": {"2", "2", "3"}, "statuses": {"published,draft"}, "limit": {"0"}})
	if err != nil || f.Limit != 1000 || len(f.Tags) != 2 || len(f.Statuses) != 2 || f.Coverage != "pipeline" {
		t.Fatalf("filters=%+v err=%v", f, err)
	}
}
