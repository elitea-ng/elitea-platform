package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// partRecorder is a minimal S3 multipart endpoint that remembers which
// checksum headers the upload's requests carried.
type partRecorder struct {
	mu        sync.Mutex
	initiate  string   // CreateMultipartUpload's x-amz-checksum-algorithm, or ""
	checksums []string // one entry per UploadPart: its x-amz-checksum-* header names, or ""
}

func (p *partRecorder) recorded() (string, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.initiate, append([]string(nil), p.checksums...)
}

func (p *partRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	query := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && query.Has("uploads"):
		p.mu.Lock()
		p.initiate = r.Header.Get("X-Amz-Checksum-Algorithm")
		p.mu.Unlock()
		_, _ = fmt.Fprint(w, `<InitiateMultipartUploadResult><Bucket>physical</Bucket><Key>k</Key><UploadId>u1</UploadId></InitiateMultipartUploadResult>`)
	case r.Method == http.MethodPut && query.Has("partNumber"):
		var names []string
		for name := range r.Header {
			if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-amz-checksum-") {
				names = append(names, lower)
			}
		}
		for _, trailer := range r.Header.Values("X-Amz-Trailer") {
			names = append(names, "trailer:"+strings.ToLower(trailer))
		}
		p.mu.Lock()
		p.checksums = append(p.checksums, strings.Join(names, ","))
		p.mu.Unlock()
		w.Header().Set("ETag", `"etag-`+query.Get("partNumber")+`"`)
	case r.Method == http.MethodPost && query.Has("uploadId"):
		_, _ = fmt.Fprint(w, `<CompleteMultipartUploadResult><Bucket>physical</Bucket><Key>k</Key><ETag>"done"</ETag></CompleteMultipartUploadResult>`)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusBadRequest)
	}
}

// A non-seekable Put goes through the transfer manager, which ignores the
// client's checksum setting unless handed it. With
// AWS_REQUEST_CHECKSUM_CALCULATION=when_required its parts carry no
// checksum; by default they still do.
func TestStreamedPutPartsFollowTheClientChecksumSetting(t *testing.T) {
	for _, tc := range []struct {
		setting      string
		wantChecksum bool
	}{
		{"", true},
		{"when_required", false},
	} {
		t.Run("setting="+tc.setting, func(t *testing.T) {
			t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", tc.setting)
			// The SDK also reads the shared config files; keep the
			// developer's profile out of it.
			missing := t.TempDir() + "/none"
			t.Setenv("AWS_CONFIG_FILE", missing)
			t.Setenv("AWS_SHARED_CREDENTIALS_FILE", missing)
			t.Setenv("AWS_PROFILE", "")
			recorder := &partRecorder{}
			server := httptest.NewServer(recorder)
			defer server.Close()

			body := io.NopCloser(bytes.NewReader(make([]byte, 20<<20)))
			if _, err := checksumBackend(t, server.URL, "physical").Put(context.Background(), checksumRef(t), body, storage.PutOptions{ContentLength: -1}); err != nil {
				t.Fatalf("Put: %v", err)
			}
			initiate, checksums := recorder.recorded()
			if len(checksums) < 2 {
				t.Fatalf("want a multipart upload, got %d part(s)", len(checksums))
			}
			if got := initiate != ""; got != tc.wantChecksum {
				t.Errorf("CreateMultipartUpload declared checksum %q, want checksum=%v", initiate, tc.wantChecksum)
			}
			for i, names := range checksums {
				if got := names != ""; got != tc.wantChecksum {
					t.Errorf("part %d checksum headers %q, want checksum=%v", i+1, names, tc.wantChecksum)
				}
			}
		})
	}
}
