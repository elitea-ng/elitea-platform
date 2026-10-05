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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/extract"
)

// These tests pin what the route now does with a document that is not plain
// UTF-8 text: it extracts it, serves the text with its page map, says whether
// the text is complete, and files the result in the sidecar store.

func attachmentFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "extract", "testdata", name))
	require.NoError(t, err)
	return data
}

func fixtureSource(bucket, name string, data []byte, mediaType string) AttachmentObjectSource {
	return attachmentObjectSourceFunc(func(
		context.Context, int64, string, string, int64,
	) (AttachmentObjectRecord, error) {
		return AttachmentObjectRecord{
			Bucket: bucket, Name: name, MediaType: mediaType,
			ByteLength: int64(len(data)), Content: bytes.Clone(data),
		}, nil
	})
}

func serveAttachment(t *testing.T, server *ContentServer, name string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, attachmentObjectRequest(
		t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size), "chat-attachments", name,
	))
	return response
}

func TestAttachmentObjectRouteServesExtractedPDFText(t *testing.T) {
	t.Parallel()
	key := attachmentTestConversation + "/Unknown.pdf"
	server := newAttachmentObjectTestServer(
		t,
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
		fixtureSource("chat-attachments", key, attachmentFixture(t, "report.pdf"), "application/pdf"),
	)
	response := serveAttachment(t, server, key)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	var document RuntimeAttachmentObjectContext
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &document))
	require.Equal(t, "pdf", document.Format)
	require.Contains(t, document.Content, "FIXTURETOKENPDF1")
	require.Contains(t, document.Content, "FIXTURETOKENPDF2")
	require.True(t, document.Complete)
	require.Empty(t, document.PartialReason)
	require.Equal(t, 4, document.UnitCount)
	require.Len(t, document.Units, 4)
	require.Equal(t, []int{3}, document.LowTextUnits, "page 3 has no text layer")
	require.Contains(t, document.Content[document.Units[1].Start:document.Units[1].End], "FIXTURETOKENPDF2")
	require.EqualValues(t, len(document.Content), document.TextBytes)
}

func TestAttachmentObjectRouteNamesTheReasonForAnEncryptedPDF(t *testing.T) {
	t.Parallel()
	key := attachmentTestConversation + "/locked.pdf"
	server := newAttachmentObjectTestServer(
		t,
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
		fixtureSource("chat-attachments", key, attachmentFixture(t, "encrypted.pdf"), "application/pdf"),
	)
	response := serveAttachment(t, server, key)
	require.Equal(t, http.StatusUnprocessableEntity, response.Code)
	var body RuntimeAttachmentUnreadable
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, "encrypted", body.Reason)
}

// TestAttachmentObjectRouteSaysWhenItServesOnlyPartOfTheText is the
// owner's rule: never cut content without telling the model. A text longer
// than one response is served up to a unit boundary, and the document says
// it is not complete and which units it covers.
func TestAttachmentObjectRouteSaysWhenItServesOnlyPartOfTheText(t *testing.T) {
	t.Parallel()
	key := attachmentTestConversation + "/long.txt"
	// 40 parts of ~8 KiB each.
	line := strings.Repeat("z", 99) + "\n"
	data := []byte(strings.Repeat(line, 3_200))
	service, err := NewRuntimeAttachmentObjectService(
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
		fixtureSource("chat-attachments", key, data, "text/plain"),
		extract.New(extract.DefaultLimits()),
		nil,
	)
	require.NoError(t, err)
	service.maxServed = 100 << 10
	response := serveAttachment(t, newAttachmentObjectTestServerWithService(t, service), key)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	var document RuntimeAttachmentObjectContext
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &document))
	require.False(t, document.Complete)
	require.Equal(t, "served_limit", document.PartialReason)
	require.LessOrEqual(t, len(document.Content), 100<<10)
	require.EqualValues(t, len(data), document.TextBytes)
	require.Greater(t, document.UnitCount, len(document.Units))
	last := document.Units[len(document.Units)-1]
	require.Equal(t, len(document.Content), last.End, "the cut falls on a unit boundary")
}

func TestAttachmentObjectRouteDoesNotHTMLEscapeText(t *testing.T) {
	t.Parallel()
	key := attachmentTestConversation + "/page.html"
	server := newAttachmentObjectTestServer(
		t,
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
		fixtureSource("chat-attachments", key, []byte("<b>a & b</b>"), "text/html"),
	)
	response := serveAttachment(t, server, key)
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), `"content":"<b>a & b</b>"`)
}

type fakeExtractionCache struct {
	mu        sync.Mutex
	hit       *AttachmentExtraction
	digestHit *AttachmentExtraction
	version   AttachmentObjectVersion
	loadErr   error
	saveErr   error
	saved     []AttachmentExtraction
	digests   [][]byte
	loads     int
	// serveSaved makes a load return what was filed, as the real store does.
	serveSaved bool
}

