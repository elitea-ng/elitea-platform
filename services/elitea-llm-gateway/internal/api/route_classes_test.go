package api

import (
	"bufio"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// routeClassesFile is the checked-in class of every mounted route. elitea-main
// reads the same file, so do not move it without its test there.
const routeClassesFile = "testdata/llm_route_classes.txt"

// readRouteClasses parses routeClassesFile into "METHOD pattern" -> class.
func readRouteClasses(t *testing.T) map[string]string {
	t.Helper()
	file, err := os.Open(routeClassesFile)
	if err != nil {
		t.Fatalf("open %s: %v", routeClassesFile, err)
	}
	defer func() { _ = file.Close() }()

	classes := map[string]string{}
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 3 {
			t.Fatalf("%s:%d: want <class> <METHOD> <pattern>, got %q", routeClassesFile, line, text)
		}
		class, key := fields[0], fields[1]+" "+fields[2]
		if class != "inference" && class != "other" {
			t.Fatalf("%s:%d: class %q is not inference or other", routeClassesFile, line, class)
		}
		if _, dup := classes[key]; dup {
			t.Fatalf("%s:%d: %s is listed twice", routeClassesFile, line, key)
		}
		classes[key] = class
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", routeClassesFile, err)
	}
	return classes
}

// TestEveryMountedRouteHasAClass walks the real router. A route the gateway
// mounts with no class here could be a new model call that the analytics pages
// in elitea-main never count, while the ledger still bills it. The test makes
// the author decide.
func TestEveryMountedRouteHasAClass(t *testing.T) {
	classes := readRouteClasses(t)

	routes, ok := testRouter().(chi.Routes)
	if !ok {
		t.Fatalf("NewRouter returned %T, which chi.Walk cannot walk", testRouter())
	}
	mounted := map[string]bool{}
	walk := func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		mounted[method+" "+route] = true
		return nil
	}
	if err := chi.Walk(routes, walk); err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if len(mounted) == 0 {
		t.Fatal("chi.Walk found no routes; the test measures nothing")
	}

	var unclassified, unmounted []string
	for route := range mounted {
		if _, ok := classes[route]; !ok {
			unclassified = append(unclassified, route)
		}
	}
	for route := range classes {
		if !mounted[route] {
			unmounted = append(unmounted, route)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(unmounted)
	for _, route := range unclassified {
		t.Errorf("%s is mounted and has no class in %s. Add it as inference or other; "+
			"an inference route also goes into elitea-main's analytics.InferenceRouteSQLList", route, routeClassesFile)
	}
	for _, route := range unmounted {
		t.Errorf("%s is in %s but the router does not mount it. Remove the line", route, routeClassesFile)
	}
}
