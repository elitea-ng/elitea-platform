package folders_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Issue 1968: a folder name is at most 128 characters (runes), on create and
// on both update verbs, and the 400 names the field and the limit.
func TestFolderNameLimit(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		value  string
		want   int
	}{
		{"create 128", http.MethodPost, "/api/v2/projects/proj-1/folders/", strings.Repeat("a", 128), http.StatusCreated},
		{"create 129", http.MethodPost, "/api/v2/projects/proj-1/folders/", strings.Repeat("a", 129), http.StatusBadRequest},
		{"create 128 multibyte", http.MethodPost, "/api/v2/projects/proj-1/folders/", strings.Repeat("я", 128), http.StatusCreated},
		{"create 129 multibyte", http.MethodPost, "/api/v2/projects/proj-1/folders/", strings.Repeat("я", 129), http.StatusBadRequest},
		{"put 129", http.MethodPut, "/api/v2/projects/proj-1/folders/f-1", strings.Repeat("a", 129), http.StatusBadRequest},
		{"put 128", http.MethodPut, "/api/v2/projects/proj-1/folders/f-1", strings.Repeat("a", 128), http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &mockFolderRepo{}
			r := setupFoldersRouter(repo)
			payload, _ := json.Marshal(map[string]any{"name": tc.value})
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(payload))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != http.StatusBadRequest {
				return
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["field"] != "name" || body["max_length"] != float64(128) {
				t.Fatalf("400 must name the field and limit, got %v", body)
			}
		})
	}
}
