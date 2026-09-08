package routes_test

// Table.Poll — the hook a facade uses to see the terminal payload the browser
// drains. DeepWiki's wiki chat records the answer there, because that is the
// only moment a proxying facade ever sees one.

import (
	"net/http"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/routes"
)

// It replaces the hop on GET-invocation ALONE. The other three routes must
// stay where they were: /slots and the invoke are not that facade's business
// to observe, and the DELETE on this same path is a cancel — no answer to
// record, and a different permission in front of it.
func TestPollReplacesTheHopOnGetInvocationOnly(t *testing.T) {
	var plain []forwarded
	var polled []forwarded
	tbl := table(&plain, nil)
	tbl.Poll = func(w http.ResponseWriter, r *http.Request, providerPath, projectID, userID string) {
		polled = append(polled, forwarded{providerPath, projectID, userID, r.Method})
		w.WriteHeader(http.StatusOK)
	}
	handler, err := routes.Build(tbl)
	if err != nil {
		t.Fatal(err)
	}

	if w := do(handler, http.MethodGet, "/x/invocations/1/Wikis/ask/inv-1", "all"); w.Code != http.StatusOK {
		t.Fatalf("polling answered %d", w.Code)
	}
	if len(polled) != 1 || polled[0].path != "/tools/Wikis/ask/invocations/inv-1" {
		t.Fatalf("the poll hook saw %+v", polled)
	}
	if len(polled) != 1 || polled[0].project != "1" || polled[0].user != "7" {
		t.Fatalf("the poll hook was handed %+v, want project 1 user 7", polled)
	}
	if len(plain) != 0 {
		t.Fatalf("the plain hop also ran for a poll: %+v", plain)
	}

	polled = nil
	if w := do(handler, http.MethodGet, "/x/slots/1", "all"); w.Code != http.StatusOK {
		t.Fatalf("slots answered %d", w.Code)
	}
	if w := do(handler, http.MethodDelete, "/x/invocations/1/Wikis/ask/inv-1", "all"); w.Code != http.StatusOK {
		t.Fatalf("cancelling answered %d", w.Code)
	}
	if len(polled) != 0 {
		t.Fatalf("the poll hook ran for routes it does not own: %+v", polled)
	}
	if len(plain) != 2 {
		t.Fatalf("the plain hop ran %d times, want 2", len(plain))
	}
}

// A table with no Poll is unchanged — every facade that does not record a
// transcript is one, and the Inventory facade is one today.
func TestWithoutAPollHookEveryRouteStillTakesThePlainHop(t *testing.T) {
	var plain []forwarded
	handler, err := routes.Build(table(&plain, nil))
	if err != nil {
		t.Fatal(err)
	}
	if w := do(handler, http.MethodGet, "/x/invocations/1/Wikis/ask/inv-1", "all"); w.Code != http.StatusOK {
		t.Fatalf("polling answered %d", w.Code)
	}
	if len(plain) != 1 {
		t.Fatalf("the plain hop ran %d times, want 1", len(plain))
	}
}
