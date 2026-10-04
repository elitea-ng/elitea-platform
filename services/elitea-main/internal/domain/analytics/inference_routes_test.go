package analytics

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// gatewayRouteClasses is the gateway's checked-in class of every route it
// mounts. The gateway's own test walks its chi router against this file, so
// the file cannot fall behind the router.
var gatewayRouteClasses = filepath.Join("..", "..", "..", "..",
	"elitea-llm-gateway", "internal", "api", "testdata", "llm_route_classes.txt")

// The inference list here is a hand copy of the gateway's model-call routes,
// and the gateway module is outside go.work, so nothing else ties the two. A
// cost-incurring route the gateway adds and this list misses disappears from
// every analytics call count while the ledger still bills it. A route this
// list names that the gateway does not class as inference counts something
// that is not a model call.
//
// The test FAILS, and does not skip, when the file is absent: both modules
// live in one repository, so an absent file means it moved, and a skip would
// report the drift check as green while it checked nothing.
func TestInferenceRoutesMatchTheGatewayRouteClasses(t *testing.T) {
	source, err := os.ReadFile(gatewayRouteClasses)
	if err != nil {
		t.Fatalf("read the gateway route classes: %v", err)
	}
	gateway := map[string]bool{}
	for _, line := range strings.Split(string(source), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "inference" {
			gateway[fields[2]] = true
		}
	}
	if len(gateway) == 0 {
		t.Fatal("the gateway route classes name no inference route; the comparison measures nothing")
	}

	ours := map[string]bool{}
	for _, route := range InferenceRoutes() {
		ours[route] = true
	}
	var missing, extra []string
	for route := range gateway {
		if !ours[route] {
			missing = append(missing, route)
		}
	}
	for route := range ours {
		if !gateway[route] {
			extra = append(extra, route)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	for _, route := range missing {
		t.Errorf("the gateway classes %s as inference, and InferenceRouteSQLList does not count it", route)
	}
	for _, route := range extra {
		t.Errorf("InferenceRouteSQLList counts %s, and the gateway does not class it as inference", route)
	}
}

// The list is written into SQL text, so every entry must be one quoted route
// pattern and nothing else.
func TestInferenceRouteSQLListIsWellFormed(t *testing.T) {
	entry := regexp.MustCompile(`^'/llm/v1/[a-z/_]+'$`)
	for _, part := range strings.Split(InferenceRouteSQLList, ",") {
		if !entry.MatchString(strings.TrimSpace(part)) {
			t.Errorf("entry %q is not one quoted /llm/v1 route pattern", part)
		}
	}
}

// Embeddings are model calls (legacy issue 6879: the Usage page counted them
// and Analytics must too). The operator and catalogue routes are not.
func TestInferenceRoutesIncludeEmbeddingsAndExcludeOperatorRoutes(t *testing.T) {
	routes := map[string]bool{}
	for _, route := range InferenceRoutes() {
		routes[route] = true
	}
	for _, want := range []string{"/llm/v1/chat/completions", "/llm/v1/embeddings", "/llm/v1/images/generations", "/llm/v1/messages"} {
		if !routes[want] {
			t.Errorf("%s is a model call and is missing", want)
		}
	}
	for _, excluded := range []string{
		"/llm/v1/models", "/llm/v1/models/*", "/llm/v1/messages/count_tokens", "/llm/v1/messages/*",
		"/llm/v1/check_connection", "/llm/v1/list_provider_models", "/llm/v1/list_provider_voices", "(unmatched)",
	} {
		if routes[excluded] {
			t.Errorf("%s is not a model call and must not be counted", excluded)
		}
	}
}
