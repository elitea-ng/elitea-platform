package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/extract"
)

// These tests pin how the attachment route behaves under load: one
// extraction per object, a bounded wait that includes the wait for a slot,
// a bounded queue, a pool of its own on the listener, and the filed timeout.

var testAttachmentClaim = ContentClaim{ExecutionID: "execution-1", Generation: 1}

// blockingExtractor counts its calls and holds each one until release is
// closed.
type blockingExtractor struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func newBlockingExtractor() *blockingExtractor {
	return &blockingExtractor{started: make(chan struct{}, 64), release: make(chan struct{})}
}

func (extractor *blockingExtractor) Extract(ctx context.Context, data []byte) (extract.Document, error) {
	extractor.calls.Add(1)
	extractor.started <- struct{}{}
	<-extractor.release
	return extract.New(extract.DefaultLimits()).Extract(ctx, data)
}

func TestAttachmentObjectServiceRunsOneExtractionPerObject(t *testing.T) {
	t.Parallel()
	key := attachmentTestConversation + "/report.txt"
	cache := &fakeExtractionCache{
		version:    AttachmentObjectVersion{ObjectID: 9, ByteLength: 11, UpdatedAt: 77},
		serveSaved: true,
	}
	extractor := newBlockingExtractor()
	service, err := NewRuntimeAttachmentObjectService(
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
		fixtureSource("chat-attachments", key, []byte("hello there"), "text/plain"),
		extractor,
		cache,
	)
	require.NoError(t, err)
	service.extractionWait = time.Minute

	const requests = 6
	var wait sync.WaitGroup
	results := make(chan RuntimeAttachmentObjectContext, requests)
	for range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			document, err := service.Resolve(context.Background(), testAttachmentClaim, "chat-attachments", key)
			if err == nil {
				results <- document
			}
		}()
	}
	<-extractor.started
	// Every request is now waiting on the one running extraction.
	require.Eventually(t, func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.loads == requests
	}, 5*time.Second, 5*time.Millisecond)
	// Let the last request reach the flight it joins.
	time.Sleep(50 * time.Millisecond)
	close(extractor.release)
	wait.Wait()
	close(results)
	served := 0
	for document := range results {
		require.Equal(t, "hello there", document.Content)
		served++
	}
	require.Equal(t, requests, served)
	require.EqualValues(t, 1, extractor.calls.Load(), "concurrent requests for one object share one extraction")
	require.Equal(t, 1, cache.savedCount())
}

func TestAttachmentObjectServiceSlotWaitIsInsideTheWindow(t *testing.T) {
	t.Parallel()
	extractor := newBlockingExtractor()
	defer close(extractor.release)
	cache := &fakeExtractionCache{version: AttachmentObjectVersion{ObjectID: 9, ByteLength: 5, UpdatedAt: 77}}
	service, err := NewRuntimeAttachmentObjectService(
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
		attachmentObjectSourceFunc(func(_ context.Context, _ int64, bucket, name string, _ int64) (AttachmentObjectRecord, error) {
			return AttachmentObjectRecord{Bucket: bucket, Name: name, ByteLength: 5, Content: []byte("words")}, nil
		}),
		extractor,
		cache,
	)
	require.NoError(t, err)
	service.extractionWait = 50 * time.Millisecond

	// Every slot is taken by a slow extraction of ANOTHER object.
	for index := range MaxConcurrentAttachmentExtractions {
		name := attachmentTestConversation + "/slow-" + string(rune('a'+index)) + ".txt"
		_, err := service.Resolve(context.Background(), testAttachmentClaim, "chat-attachments", name)
		var refusal *AttachmentUnreadableError
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, ReasonAttachmentProcessing, refusal.Reason)
	}
	for range MaxConcurrentAttachmentExtractions {
		<-extractor.started
	}

	// A new object cannot get a slot. The request still answers within its
	// wait, with "processing", instead of blocking until the worker's own
	// deadline.
	started := time.Now()
	_, err = service.Resolve(context.Background(), testAttachmentClaim, "chat-attachments", attachmentTestConversation+"/new.txt")
	elapsed := time.Since(started)
	var refusal *AttachmentUnreadableError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, ReasonAttachmentProcessing, refusal.Reason)
	require.Less(t, elapsed, 2*time.Second)
	require.EqualValues(t, MaxConcurrentAttachmentExtractions, extractor.calls.Load(), "the queued one has not started")
}

