package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Issue 1968: a personal access token name is at most 128 characters (runes),
// not bytes, and the 400 names the field and the limit.
func TestTokenCreateNameLimit(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  int
	}{
		{"128 ascii", strings.Repeat("a", 128), http.StatusCreated},
		{"129 ascii", strings.Repeat("a", 129), http.StatusBadRequest},
		{"128 multibyte (512 bytes)", strings.Repeat("я", 128), http.StatusCreated},
		{"129 multibyte", strings.Repeat("я", 129), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := &Handler{
				tokenSigningKey: []byte("test-secret"),
				tokens: tokenRepositoryStub{
					createFunc: func(context.Context, tokenCreateInput) (tokenRecord, error) {
						return tokenRecord{}, nil
					},
				},
			}
			payload, _ := json.Marshal(map[string]any{"name": tc.value})
			req := authenticatedTokenRequest(http.MethodPost, "/token/", string(payload))
			rec := httptest.NewRecorder()
			handler.Routes().ServeHTTP(rec, req)
			if tc.want == http.StatusBadRequest {
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
				}
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body["field"] != "name" || body["max_length"] != float64(128) {
					t.Fatalf("400 must name the field and limit, got %v", body)
				}
				return
			}
			if rec.Code == http.StatusBadRequest {
				t.Fatalf("a name at the limit must not be refused, body = %s", rec.Body.String())
			}
		})
	}
}
