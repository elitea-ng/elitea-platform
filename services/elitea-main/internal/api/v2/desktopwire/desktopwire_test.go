package desktopwire

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPositiveIDIsCanonical(t *testing.T) {
	for raw, want := range map[string]bool{
		"1": true, "2147483647": true, "0": false, "-1": false, "007": false, "+7": false,
		"2147483648": false, "1e3": false, "": false,
	} {
		if _, ok := PositiveID(raw); ok != want {
			t.Errorf("PositiveID(%q) = %v, want %v", raw, ok, want)
		}
	}
}

func TestWriteErrorIsUncachedJSON(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteError(recorder, http.StatusConflict, "code", "message")
	if recorder.Code != http.StatusConflict || recorder.Header().Get("Cache-Control") != "no-store" ||
		recorder.Header().Get("Content-Type") != "application/json" ||
		recorder.Body.String() != "{\"error\":\"code\",\"message\":\"message\"}\n" {
		t.Fatalf("answer = %d %v %q", recorder.Code, recorder.Header(), recorder.Body)
	}
}