func TestAttachmentObjectServiceRefusesPastAFullQueue(t *testing.T) {
	t.Parallel()
	extractor := newBlockingExtractor()
	defer close(extractor.release)
	service, err := NewRuntimeAttachmentObjectService(
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
		attachmentObjectSourceFunc(func(_ context.Context, _ int64, bucket, name string, _ int64) (AttachmentObjectRecord, error) {
			return AttachmentObjectRecord{Bucket: bucket, Name: name, ByteLength: 5, Content: []byte("words")}, nil
		}),
		extractor,
		&fakeExtractionCache{version: AttachmentObjectVersion{ObjectID: 9, ByteLength: 5, UpdatedAt: 77}},
	)
	require.NoError(t, err)
	service.extractionWait = 10 * time.Millisecond
	// Fill the slots, then the queue.
	for index := range MaxConcurrentAttachmentExtractions + maxQueuedAttachmentExtractions {
		name := attachmentTestConversation + "/f" + string(rune('A'+index)) + ".txt"
		_, _ = service.Resolve(context.Background(), testAttachmentClaim, "chat-attachments", name)
	}
	require.Eventually(t, func() bool {
		return service.queued.Load() == maxQueuedAttachmentExtractions
	}, 5*time.Second, 5*time.Millisecond)
	_, err = service.Resolve(context.Background(), testAttachmentClaim, "chat-attachments", attachmentTestConversation+"/one-more.txt")
	require.ErrorIs(t, err, ErrContentUnavailable, "a full queue is a server condition, never a file property")
}

func TestAttachmentObjectServiceFiledTimeoutAnswersUntilItIsDue(t *testing.T) {
	t.Parallel()
	key := attachmentTestConversation + "/slow.pdf"
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for name, testCase := range map[string]struct {
		age      time.Duration
		extracts bool
	}{
		"a recent timeout is served without parsing again": {age: time.Minute, extracts: false},
		"an old timeout is tried again":                    {age: 2 * attachmentTimeoutRetryAfter, extracts: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			cache := &fakeExtractionCache{
				version: AttachmentObjectVersion{ObjectID: 9, ByteLength: 5, UpdatedAt: 77},
				hit:     &AttachmentExtraction{Refused: true, Reason: extract.ReasonTimeout, ExtractedAt: now.Add(-testCase.age)},
			}
			service, err := NewRuntimeAttachmentObjectService(
				attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
				fixtureSource("chat-attachments", key, []byte("words"), "text/plain"),
				extractorFunc(func(ctx context.Context, data []byte) (extract.Document, error) {
					calls.Add(1)
					return extract.New(extract.DefaultLimits()).Extract(ctx, data)
				}),
				cache,
			)
			require.NoError(t, err)
			service.now = func() time.Time { return now }
			document, err := service.Resolve(context.Background(), testAttachmentClaim, "chat-attachments", key)
			if testCase.extracts {
				require.NoError(t, err)
				require.Equal(t, "words", document.Content)
				require.EqualValues(t, 1, calls.Load())
				return
			}
			var refusal *AttachmentUnreadableError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, extract.ReasonTimeout, refusal.Reason)
			require.Zero(t, calls.Load())
		})
	}
}

func TestAttachmentObjectServiceUsesTheDigestForAReupload(t *testing.T) {
	t.Parallel()
	key := attachmentTestConversation + "/deck.pdf"
	cache := &fakeExtractionCache{
		version: AttachmentObjectVersion{ObjectID: 9, ByteLength: 5, UpdatedAt: 78},
		digestHit: &AttachmentExtraction{Document: extract.Document{
			Format: extract.FormatText, Text: "filed", UnitCount: 1, ExtractorVersion: extract.Version,
			Units: []extract.Unit{{Kind: extract.UnitPart, Label: "1", Start: 0, End: 5, HasText: true}},
		}},
	}
	service, err := NewRuntimeAttachmentObjectService(
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
		fixtureSource("chat-attachments", key, []byte("filed"), "text/plain"),
		extractorFunc(func(context.Context, []byte) (extract.Document, error) {
			t.Fatal("a digest hit must not extract")
			return extract.Document{}, nil
		}),
		cache,
	)
	require.NoError(t, err)
	document, err := service.Resolve(context.Background(), testAttachmentClaim, "chat-attachments", key)
	require.NoError(t, err)
	require.Equal(t, "filed", document.Content)
	require.Zero(t, cache.savedCount())
}

func TestAttachmentObjectServiceStillAnswersWhenFilingFails(t *testing.T) {
	t.Parallel()
	key := attachmentTestConversation + "/notes.txt"
	cache := &fakeExtractionCache{
		version: AttachmentObjectVersion{ObjectID: 9, ByteLength: 5, UpdatedAt: 77},
		saveErr: errors.New("connection reset"),
	}
	service, err := NewRuntimeAttachmentObjectService(
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
		fixtureSource("chat-attachments", key, []byte("notes"), "text/plain"),
		extract.New(extract.DefaultLimits()),
		cache,
	)
	require.NoError(t, err)
	document, err := service.Resolve(context.Background(), testAttachmentClaim, "chat-attachments", key)
	require.NoError(t, err)
	require.Equal(t, "notes", document.Content)
}

