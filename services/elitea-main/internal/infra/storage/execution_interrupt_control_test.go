package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type interruptControlStub struct {
	fetches, acks int
	err           error
	body          []byte
	ack           []byte
}

func (s *interruptControlStub) FetchExecutionInterrupts(context.Context, ContentClaim) ([]byte, error) {
	s.fetches++
	return s.body, s.err
}

func (s *interruptControlStub) AckExecutionInterrupt(_ context.Context, _ ContentClaim, raw []byte) ([]byte, error) {
	s.acks++
	s.ack = raw
	return s.body, s.err
}

func interruptServer(t *testing.T, control ExecutionInterruptControlStore) http.Handler {
	t.Helper()
	s, err := NewContentServer(contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
		t.Fatal("ordinary content authority crossed")
		return ContentAuthorization{}, nil
	}), contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
		t.Fatal("ordinary input opened")
		return nil, nil
	}), 8192)
	if err != nil {
		t.Fatal(err)
	}
	return s.WithExecutionInterrupts(control).Routes()
}

func interruptRequest(t *testing.T, route, body string) *http.Request {
	req := validContentRequest(t)
	req.Method = http.MethodPost
	req.URL.Path = "/executions/0123456789abcdef0123456789abcdef/generations/1/interrupts/" + route
	req.Body = io.NopCloser(strings.NewReader(body))
	req.ContentLength = int64(len(body))
	return req
}

// TestExecutionInterruptRoutesMapTypedRefusalsToFixedStatuses pins the
// contract v1.1 §7.1 status table and that no error body echoes ledger data.
func TestExecutionInterruptRoutesMapTypedRefusalsToFixedStatuses(t *testing.T) {
	for _, spec := range []struct {
		err    error
		status int
	}{
		{nil, http.StatusOK},
		{ErrInterruptClaimRefused, http.StatusForbidden},
		{ErrInterruptRequestInvalid, http.StatusBadRequest},
		{ErrInterruptNotFound, http.StatusNotFound},
		{ErrInterruptConflict, http.StatusConflict},
		{fmt.Errorf("wrapped: %w", ErrInterruptConflict), http.StatusConflict},
		{errors.New("database is down: secret-ish detail"), http.StatusServiceUnavailable},
	} {
		for _, route := range []string{"fetch", "ack"} {
			control := &interruptControlStub{err: spec.err, body: []byte(`{"schema":"x"}`)}
			body := ""
			if route == "ack" {
				body = `{"interrupt_key":"k"}`
			}
			w := httptest.NewRecorder()
			interruptServer(t, control).ServeHTTP(w, interruptRequest(t, route, body))
			if w.Code != spec.status {
				t.Fatalf("%s %v: %d", route, spec.err, w.Code)
			}
			if spec.err == nil && (w.Body.String() != `{"schema":"x"}` || w.Header().Get("Cache-Control") != "no-store") {
				t.Fatalf("%s success body %q headers %v", route, w.Body.String(), w.Header())
			}
			if spec.err != nil && strings.Contains(w.Body.String(), "secret-ish") {
				t.Fatalf("%s error body leaks: %q", route, w.Body.String())
			}
		}
	}
}

func TestExecutionInterruptRoutesBoundTheRequestBeforeTheStore(t *testing.T) {
	control := &interruptControlStub{body: []byte(`{}`)}
	routes := interruptServer(t, control)
	cases := map[string]struct {
		req    *http.Request
		status int
	}{
		"fetch with a body":     {interruptRequest(t, "fetch", "{}"), http.StatusBadRequest},
		"ack without a body":    {interruptRequest(t, "ack", ""), http.StatusBadRequest},
		"ack over 1024 bytes":   {interruptRequest(t, "ack", strings.Repeat(" ", MaxExecutionInterruptAckBytes+1)), http.StatusBadRequest},
		"ack at 1024 bytes":     {interruptRequest(t, "ack", strings.Repeat(" ", MaxExecutionInterruptAckBytes)), http.StatusOK},
		"no client certificate": {func() *http.Request { r := interruptRequest(t, "fetch", ""); r.TLS = nil; return r }(), http.StatusForbidden},
		"no fence header":       {func() *http.Request { r := interruptRequest(t, "fetch", ""); r.Header.Del(fenceHeader); return r }(), http.StatusForbidden},
		"generation not decimal": {func() *http.Request {
			r := interruptRequest(t, "fetch", "")
			r.URL.Path = strings.Replace(r.URL.Path, "/1/", "/01/", 1)
			return r
		}(), http.StatusForbidden},
		"query string": {func() *http.Request { r := interruptRequest(t, "ack", "{}"); r.URL.RawQuery = "a=b"; return r }(), http.StatusBadRequest},
	}
	for name, spec := range cases {
		before := control.fetches + control.acks
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, spec.req)
		if w.Code != spec.status {
			t.Errorf("%s: %d", name, w.Code)
		}
		if spec.status != http.StatusOK && control.fetches+control.acks != before {
			t.Errorf("%s reached the store", name)
		}
	}
	if !bytes.Equal(control.ack, bytes.Repeat([]byte(" "), MaxExecutionInterruptAckBytes)) {
		t.Fatal("the store did not receive the exact ACK bytes")
	}
}

func TestExecutionInterruptRoutesAreUnregisteredWithoutAStore(t *testing.T) {
	w := httptest.NewRecorder()
	interruptServer(t, nil).ServeHTTP(w, interruptRequest(t, "fetch", ""))
	if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("route without a store: %d", w.Code)
	}
}
