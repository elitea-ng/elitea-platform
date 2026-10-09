package eliteacore_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

// injectionProjectIDs are project ids whose text left the identifier when the
// handlers built the schema with fmt.Sprintf("p_%s", projectID) and quoted it
// with %q.
//
// %q writes an embedded double quote as \" . PostgreSQL treats the backslash
// inside a quoted identifier as an ordinary character and ends the identifier
// at the quote, so every id below became SQL rather than a schema name. Only
// the stray backslash kept the statement from running, and that is a property
// of %q rather than a decision. See issue #543.
var injectionProjectIDs = []string{
	`1".configuration, centry.project x --`,
	`1"; DROP TABLE centry.project; --`,
	`1".configuration UNION SELECT * FROM centry.secrets --`,
	`1"`,
	`1\"`,
	"abc",
	"-1",
	"1 OR 1=1",
}

// schemaFirstHandlers are the handlers that resolve the tenant schema before
// they touch the pool. A nil pool is therefore safe here: a refused project id
// never reaches a query.
func schemaFirstHandlers(h *eliteacore.Handler) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"SearchOptions":       h.SearchOptions,
		"ChatConfig":          h.ChatConfig,
		"AgentCategories":     h.AgentCategories,
		"ListCollections":     h.ListCollections,
		"Recommendations":     h.Recommendations,
		"ApplicationRelation": h.ApplicationRelation,
		"GetCollection":       h.GetCollection,
	}
}

// TestHandlersRefuseAProjectIDThatLeavesTheIdentifier is the handler-level
// regression test for issue #543.
//
// Before the correction each handler interpolated the id below into the
// statement and asked PostgreSQL about it. The handler now refuses the request
// and the id reaches no statement at all.
//
// It fails without the correction: with a nil pool the old handlers reached
// h.pool.Query and panicked on the nil dereference instead of answering 400.
func TestHandlersRefuseAProjectIDThatLeavesTheIdentifier(t *testing.T) {
	for name, handler := range schemaFirstHandlers(newHandler()) {
		for _, projectID := range injectionProjectIDs {
			t.Run(name+"/"+projectID, func(t *testing.T) {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Fatalf("%s reached the database with project id %q: %v",
							name, projectID, recovered)
					}
				}()

				w := httptest.NewRecorder()
				handler(w, newRequest(http.MethodGet, "/", map[string]string{
					"projectID":     projectID,
					"applicationID": "1",
					"versionID":     "1",
					"collectionID":  "1",
				}, nil))

				assertStatus(t, w, http.StatusBadRequest)

				body := w.Body.String()
				// The refusal must not echo the caller's text back, and it must
				// not carry a SQL error naming a relation or a schema.
				for _, leak := range []string{projectID, "p_", "SQLSTATE", "relation", "syntax error"} {
					if strings.Contains(body, leak) {
						t.Errorf("%s refusal body %q discloses %q", name, body, leak)
					}
				}
			})
		}
	}
}

// TestHandlersAcceptARealProjectID proves the refusal is not simply "refuse
// everything": a plain decimal project id still reaches the query, which with
// a nil pool means it gets past validation rather than being answered 400.
func TestHandlersAcceptARealProjectID(t *testing.T) {
	for name, handler := range schemaFirstHandlers(newHandler()) {
		t.Run(name, func(t *testing.T) {
			defer func() {
				// A nil-pool panic here is the proof: validation let the id
				// through and the handler went on to query.
				_ = recover()
			}()

			w := httptest.NewRecorder()
			handler(w, newRequest(http.MethodGet, "/", map[string]string{
				"projectID":     "1",
				"applicationID": "1",
				"versionID":     "1",
				"collectionID":  "1",
			}, nil))

			if w.Code == http.StatusBadRequest {
				t.Errorf("%s refused project id 1, which is a real project id: %s", name, w.Body.String())
			}
		})
	}
}