func (cache *fakeExtractionCache) LoadAttachmentExtractionByDigest(
	_ context.Context, version AttachmentObjectVersion, extractorVersion string, digest []byte,
) (AttachmentExtraction, bool, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if version != cache.version || extractorVersion != extract.Version || len(digest) != sha256.Size {
		return AttachmentExtraction{}, false, errors.New("wrong digest lookup")
	}
	if cache.digestHit != nil {
		return *cache.digestHit, true, nil
	}
	return AttachmentExtraction{}, false, nil
}

func (cache *fakeExtractionCache) savedCount() int {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return len(cache.saved)
}

func (cache *fakeExtractionCache) LoadAttachmentExtraction(
	_ context.Context, projectID int64, _ string, _ string, extractorVersion string,
) (AttachmentExtraction, AttachmentObjectVersion, bool, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.loads++
	if projectID != 4242 || extractorVersion != extract.Version {
		return AttachmentExtraction{}, AttachmentObjectVersion{}, false, errors.New("wrong lookup")
	}
	if cache.loadErr != nil {
		return AttachmentExtraction{}, AttachmentObjectVersion{}, false, cache.loadErr
	}
	if cache.hit != nil {
		return *cache.hit, cache.version, true, nil
	}
	// What was filed answers the next load, as the real store does.
	if len(cache.saved) > 0 && cache.serveSaved {
		return cache.saved[len(cache.saved)-1], cache.version, true, nil
	}
	return AttachmentExtraction{}, cache.version, false, nil
}

func (cache *fakeExtractionCache) SaveAttachmentExtraction(
	_ context.Context, version AttachmentObjectVersion, digest []byte, extraction AttachmentExtraction,
) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if version != cache.version {
		return errors.New("saved under the wrong version")
	}
	if cache.saveErr != nil {
		return cache.saveErr
	}
	cache.saved = append(cache.saved, extraction)
	cache.digests = append(cache.digests, digest)
	return nil
}

type extractorFunc func(context.Context, []byte) (extract.Document, error)

func (f extractorFunc) Extract(ctx context.Context, data []byte) (extract.Document, error) {
	return f(ctx, data)
}

