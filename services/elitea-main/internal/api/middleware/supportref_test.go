package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
	"github.com/elitea-ng/elitea-platform/libs/go/faults/logx"
)

var serverRef = regexp.MustCompile(`^sr-[0-9a-f]{16}$`)

// FU-299 C1 (delivery gate §3a, forged identity): a client cannot choose,
// forge or inject the support reference. A request carrying X-Request-Id and
// X-Elitea-Support-Ref gets a fresh server reference, and the supplied
// values never appear as support_ref in the response or the log.
func TestTheSupportReferenceIsAlwaysGeneratedByTheServer(t *testing.T) {
	var logs bytes.Buffer
	logger := logx.New(&logs, logx.Options{})
	var seen string
	handler := RequestID(SupportRef(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = faults.SupportRef(r.Context())
		logger.InfoContext(r.Context(), "handled")
	})))
	request := httptest.NewRequest(http.MethodGet, "/api/v2/anything", nil)
	forged := "sr-0000000000000000\n{\"level\":\"ERROR\"}"
	request.Header.Set("X-Request-Id", forged)
	request.Header.Set("X-Elitea-Support-Ref", "sr-0000000000000000")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	got := recorder.Header().Get("X-Elitea-Support-Ref")
	if !serverRef.MatchString(got) || got == "sr-0000000000000000" || got != seen {
		t.Fatalf("support ref = %q (handler saw %q), want a fresh server reference", got, seen)
	}
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &line); err != nil {
		t.Fatal(err)
	}
	if line["support_ref"] != got {
		t.Fatalf("logged support_ref = %v, want %q", line["support_ref"], got)
	}
	if strings.Contains(logs.String(), "sr-0000000000000000") {
		t.Fatalf("a client-supplied value reached the log: %s", logs.String())
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/", nil))
	if second.Header().Get("X-Elitea-Support-Ref") == got {
		t.Fatal("two requests got the same support reference")
	}
}

func TestAPanicIsOneFaultLineAndAJSON500WithoutTheValue(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(logx.New(&logs, logx.Options{}))
	defer slog.SetDefault(previous)

	handler := SupportRef(Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret token=abc for ada@example.com")
	})))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v2/things", nil))

	ref := recorder.Header().Get("X-Elitea-Support-Ref")
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", recorder.Body.String(), err)
	}
	if recorder.Code != http.StatusInternalServerError || body["code"] != "internal.panic" || body["support_ref"] != ref || len(body) != 3 {
		t.Fatalf("answer = %d %v", recorder.Code, body)
	}
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &line); err != nil {
		t.Fatal(err)
	}
	if line["code"] != "internal.panic" || line["support_ref"] != ref || line["method"] != "POST" || line["stack"] == nil {
		t.Fatalf("log line = %v", line)
	}
	for _, leak := range []string{"secret", "ada@example.com"} {
		if strings.Contains(logs.String(), leak) || strings.Contains(recorder.Body.String(), leak) {
			t.Fatalf("the panic value leaked: %q", leak)
		}
	}
}
