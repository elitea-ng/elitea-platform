package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type recoveryResultStub struct {
	data  []byte
	calls int
	claim ContentClaim
}

func (s *recoveryResultStub) ReadNodeRecoveryResult(_ context.Context, c ContentClaim, _ string, _ string) ([]byte, error) {
	s.calls++
	s.claim = c
	return append([]byte(nil), s.data...), nil
}
func TestNodeRecoveryResultTransportRequiresMTLSAndExactBytes(t *testing.T) {
	data := []byte(`{"schema":"fixture"}`)
	hash := sha256.Sum256(data)
	version := hex.EncodeToString(hash[:])
	store := &recoveryResultStub{data: data}
	s, err := NewContentServer(contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
		t.Fatal("ordinary input authority used")
		return ContentAuthorization{}, nil
	}), contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
		t.Fatal("input opened")
		return nil, nil
	}), 8192)
	if err != nil {
		t.Fatal(err)
	}
	s.WithNodeRecoveryResults(store)
	for _, tc := range []struct {
		name, body, version, query string
		noTLS                      bool
		status                     int
	}{{name: "exact result", status: 200}, {name: "body", body: "{}", status: 400}, {name: "query", query: "credential=x", status: 400}, {name: "no TLS", noTLS: true, status: 403}, {name: "version digest mismatch", version: strings.Repeat("9", 64), status: 409}} {
		t.Run(tc.name, func(t *testing.T) {
			v := tc.version
			if v == "" {
				v = version
			}
			r := validContentRequest(t)
			r.Method = http.MethodPost
			r.URL.Path = "/executions/0123456789abcdef0123456789abcdef/generations/1/node-recovery/results/" + strings.Repeat("3", 64) + "/versions/" + v
			r.URL.RawQuery = tc.query
			r.Body = io.NopCloser(strings.NewReader(tc.body))
			r.ContentLength = int64(len(tc.body))
			if tc.noTLS {
				r.TLS = nil
			}
			w := httptest.NewRecorder()
			s.Routes().ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatal(w.Code, w.Body.String())
			}
			if w.Code == 200 && (w.Body.String() != string(data) || w.Header().Get("X-Elitea-Content-SHA256") != version || w.Header().Get("Cache-Control") != "no-store") {
				t.Fatal(w.Header(), w.Body.String())
			}
		})
	}
	if store.calls != 2 || store.claim.ClaimID != "claim-1" || store.claim.Generation != 1 {
		t.Fatal(store)
	}
}
