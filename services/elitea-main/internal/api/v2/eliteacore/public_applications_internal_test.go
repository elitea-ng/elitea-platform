package eliteacore

// Unit coverage for the catalogue query parser and the SQL it builds.
//
// The parser is the enforcement point for every allowlist the OpenAPI
// description names, so these tests pin the refusals as well as the accepted
// values: a sort key that falls back silently instead of refusing is the way a
// catalogue quietly answers the wrong order.

import (
	"net/url"
	"strings"
	"testing"
)

func TestParsePublicApplicationsFilterDefaults(t *testing.T) {
	filter, err := parsePublicApplicationsFilter(url.Values{})
	if err != nil {
		t.Fatalf("empty query refused: %v", err)
	}
	if filter.SortBy != "created_at" {
		t.Errorf("default sort_by = %q, want created_at", filter.SortBy)
	}
	if filter.SortOrder != "desc" {
		t.Errorf("default sort_order = %q, want desc", filter.SortOrder)
	}
	if filter.Limit != publicApplicationsDefaultLimit {
		t.Errorf("default limit = %d, want %d", filter.Limit, publicApplicationsDefaultLimit)
	}
	if filter.Offset != 0 {
		t.Errorf("default offset = %d, want 0", filter.Offset)
	}
	if filter.MyLiked {
		t.Error("my_liked defaults to true; it must default to false")
	}
}

func TestParsePublicApplicationsFilterAccepts(t *testing.T) {
	values := url.Values{
		"category":    {"Development"},
		"query":       {"  release notes  "},
		"statuses":    {"published, draft"},
		"agents_type": {"Pipeline"},
		"sort_by":     {"NAME"},
		"sort_order":  {"ASC"},
		"limit":       {"7"},
		"offset":      {"14"},
		"my_liked":    {"true"},
	}
	filter, err := parsePublicApplicationsFilter(values)
	if err != nil {
		t.Fatalf("valid query refused: %v", err)
	}
	if filter.Category != "Development" {
		t.Errorf("category = %q", filter.Category)
	}
	if filter.Query != "release notes" {
		t.Errorf("query = %q, want the trimmed text", filter.Query)
	}
	if len(filter.Statuses) != 2 || filter.Statuses[0] != "published" || filter.Statuses[1] != "draft" {
		t.Errorf("statuses = %v", filter.Statuses)
	}
	if filter.AgentsType != "pipeline" {
		t.Errorf("agents_type = %q, want the lowercased value", filter.AgentsType)
	}
	if filter.SortBy != "name" || filter.SortOrder != "asc" {
		t.Errorf("sort = %q %q, want name asc", filter.SortBy, filter.SortOrder)
	}
	if filter.Limit != 7 || filter.Offset != 14 {
		t.Errorf("limit/offset = %d/%d, want 7/14", filter.Limit, filter.Offset)
	}
	if !filter.MyLiked {
		t.Error("my_liked=true was not read")
	}
}

func TestParsePublicApplicationsFilterRefusals(t *testing.T) {
	cases := []struct {
		name  string
		query url.Values
		param string
	}{
		{"unknown sort key", url.Values{"sort_by": {"instructions"}}, "sort_by"},
		{"sql in the sort key", url.Values{"sort_by": {"a.id; DROP TABLE applications"}}, "sort_by"},
		{"unknown direction", url.Values{"sort_order": {"sideways"}}, "sort_order"},
		{"unknown agent type", url.Values{"agents_type": {"swarm"}}, "agents_type"},
		{"unknown status", url.Values{"statuses": {"published,retired"}}, "statuses"},
		{"limit is not a number", url.Values{"limit": {"many"}}, "limit"},
		{"limit is zero", url.Values{"limit": {"0"}}, "limit"},
		{"limit is above the ceiling", url.Values{"limit": {"1001"}}, "limit"},
		{"offset is negative", url.Values{"offset": {"-1"}}, "offset"},
		{"offset is not a number", url.Values{"offset": {"page two"}}, "offset"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parsePublicApplicationsFilter(testCase.query)
			if err == nil {
				t.Fatalf("%v was accepted", testCase.query)
			}
			paramErr, ok := err.(*publicApplicationsParamError)
			if !ok {
				t.Fatalf("error type = %T, want *publicApplicationsParamError", err)
			}
			if paramErr.Param != testCase.param {
				t.Errorf("refused parameter = %q, want %q", paramErr.Param, testCase.param)
			}
			if paramErr.Error() == "" {
				t.Error("the refusal carries no message")
			}
		})
	}
}

func TestParsePublicApplicationsFilterSkipsEmptyStatuses(t *testing.T) {
	filter, err := parsePublicApplicationsFilter(url.Values{"statuses": {"published,,"}})
	if err != nil {
		t.Fatalf("trailing separators refused: %v", err)
	}
	if len(filter.Statuses) != 1 || filter.Statuses[0] != "published" {
		t.Errorf("statuses = %v, want [published]", filter.Statuses)
	}
}

