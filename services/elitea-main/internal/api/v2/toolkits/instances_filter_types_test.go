package toolkits

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseInstanceListFilterReadsARepeatableTypeParameter(t *testing.T) {
	filter, err := parseInstanceListFilter(url.Values{"type": {"wiki", " deepwiki ", "wiki", ""}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(filter.Types, ",") != "wiki,deepwiki" {
		t.Fatalf("types = %v, want [wiki deepwiki] trimmed and deduplicated", filter.Types)
	}
	where, args := instanceWhere(filter)
	if where != ` WHERE type = ANY($1::text[])` || len(args) != 1 {
		t.Fatalf("where = %q args = %v", where, args)
	}
}

func TestInstanceTypeFilterCombinesWithTheOtherFiltersAndBoundsItsInput(t *testing.T) {
	yes := true
	where, args := instanceWhere(InstanceListFilter{MCP: &yes, Query: "q", Types: []string{"mcp"}})
	if !strings.Contains(where, "type = ANY($2::text[])") || len(args) != 2 {
		t.Fatalf("the type placeholder must follow the query's: %q %v", where, args)
	}
	many := make([]string, maxInstanceTypeValues+1)
	for i := range many {
		many[i] = "t" + strings.Repeat("x", i)
	}
	for name, values := range map[string][]string{
		"too many values": many,
		"too long":        {strings.Repeat("a", maxInstanceTypeRunes+1)},
		"a NUL byte":      {"a\x00b"},
	} {
		if _, err := parseInstanceListFilter(url.Values{"type": values}); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	if filter, err := parseInstanceListFilter(url.Values{}); err != nil || len(filter.Types) != 0 {
		t.Fatalf("no type means no filter: %v %v", filter, err)
	}
}
