package s3

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// GetBucketEncryption has three answers: a rule (confirmed), no rule
// (refused, and no attestation overrides it), and a backend that does not
// implement the call (unreportable, which the operator may attest).
func TestDefaultEncryptionConfirmedMapsTheBackendAnswer(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		confirmed  bool
		unreported bool
		otherwise  bool
	}{
		{"a default rule", http.StatusOK, `<?xml version="1.0" encoding="UTF-8"?>
<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`, true, false, false},
		{"no rule", http.StatusNotFound, `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>ServerSideEncryptionConfigurationNotFoundError</Code><Message>none</Message></Error>`, false, false, false},
		{"not implemented", http.StatusNotImplemented, `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NotImplemented</Code><Message>no</Message></Error>`, false, true, false},
		{"a bare 501", http.StatusNotImplemented, ``, false, true, false},
		{"access denied", http.StatusForbidden, `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>no</Message></Error>`, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			backend := checksumBackend(t, server.URL, "physical")
			confirmed, err := backend.DefaultEncryptionConfirmed(context.Background())
			if confirmed != tc.confirmed {
				t.Fatalf("confirmed = %t, want %t (err %v)", confirmed, tc.confirmed, err)
			}
			if got := errors.Is(err, storage.ErrEncryptionUnreportable); got != tc.unreported {
				t.Fatalf("unreportable = %t, want %t (err %v)", got, tc.unreported, err)
			}
			if (err != nil && !tc.unreported) != tc.otherwise {
				t.Fatalf("err = %v, want an error %t", err, tc.otherwise)
			}
		})
	}
}
