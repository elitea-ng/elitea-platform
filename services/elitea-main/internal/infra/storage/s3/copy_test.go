package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// Copy is one CopyObject inside the bucket, asking for a SHA-256 checksum,
// and reports the copy's full-object digest. The bytes never pass through
// Main.
func TestCopyIsOneServerSideCopyObject(t *testing.T) {
	digest := sha256.Sum256([]byte("segment"))
	var mu sync.Mutex
	var method, path, source, algorithm string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		method, path = r.Method, r.URL.Path
		source, algorithm = r.Header.Get("x-amz-copy-source"), r.Header.Get("x-amz-checksum-algorithm")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><CopyObjectResult><ETag>"e"</ETag>` +
			`<ChecksumSHA256>` + base64.StdEncoding.EncodeToString(digest[:]) + `</ChecksumSHA256>` +
			`<ChecksumType>FULL_OBJECT</ChecksumType></CopyObjectResult>`))
	}))
	defer server.Close()
	backend := checksumBackend(t, server.URL, "physical")
	src, _ := storage.NewObjectRef("7", "elitea-workspaces", "t/seg/a.seg.upload-1")
	dst, _ := storage.NewObjectRef("7", "elitea-workspaces", "t/seg/a.seg")
	info, err := storage.CopyObject(context.Background(), storage.Instrument(backend, "s3"), src, dst)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if method != http.MethodPut || !strings.HasSuffix(path, dst.StorageKey("")) ||
		!strings.HasPrefix(source, "physical/") || !strings.HasSuffix(source, src.StorageKey("")) {
		t.Fatalf("request = %s %s, copy source %q", method, path, source)
	}
	if !strings.EqualFold(algorithm, "SHA256") {
		t.Fatalf("x-amz-checksum-algorithm = %q, want SHA256", algorithm)
	}
	if !bytes.Equal(info.DigestSHA256, digest[:]) {
		t.Fatalf("copy digest = %x", info.DigestSHA256)
	}
}
