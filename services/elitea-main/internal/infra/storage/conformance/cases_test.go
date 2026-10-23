package conformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// runCases exercises every conformance case against store. Cases 1-11 run
// unconditionally; cases 12-14 are gated on Capabilities() and skip with a
// recorded reason when the backend reports the feature unavailable. Each
// case uses its own logical bucket name (a key-path segment, not a
// separately-provisioned physical resource — see conformance_test.go) so
// cases never collide with each other.
func runCases(t *testing.T, store storage.ObjectStore) {
	t.Helper()
	ctx := context.Background()
	caps := store.Capabilities()

	t.Run("PutThenStatMatchesSizeAndContentType", func(t *testing.T) {
		ref := ref(t, "case01", "object.txt")
		content := []byte("put-then-stat contents")
		if _, err := store.Put(ctx, ref, bytes.NewReader(content), storage.PutOptions{ContentLength: int64(len(content)), ContentType: "text/plain"}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		info, err := store.Stat(ctx, ref)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info.Size != int64(len(content)) {
			t.Errorf("Stat().Size = %d, want %d", info.Size, len(content))
		}
		if info.ContentType != "text/plain" {
			t.Errorf("Stat().ContentType = %q, want %q", info.ContentType, "text/plain")
		}
	})

	t.Run("GetMissingKeyIsErrNotFound", func(t *testing.T) {
		missing := ref(t, "case02", "does-not-exist.txt")
		_, _, err := store.Get(ctx, missing, nil)
		if !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Get(missing) err = %v, want errors.Is(err, ErrNotFound)", err)
		}
	})

	t.Run("DeleteMissingKeyReturnsNil", func(t *testing.T) {
		missing := ref(t, "case03", "does-not-exist.txt")
		if err := store.Delete(ctx, missing); err != nil {
			t.Fatalf("Delete(missing) err = %v, want nil", err)
		}
	})

	t.Run("ListWithKeyPrefixReturnsOnlyMatchingKeys", func(t *testing.T) {
		bucket := bucketRef(t, "case04")
		mustPut(t, store, "case04", "match/one.txt", []byte("a"))
		mustPut(t, store, "case04", "match/two.txt", []byte("b"))
		mustPut(t, store, "case04", "other/three.txt", []byte("c"))

		page, err := store.List(ctx, storage.ListQuery{Bucket: bucket, KeyPrefix: "match/"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page.Objects) != 2 {
			t.Fatalf("List returned %d objects, want 2: %+v", len(page.Objects), page.Objects)
		}
		for _, o := range page.Objects {
			if !strings.HasPrefix(o.Key, "match/") {
				t.Errorf("List returned key %q outside prefix %q", o.Key, "match/")
			}
		}
	})

	t.Run("ListWithDelimiterReturnsCommonPrefixes", func(t *testing.T) {
		bucket := bucketRef(t, "case05")
		mustPut(t, store, "case05", "dir/a.txt", []byte("a"))
		mustPut(t, store, "case05", "dir/b.txt", []byte("b"))
		mustPut(t, store, "case05", "dir/sub/c.txt", []byte("c"))

		page, err := store.List(ctx, storage.ListQuery{Bucket: bucket, KeyPrefix: "dir/", Delimiter: "/"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page.CommonPrefixes) != 1 || page.CommonPrefixes[0] != "dir/sub/" {
			t.Errorf("List CommonPrefixes = %v, want [\"dir/sub/\"]", page.CommonPrefixes)
		}
		for _, o := range page.Objects {
			if strings.HasPrefix(o.Key, "dir/sub/") {
				t.Errorf("List returned key %q below the delimiter, want it rolled up into CommonPrefixes", o.Key)
			}
		}
		wantKeys := map[string]bool{"dir/a.txt": true, "dir/b.txt": true}
		if len(page.Objects) != len(wantKeys) {
			t.Errorf("List returned %d objects, want %d (%v): got %+v", len(page.Objects), len(wantKeys), wantKeys, page.Objects)
		}
		for _, o := range page.Objects {
			if !wantKeys[o.Key] {
				t.Errorf("List returned unexpected key %q", o.Key)
			}
		}
	})

	t.Run("ListPaginatesWithNoDuplicatesOrGaps", func(t *testing.T) {
		bucket := bucketRef(t, "case06")
		want := map[string]bool{}
		for i := 0; i < 5; i++ {
			key := fmt.Sprintf("obj-%02d.txt", i)
			mustPut(t, store, "case06", key, []byte(key))
			want[key] = true
		}

		got := map[string]bool{}
		token := ""
		pages := 0
		for {
			page, err := store.List(ctx, storage.ListQuery{Bucket: bucket, MaxKeys: 2, ContinuationToken: token})
			if err != nil {
				t.Fatalf("List (page %d): %v", pages, err)
			}
			pages++
			if pages == 1 && !page.IsTruncated {
				t.Errorf("first page IsTruncated = false, want true (5 objects, MaxKeys 2)")
			}
			for _, o := range page.Objects {
				if got[o.Key] {
					t.Errorf("List returned duplicate key %q across pages", o.Key)
				}
				got[o.Key] = true
			}
			if !page.IsTruncated {
				break
			}
			token = page.NextContinuationToken
			if token == "" {
				t.Fatalf("page reported IsTruncated=true but NextContinuationToken is empty")
			}
			if pages > 10 {
				t.Fatalf("List did not terminate after %d pages", pages)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("List across pages returned %d unique keys, want %d: got=%v want=%v", len(got), len(want), got, want)
		}
		for k := range want {
			if !got[k] {
				t.Errorf("List across pages is missing key %q (a gap)", k)
			}
		}
	})

	t.Run("GetByteRangeReturnsExactSlice", func(t *testing.T) {
		content := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
		ref := ref(t, "case07", "ranged.txt")
		if _, err := store.Put(ctx, ref, bytes.NewReader(content), storage.PutOptions{ContentLength: int64(len(content))}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		body, info, err := store.Get(ctx, ref, &storage.ByteRange{Start: 10, End: 19})
		if err != nil {
			t.Fatalf("Get(range): %v", err)
		}
		got, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil {
			t.Fatalf("read range body: %v", err)
		}
		want := content[10:20]
		if len(got) != 10 {
			t.Fatalf("Get(range 10-19) returned %d bytes, want 10", len(got))
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("Get(range 10-19) = %q, want %q", got, want)
		}
		// The reported ObjectInfo was discarded here before. That is how the
		// GCS backend came to report the WHOLE object size for a ranged
		// read: the handler declared it as Content-Length, net/http saw a
		// short write, and every ranged download ended in an unexpected EOF.
		if info.Size != int64(len(got)) {
			t.Errorf("Get(range 10-19) reported Size %d, want the range length %d", info.Size, len(got))
		}
		if info.TotalSize != int64(len(content)) {
			t.Errorf("Get(range 10-19) reported TotalSize %d, want the whole object %d", info.TotalSize, len(content))
		}
	})

	t.Run("DeleteBatchOver1500KeysDeletesAllWithNoFailures", func(t *testing.T) {
		const n = 1500
		refs := make([]storage.ObjectRef, n)
		var wg sync.WaitGroup
		sem := make(chan struct{}, 32)
		for i := 0; i < n; i++ {
			r := ref(t, "case08", fmt.Sprintf("bulk-%04d.txt", i))
			refs[i] = r
			wg.Add(1)
			sem <- struct{}{}
			go func(r storage.ObjectRef) {
				defer wg.Done()
				defer func() { <-sem }()
				if _, err := store.Put(ctx, r, bytes.NewReader(nil), storage.PutOptions{ContentLength: 0}); err != nil {
					t.Errorf("Put(%s): %v", r.Key(), err)
				}
			}(r)
		}
		wg.Wait()
		if t.Failed() {
			t.Fatal("provisioning failed, aborting DeleteBatch case")
		}

		result, err := store.DeleteBatch(ctx, refs)
		if err != nil {
			t.Fatalf("DeleteBatch: %v", err)
		}
		if len(result.Failed) != 0 {
			t.Fatalf("DeleteBatch reported %d failures, want 0: %+v", len(result.Failed), result.Failed[:min(5, len(result.Failed))])
		}
		if len(result.Deleted) != n {
			t.Fatalf("DeleteBatch reported %d deleted, want %d", len(result.Deleted), n)
		}
	})

	t.Run("DeleteBatchWithMissingKeysSucceedsForPresentOnes", func(t *testing.T) {
		present := []storage.ObjectRef{
			ref(t, "case09", "present-1.txt"),
			ref(t, "case09", "present-2.txt"),
		}
		for _, r := range present {
			mustPut(t, store, "case09", r.Key(), []byte("x"))
		}
		missing := []storage.ObjectRef{
			ref(t, "case09", "missing-1.txt"),
			ref(t, "case09", "missing-2.txt"),
			ref(t, "case09", "missing-3.txt"),
		}

		result, err := store.DeleteBatch(ctx, append(append([]storage.ObjectRef{}, present...), missing...))
		if err != nil {
			t.Fatalf("DeleteBatch: %v", err)
		}
		deleted := map[string]bool{}
		for _, k := range result.Deleted {
			deleted[k] = true
		}
		for _, r := range present {
			if !deleted[r.Key()] {
				t.Errorf("DeleteBatch did not report %q as deleted", r.Key())
			}
		}
		if len(result.Failed) != 0 {
			t.Errorf("DeleteBatch reported %d failures for missing keys, want 0 (Delete is idempotent): %+v", len(result.Failed), result.Failed)
		}
	})

	t.Run("FiveMiBObjectRoundTripsWithMatchingDigest", func(t *testing.T) {
		content := randomBytes(t, 5<<20)
		wantSum := sha256.Sum256(content)

		ref := ref(t, "case10", "five-mib.bin")
		if _, err := store.Put(ctx, ref, bytes.NewReader(content), storage.PutOptions{ContentLength: int64(len(content))}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		body, info, err := store.Get(ctx, ref, nil)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		got, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if info.Size != int64(len(content)) {
			t.Errorf("Get info.Size = %d, want %d", info.Size, len(content))
		}
		gotSum := sha256.Sum256(got)
		if gotSum != wantSum {
			t.Fatalf("round-tripped SHA-256 mismatch (lengths equal=%v): got %x want %x", len(got) == len(content), gotSum, wantSum)
		}
	})

	t.Run("ZeroByteObjectRoundTrips", func(t *testing.T) {
		ref := ref(t, "case11", "empty.bin")
		if _, err := store.Put(ctx, ref, bytes.NewReader(nil), storage.PutOptions{ContentLength: 0}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		body, info, err := store.Get(ctx, ref, nil)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		got, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("Get returned %d bytes, want 0", len(got))
		}
		if info.Size != 0 {
			t.Fatalf("Get info.Size = %d, want 0", info.Size)
		}
	})

	t.Run("PresignGetExpiresAfterTTL", func(t *testing.T) {
		if !caps.Presign {
			t.Skip("Capabilities().Presign is false for this backend/credential path")
		}
		content := []byte("presigned get content")
		ref := ref(t, "case12", "presigned.txt")
		if _, err := store.Put(ctx, ref, bytes.NewReader(content), storage.PutOptions{ContentLength: int64(len(content))}); err != nil {
			t.Fatalf("Put: %v", err)
		}

		url, err := store.PresignGet(ctx, ref, 2*time.Second)
		if err != nil {
			t.Fatalf("PresignGet: %v", err)
		}

		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("unauthenticated GET of presigned URL: %v", err)
		}
		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("presigned GET before TTL expiry: status %d, want 200", resp.StatusCode)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("presigned GET body = %q, want %q", got, content)
		}

		time.Sleep(3 * time.Second)
		resp2, err := http.Get(url)
		if err != nil {
			t.Fatalf("unauthenticated GET of expired presigned URL: %v", err)
		}
		_ = resp2.Body.Close()
		if resp2.StatusCode != http.StatusForbidden {
			t.Fatalf("presigned GET after TTL expiry: status %d, want 403", resp2.StatusCode)
		}
	})

	t.Run("NativeMultipartRoundTripsMatchingDigest", func(t *testing.T) {
		if !caps.NativeMultipart {
			t.Skip("Capabilities().NativeMultipart is false for this backend/credential path")
		}
		part1 := randomBytes(t, 5<<20)
		part2 := randomBytes(t, 5<<20)
		want := sha256.Sum256(append(append([]byte{}, part1...), part2...))

		ref := ref(t, "case13", "multipart.bin")
		id, err := store.StartMultipart(ctx, ref, storage.PutOptions{})
		if err != nil {
			t.Fatalf("StartMultipart: %v", err)
		}

		url1, err := store.PresignPart(ctx, ref, id, 1, time.Minute)
		if err != nil {
			t.Fatalf("PresignPart(1): %v", err)
		}
		etag1, err := httpPutBytes(url1, part1)
		if err != nil {
			t.Fatalf("stage part 1: %v", err)
		}
		url2, err := store.PresignPart(ctx, ref, id, 2, time.Minute)
		if err != nil {
			t.Fatalf("PresignPart(2): %v", err)
		}
		etag2, err := httpPutBytes(url2, part2)
		if err != nil {
			t.Fatalf("stage part 2: %v", err)
		}

		// Parts deliberately passed out of number order — CompleteMultipart
		// must assemble by Number, not by slice order.
		parts := []storage.Part{{Number: 2, ETag: etag2}, {Number: 1, ETag: etag1}}
		if _, err := store.CompleteMultipart(ctx, ref, id, parts); err != nil {
			t.Fatalf("CompleteMultipart: %v", err)
		}

		body, _, err := store.Get(ctx, ref, nil)
		if err != nil {
			t.Fatalf("Get after CompleteMultipart: %v", err)
		}
		got, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		gotSum := sha256.Sum256(got)
		if gotSum != want {
			t.Fatalf("assembled object digest mismatch: got %x want %x (len got=%d want=%d)", gotSum, want, len(got), len(part1)+len(part2))
		}
	})

	t.Run("AbortMultipartLeavesNoListableObject", func(t *testing.T) {
		if !caps.NativeMultipart {
			t.Skip("Capabilities().NativeMultipart is false for this backend/credential path")
		}
		ref := ref(t, "case14", "aborted.bin")
		id, err := store.StartMultipart(ctx, ref, storage.PutOptions{})
		if err != nil {
			t.Fatalf("StartMultipart: %v", err)
		}
		url1, err := store.PresignPart(ctx, ref, id, 1, time.Minute)
		if err != nil {
			t.Fatalf("PresignPart: %v", err)
		}
		if _, err := httpPutBytes(url1, []byte("this part is never committed")); err != nil {
			t.Fatalf("stage part: %v", err)
		}

		if err := store.AbortMultipart(ctx, ref, id); err != nil {
			t.Fatalf("AbortMultipart: %v", err)
		}

		if _, _, err := store.Get(ctx, ref, nil); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Get after AbortMultipart err = %v, want ErrNotFound", err)
		}
	})

	t.Run("NonSeekableBodyRoundTripsMatchingDigest", func(t *testing.T) {
		// S9's real HTTP upload handler passes Put a *multipart.Part, which
		// is not an io.ReadSeeker — io.NopCloser here strips any Seek method
		// the underlying bytes.Reader has, forcing the same non-seekable
		// path in the s3 backend, which (unlike azure/gcs) branches on
		// io.ReadSeeker and routes non-seekable bodies through a multipart
		// uploader instead of a single PutObject (see s3/backend.go's Put).
		content := randomBytes(t, 5<<20)
		wantSum := sha256.Sum256(content)

		ref := ref(t, "case16", "non-seekable.bin")
		body := io.NopCloser(bytes.NewReader(content))
		if _, ok := body.(io.ReadSeeker); ok {
			t.Fatal("io.NopCloser body unexpectedly still satisfies io.ReadSeeker")
		}
		putInfo, err := store.Put(ctx, ref, body, storage.PutOptions{ContentLength: int64(len(content))})
		if err != nil {
			t.Fatalf("Put(non-seekable body): %v", err)
		}
		// Put's own returned ObjectInfo.Size, not a subsequent Get/Stat's —
		// those derive Size from an independent, already-correct backend
		// response and would pass even if Put itself always reported 0. S19
		// found exactly that: s3 and azure's non-seekable Put path (the one
		// every real multipart-form upload actually takes — see S9's
		// UploadObject, which always passes ContentLength: -1) silently
		// dropped Size on the floor because neither SDK's streaming-upload
		// response carries a size field to read it from.
		if putInfo.Size != int64(len(content)) {
			t.Fatalf("Put(non-seekable body) returned Size=%d, want %d", putInfo.Size, len(content))
		}

		got, _, err := store.Get(ctx, ref, nil)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		gotBytes, err := io.ReadAll(got)
		_ = got.Close()
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		gotSum := sha256.Sum256(gotBytes)
		if gotSum != wantSum {
			t.Fatalf("round-tripped SHA-256 mismatch (lengths equal=%v): got %x want %x", len(gotBytes) == len(content), gotSum, wantSum)
		}
	})

	t.Run("NonNativeMultipartBackendReturnsErrNotSupported", func(t *testing.T) {
		if caps.NativeMultipart {
			t.Skip("Capabilities().NativeMultipart is true for this backend/credential path")
		}
		ref := ref(t, "case15", "unsupported.bin")

		if _, err := store.StartMultipart(ctx, ref, storage.PutOptions{}); !errors.Is(err, storage.ErrNotSupported) {
			t.Errorf("StartMultipart err = %v, want ErrNotSupported", err)
		}
		if _, err := store.PresignPart(ctx, ref, "dummy", 1, time.Minute); !errors.Is(err, storage.ErrNotSupported) {
			t.Errorf("PresignPart err = %v, want ErrNotSupported", err)
		}
		if _, err := store.CompleteMultipart(ctx, ref, "dummy", nil); !errors.Is(err, storage.ErrNotSupported) {
			t.Errorf("CompleteMultipart err = %v, want ErrNotSupported", err)
		}
		if err := store.AbortMultipart(ctx, ref, "dummy"); !errors.Is(err, storage.ErrNotSupported) {
			t.Errorf("AbortMultipart err = %v, want ErrNotSupported", err)
		}
	})
}

func ref(t *testing.T, logicalBucket, key string) storage.ObjectRef {
	t.Helper()
	r, err := storage.NewObjectRef(conformanceProjectID, logicalBucket, key)
	if err != nil {
		t.Fatalf("NewObjectRef(%q, %q, %q): %v", conformanceProjectID, logicalBucket, key, err)
	}
	return r
}

func bucketRef(t *testing.T, logicalBucket string) storage.ObjectRef {
	t.Helper()
	r, err := storage.NewBucketRef(conformanceProjectID, logicalBucket)
	if err != nil {
		t.Fatalf("NewBucketRef(%q, %q): %v", conformanceProjectID, logicalBucket, err)
	}
	return r
}

func mustPut(t *testing.T, store storage.ObjectStore, logicalBucket, key string, content []byte) {
	t.Helper()
	r := ref(t, logicalBucket, key)
	if _, err := store.Put(context.Background(), r, bytes.NewReader(content), storage.PutOptions{ContentLength: int64(len(content))}); err != nil {
		t.Fatalf("Put(%s/%s): %v", logicalBucket, key, err)
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("crypto/rand.Read: %v", err)
	}
	return buf
}

// httpPutBytes stages one multipart part and returns the ETag the provider
// assigned it. CompleteMultipart (S3 in particular) validates each part's
// ETag exactly matches what its own UploadPart/staging response returned —
// omitting it, or sending an empty string, fails the whole completion.
func httpPutBytes(url string, body []byte) (string, error) {
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.ContentLength = int64(len(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("PUT %s: status %d: %s", url, resp.StatusCode, b)
	}
	return resp.Header.Get("ETag"), nil
}
