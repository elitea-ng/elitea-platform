package configurations

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestConfigurationIDsQueryAndBoundPredicate(t *testing.T) {
	request := parseConfigurationListQuery(url.Values{"ids": {"3,2,3"}, "type": {"github"}, "query": {"team"}, "limit": {"1"}})
	if !configurationListQueryInBounds(request) || request.limit != 1 {
		t.Fatalf("request: %#v", request)
	}
	clause, args := configurationRowFilter(request, 2)
	if !strings.Contains(clause, "id = ANY($2::integer[])") || !strings.Contains(clause, "type = ANY($3)") || !strings.Contains(clause, "$4") || !reflect.DeepEqual(args, []any{[]int32{3, 2}, []string{"github"}, "team"}) {
		t.Fatalf("predicate: %s %#v", clause, args)
	}
	for _, values := range []url.Values{{"ids": {"1,2", "3"}}, {"ids": {"bad"}}, {"ids": {"0"}}} {
		if configurationListQueryInBounds(parseConfigurationListQuery(values)) {
			t.Fatalf("accepted %v", values)
		}
	}
	for _, values := range []url.Values{{}, {"ids": {""}}} {
		clause, _ := configurationRowFilter(parseConfigurationListQuery(values), 1)
		if strings.Contains(clause, "id = ANY") {
			t.Fatalf("empty filter: %s", clause)
		}
	}
}