// THE SIDECAR IS BEHIND THE SAME GATES AS THE BYTES. A cache that would
// answer for any key must never be asked about a key of another
// conversation, nor about anything under a rejected claim.
func TestAttachmentObjectServiceConsultsTheSidecarOnlyAfterAuthorization(t *testing.T) {
	t.Parallel()
	const otherConversation = "0e1f2a3b-4c5d-4e6f-8a9b-0c1d2e3f4a5b"
	hit := &AttachmentExtraction{Document: extract.Document{
		Format: extract.FormatText, Text: "another conversation's text", UnitCount: 1,
		ExtractorVersion: extract.Version,
		Units:            []extract.Unit{{Kind: extract.UnitPart, Label: "1", Start: 0, End: 27, HasText: true}},
	}}
	never := attachmentObjectSourceFunc(func(context.Context, int64, string, string, int64) (AttachmentObjectRecord, error) {
		t.Fatal("the object must not be read")
		return AttachmentObjectRecord{}, nil
	})

	t.Run("another conversation's key", func(t *testing.T) {
		t.Parallel()
		cache := &fakeExtractionCache{hit: hit}
		service, err := NewRuntimeAttachmentObjectService(
			attachmentTestAuthorizer(t, 4242, attachmentTestConversation), never, extractorFunc(nil), cache,
		)
		require.NoError(t, err)
		_, err = service.Resolve(context.Background(), testAttachmentClaim, "chat-attachments", otherConversation+"/f.pdf")
		require.ErrorIs(t, err, ErrContentUnauthorized)
		require.Zero(t, cache.loads, "the sidecar was asked about another conversation's object")
	})

	t.Run("a rejected claim", func(t *testing.T) {
		t.Parallel()
		cache := &fakeExtractionCache{hit: hit}
		service, err := NewRuntimeAttachmentObjectService(
			agentRuntimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
				return RuntimeContextAuthorization{}, ErrContentUnauthorized
			}),
			never, extractorFunc(nil), cache,
		)
		require.NoError(t, err)
		_, err = service.Resolve(context.Background(), testAttachmentClaim, "chat-attachments", attachmentTestConversation+"/f.pdf")
		require.ErrorIs(t, err, ErrContentUnauthorized)
		require.Zero(t, cache.loads, "the sidecar was asked under a rejected claim")
	})
}

// The attachment route has its own pool: a full shared pool (token and
// version reads) does not refuse it, and a full attachment pool does not
// refuse those.
func TestAttachmentObjectRouteHasItsOwnRequestPool(t *testing.T) {
	t.Parallel()
	key := attachmentTestConversation + "/report.txt"
	server := newAttachmentObjectTestServer(
		t,
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
		fixtureSource("chat-attachments", key, []byte("hello there"), "text/plain"),
	)
	for range cap(server.requests) {
		server.requests <- struct{}{}
	}
	response := serveAttachment(t, server, key)
	require.Equal(t, http.StatusOK, response.Code, "a full shared pool must not refuse an attachment read")
	for range cap(server.requests) {
		<-server.requests
	}

	for range cap(server.attachmentRequests) {
		server.attachmentRequests <- struct{}{}
	}
	response = serveAttachment(t, server, key)
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.True(t, server.acquire(httptest.NewRecorder()), "a full attachment pool must not take the shared pool")
	server.release()
}

// A missing OBJECT is a 404 with a JSON reason, so the worker can tell it
// from a 404 for the route itself (a listener without the attachment route).
func TestAttachmentObjectRouteNamesAMissingObject(t *testing.T) {
	t.Parallel()
	server := newAttachmentObjectTestServer(
		t,
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
		attachmentObjectSourceFunc(func(context.Context, int64, string, string, int64) (AttachmentObjectRecord, error) {
			return AttachmentObjectRecord{}, ErrContentNotFound
		}),
	)
	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, attachmentObjectRequest(
		t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size),
		"chat-attachments", attachmentTestConversation+"/gone.txt",
	))
	require.Equal(t, http.StatusNotFound, response.Code)
	var body RuntimeAttachmentUnreadable
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, RuntimeAttachmentUnreadableSchemaVersion, body.SchemaVersion)
	require.Equal(t, "not_found", body.Reason)
	require.Equal(t, "application/json", response.Header().Get("Content-Type"))
}

func TestAttachmentObjectRouteNamesAnEmptyUpload(t *testing.T) {
	t.Parallel()
	key := attachmentTestConversation + "/empty.txt"
	server := newAttachmentObjectTestServer(
		t,
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
		attachmentObjectSourceFunc(func(context.Context, int64, string, string, int64) (AttachmentObjectRecord, error) {
			return AttachmentObjectRecord{}, &AttachmentUnreadableError{Reason: extract.ReasonEmpty}
		}),
	)
	response := serveAttachment(t, server, key)
	require.Equal(t, http.StatusUnprocessableEntity, response.Code)
	require.Contains(t, response.Body.String(), `"reason":"empty"`)
}