func TestAttachmentObjectServiceUsesTheSidecar(t *testing.T) {
	t.Parallel()
	key := attachmentTestConversation + "/report.pdf"
	version := AttachmentObjectVersion{ObjectID: 9, ByteLength: 4, UpdatedAt: 77, MediaType: "application/pdf"}

	t.Run("a hit never opens the object", func(t *testing.T) {
		t.Parallel()
		cache := &fakeExtractionCache{
			version: version,
			hit: &AttachmentExtraction{Document: extract.Document{
				Format: extract.FormatPDF, Text: "cached text", UnitCount: 1,
				Units:            []extract.Unit{{Kind: extract.UnitPage, Label: "1", Start: 0, End: 11, HasText: true}},
				ExtractorVersion: extract.Version,
			}},
		}
		service, err := NewRuntimeAttachmentObjectService(
			attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
			attachmentObjectSourceFunc(func(context.Context, int64, string, string, int64) (AttachmentObjectRecord, error) {
				t.Fatal("a sidecar hit must not read the object")
				return AttachmentObjectRecord{}, nil
			}),
			extractorFunc(func(context.Context, []byte) (extract.Document, error) {
				t.Fatal("a sidecar hit must not extract")
				return extract.Document{}, nil
			}),
			cache,
		)
		require.NoError(t, err)
		response := serveAttachment(t, newAttachmentObjectTestServerWithService(t, service), key)
		require.Equal(t, http.StatusOK, response.Code)
		var document RuntimeAttachmentObjectContext
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &document))
		require.Equal(t, "cached text", document.Content)
		require.Equal(t, "application/pdf", document.MediaType)
		require.EqualValues(t, 4, document.ByteLength)
	})

	t.Run("a cached refusal is served as 422 with its reason", func(t *testing.T) {
		t.Parallel()
		cache := &fakeExtractionCache{
			version: version,
			hit:     &AttachmentExtraction{Refused: true, Reason: extract.ReasonEncrypted},
		}
		service, err := NewRuntimeAttachmentObjectService(
			attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
			attachmentObjectSourceFunc(nil),
			extractorFunc(nil),
			cache,
		)
		require.NoError(t, err)
		response := serveAttachment(t, newAttachmentObjectTestServerWithService(t, service), key)
		require.Equal(t, http.StatusUnprocessableEntity, response.Code)
		require.Contains(t, response.Body.String(), `"reason":"encrypted"`)
	})

	t.Run("a miss extracts once and files the result", func(t *testing.T) {
		t.Parallel()
		cache := &fakeExtractionCache{version: version}
		service, err := NewRuntimeAttachmentObjectService(
			attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
			fixtureSource("chat-attachments", key, attachmentFixture(t, "report.pdf"), "application/pdf"),
			extract.New(extract.DefaultLimits()),
			cache,
		)
		require.NoError(t, err)
		// The first PDF compiles the PDFium module, which is slow under -race.
		service.extractionWait = 2 * time.Minute
		response := serveAttachment(t, newAttachmentObjectTestServerWithService(t, service), key)
		require.Equal(t, http.StatusOK, response.Code)
		require.Len(t, cache.saved, 1)
		require.False(t, cache.saved[0].Refused)
		require.Contains(t, cache.saved[0].Document.Text, "FIXTURETOKENPDF1")
		digest := sha256.Sum256(attachmentFixture(t, "report.pdf"))
		require.Equal(t, digest[:], cache.digests[0], "the extraction is filed with the digest of its bytes")
	})

	t.Run("a refusal is filed, a timeout too", func(t *testing.T) {
		t.Parallel()
		// A timeout is filed so that a file that always times out is not
		// parsed on every turn; it answers for attachmentTimeoutRetryAfter.
		for reason, filed := range map[extract.Reason]bool{
			extract.ReasonMalformed: true,
			extract.ReasonTimeout:   true,
		} {
			cache := &fakeExtractionCache{version: version}
			service, err := NewRuntimeAttachmentObjectService(
				attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
				fixtureSource("chat-attachments", key, []byte("%PDF-1.7 broken"), "application/pdf"),
				extractorFunc(func(context.Context, []byte) (extract.Document, error) {
					return extract.Document{}, &extract.Error{Reason: reason}
				}),
				cache,
			)
			require.NoError(t, err)
			response := serveAttachment(t, newAttachmentObjectTestServerWithService(t, service), key)
			require.Equal(t, http.StatusUnprocessableEntity, response.Code)
			require.Contains(t, response.Body.String(), `"reason":"`+string(reason)+`"`)
			require.Equal(t, filed, len(cache.saved) == 1, "reason %s", reason)
		}
	})

	t.Run("a slow extraction answers processing and still files its result", func(t *testing.T) {
		t.Parallel()
		cache := &fakeExtractionCache{version: version}
		release := make(chan struct{})
		service, err := NewRuntimeAttachmentObjectService(
			attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
			fixtureSource("chat-attachments", key, []byte("plain words"), "text/plain"),
			extractorFunc(func(ctx context.Context, data []byte) (extract.Document, error) {
				<-release
				return extract.New(extract.DefaultLimits()).Extract(ctx, data)
			}),
			cache,
		)
		require.NoError(t, err)
		service.extractionWait = 20 * time.Millisecond
		response := serveAttachment(t, newAttachmentObjectTestServerWithService(t, service), key)
		require.Equal(t, http.StatusUnprocessableEntity, response.Code)
		require.Contains(t, response.Body.String(), `"reason":"processing"`)

		// The request is gone; the extraction finishes and files its result
		// for the next turn.
		close(release)
		require.Eventually(t, func() bool {
			cache.mu.Lock()
			defer cache.mu.Unlock()
			return len(cache.saved) == 1 && cache.saved[0].Document.Text == "plain words"
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("an extractor fault is a 503, not a property of the file", func(t *testing.T) {
		t.Parallel()
		cache := &fakeExtractionCache{version: version}
		service, err := NewRuntimeAttachmentObjectService(
			attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
			fixtureSource("chat-attachments", key, []byte("%PDF-1.7"), "application/pdf"),
			extractorFunc(func(context.Context, []byte) (extract.Document, error) {
				return extract.Document{}, extract.ErrUnavailable
			}),
			cache,
		)
		require.NoError(t, err)
		response := serveAttachment(t, newAttachmentObjectTestServerWithService(t, service), key)
		require.Equal(t, http.StatusServiceUnavailable, response.Code)
		require.Empty(t, cache.saved)
	})

	t.Run("a broken sidecar store falls back to the object", func(t *testing.T) {
		t.Parallel()
		cache := &fakeExtractionCache{version: version, loadErr: errors.New("connection reset")}
		service, err := NewRuntimeAttachmentObjectService(
			attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
			fixtureSource("chat-attachments", key, []byte("plain words"), "text/plain"),
			extract.New(extract.DefaultLimits()),
			cache,
		)
		require.NoError(t, err)
		response := serveAttachment(t, newAttachmentObjectTestServerWithService(t, service), key)
		require.Equal(t, http.StatusOK, response.Code)
		require.Empty(t, cache.saved, "nothing is filed without a version")
	})
}