func TestParseBoolParam(t *testing.T) {
	for _, truthy := range []string{"1", "true", "TRUE", " yes ", "on"} {
		if !parseBoolParam(truthy) {
			t.Errorf("%q read as false", truthy)
		}
	}
	for _, falsy := range []string{"", "0", "false", "no", "maybe"} {
		if parseBoolParam(falsy) {
			t.Errorf("%q read as true", falsy)
		}
	}
}

// TestPublicApplicationsWhereBindsEveryCallerValue is the injection guard: no
// value a caller supplies may reach the statement text.
func TestPublicApplicationsWhereBindsEveryCallerValue(t *testing.T) {
	args := &argList{}
	filter := publicApplicationsFilter{
		Category:   "Devel'opment",
		Query:      "100% sure",
		Statuses:   []string{"published"},
		AgentsType: "classic",
		MyLiked:    true,
	}
	where := publicApplicationsWhere(`"p_1"`, filter, 42, args)

	for _, forbidden := range []string{"Devel'opment", "100% sure"} {
		if strings.Contains(where, forbidden) {
			t.Errorf("caller text %q reached the statement:\n%s", forbidden, where)
		}
	}
	if len(args.values) != 4 {
		t.Fatalf("bound %d parameters, want 4: %v", len(args.values), args.values)
	}
	if args.values[2] != "%100% sure%" {
		t.Errorf("search pattern = %v", args.values[2])
	}
	if args.values[3] != 42 {
		t.Errorf("my_liked bound %v, want the caller's user id", args.values[3])
	}
	if !strings.Contains(where, `COALESCE(av.agent_type, '') != 'pipeline'`) {
		t.Errorf("agents_type=classic did not narrow the set:\n%s", where)
	}
}

func TestPublicApplicationsWhereOtherCategoryTakesInTheUncategorised(t *testing.T) {
	args := &argList{}
	where := publicApplicationsWhere(`"p_1"`, publicApplicationsFilter{Category: "Other"}, 0, args)
	if len(args.values) != 0 {
		t.Errorf("the Other bucket bound %v; it is a fixed predicate", args.values)
	}
	if !strings.Contains(where, "av.meta->>'category' IS NULL") {
		t.Errorf("the Other bucket does not take in agents with no category:\n%s", where)
	}
}

// TestPublicApplicationsWhereMyLikedWithoutAnIdentityAnswersNothing pins the
// choice that matters most here: an anonymous caller asking for "My Liked"
// gets no rows, not the whole catalogue under a personal label.
func TestPublicApplicationsWhereMyLikedWithoutAnIdentityAnswersNothing(t *testing.T) {
	args := &argList{}
	where := publicApplicationsWhere(`"p_1"`, publicApplicationsFilter{MyLiked: true}, 0, args)
	if !strings.Contains(where, "AND FALSE") {
		t.Errorf("my_liked with no user identity did not empty the result:\n%s", where)
	}
	if strings.Contains(where, "social_likes") {
		t.Errorf("my_liked with no user identity still joined the like store:\n%s", where)
	}
}

func TestPublicApplicationsWherePipelineOnly(t *testing.T) {
	args := &argList{}
	where := publicApplicationsWhere(`"p_1"`, publicApplicationsFilter{AgentsType: "pipeline"}, 0, args)
	if !strings.Contains(where, `av.agent_type = 'pipeline'`) {
		t.Errorf("agents_type=pipeline did not narrow the set:\n%s", where)
	}
}

func TestPublicApplicationsOrderBy(t *testing.T) {
	cases := map[string]struct {
		filter publicApplicationsFilter
		want   string
	}{
		"created_at desc": {publicApplicationsFilter{SortBy: "created_at", SortOrder: "desc"}, " ORDER BY a.created_at DESC, a.id DESC"},
		"name asc":        {publicApplicationsFilter{SortBy: "name", SortOrder: "asc"}, " ORDER BY a.name ASC, a.id DESC"},
		"likes desc":      {publicApplicationsFilter{SortBy: "likes", SortOrder: "desc"}, " ORDER BY likes DESC, a.id DESC"},
		"id asc":          {publicApplicationsFilter{SortBy: "id", SortOrder: "asc"}, " ORDER BY a.id ASC, a.id DESC"},
		"unknown key":     {publicApplicationsFilter{SortBy: "nonsense", SortOrder: "desc"}, " ORDER BY a.id DESC, a.id DESC"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if got := publicApplicationsOrderBy(testCase.filter); got != testCase.want {
				t.Errorf("ORDER BY = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestDecodeJSONHelpers(t *testing.T) {
	if got := decodeJSONObject([]byte(`{"category":"Development"}`)); got["category"] != "Development" {
		t.Errorf("meta decode = %v", got)
	}
	if got := decodeJSONObject([]byte(`not json`)); got != nil {
		t.Errorf("malformed meta = %v, want nil", got)
	}
	if got := decodeJSONArray([]byte(`[{"id":1}]`)); len(got) != 1 {
		t.Errorf("tags decode = %v", got)
	}
	if got := decodeJSONArray([]byte(`null`)); got == nil || len(got) != 0 {
		t.Errorf("a null tag column = %v, want an empty array", got)
	}
	if got := decodeJSONArray([]byte(`not json`)); got == nil || len(got) != 0 {
		t.Errorf("a malformed tag column = %v, want an empty array", got)
	}
}
