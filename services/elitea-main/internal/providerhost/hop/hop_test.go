package hop_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/hop"
)

// The message has to name the setting an operator would edit and the value
// it holds: "invalid target" tells an operator with twenty-odd variables
// nothing about which one to look at.
func TestARefusedTargetNamesTheSettingAndTheValue(t *testing.T) {
	err := hop.ParseGRPCTarget("https://elitea-deepwiki:8443", "ELITEA_DEEPWIKI_PLATFORM_GRPC_TARGET")
	if err == nil {
		t.Fatal("a URL was accepted as a gRPC target")
	}
	if !errors.Is(err, hop.ErrInvalidTarget) {
		t.Errorf("error is not ErrInvalidTarget: %v", err)
	}
	if !strings.Contains(err.Error(), "ELITEA_DEEPWIKI_PLATFORM_GRPC_TARGET") {
		t.Errorf("the message does not name the setting: %v", err)
	}
	if !strings.Contains(err.Error(), "https://elitea-deepwiki:8443") {
		t.Errorf("the message does not name the value: %v", err)
	}
}

// A gRPC target is host:port and nothing else. What the retired BASE_URL
// held (a URL, possibly with a path) is the likeliest thing an operator
// carries over, so each shape of it is refused at construction.
func TestOnlyHostPortIsAGRPCTarget(t *testing.T) {
	for _, raw := range []string{"", "elitea-deepwiki", ":8443", "elitea-deepwiki:", "https://elitea-deepwiki:8443", "elitea-deepwiki:8443/base"} {
		if err := hop.ParseGRPCTarget(raw, "X"); err == nil {
			t.Errorf("ParseGRPCTarget(%q) accepted it", raw)
		}
	}
	for _, raw := range []string{"elitea-deepwiki:8443", "10.0.0.1:9443", "[::1]:9443"} {
		if err := hop.ParseGRPCTarget(raw, "X"); err != nil {
			t.Errorf("ParseGRPCTarget(%q) = %v, want accepted", raw, err)
		}
	}
}

// deadlineRecorder reports whether the deadline was cleared, and can refuse
// the way a ResponseWriter without deadline support does.
type deadlineRecorder struct {
	http.ResponseWriter
	cleared   bool
	supported bool
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	if !d.supported {
		return http.ErrNotSupported
	}
	if t.IsZero() {
		d.cleared = true
	}
	return nil
}

func TestTheWriteDeadlineIsCleared(t *testing.T) {
	recorder := &deadlineRecorder{ResponseWriter: httptest.NewRecorder(), supported: true}
	hop.ClearWriteDeadline(recorder, nil)
	if !recorder.cleared {
		t.Fatal("the write deadline was not cleared; a WriteTimeout would truncate a long response mid-stream")
	}
}

// A ResponseWriter that cannot set a deadline is not an error: the deadline it
// cannot set is one it does not have. httptest.ResponseRecorder is exactly
// this, so treating ErrNotSupported as a failure would make every test that
// proxies through a recorder log a warning.
func TestAWriterWithoutDeadlineSupportIsNotAnError(t *testing.T) {
	recorder := &deadlineRecorder{ResponseWriter: httptest.NewRecorder(), supported: false}
	hop.ClearWriteDeadline(recorder, nil)
	if recorder.cleared {
		t.Fatal("the recorder reported a clear it said it did not support")
	}
}
