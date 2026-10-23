package storage

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/extract"
)

// Issue 1062: the upload warms the extraction, so the first turn reads the
// sidecar instead of paying for the extraction.

func warmupTestDocument() extract.Document {
	return extract.Document{
		Format: extract.FormatPDF, Text: "warmed text", UnitCount: 1,
		Units:            []extract.Unit{{Kind: extract.UnitPage, Label: "1", Start: 0, End: 11, HasText: true}},
		ExtractorVersion: extract.Version,
	}
}

func TestWarmAttachmentExtractionFilesTheResultTheFirstTurnReads(t *testing.T) {
	t.Parallel()
	key := attachmentTestConversation + "/warm.pdf"
	cache := &fakeExtractionCache{
		version:    AttachmentObjectVersion{ObjectID: 3, ByteLength: 4, UpdatedAt: 5, MediaType: "application/pdf"},
		serveSaved: true,
	}
	var extractions atomic.Int32
	service, err := NewRuntimeAttachmentObjectService(
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
		fixtureSource("chat-attachments", key, []byte("%PDF"), "application/pdf"),
		extractorFunc(func(context.Context, []byte) (extract.Document, error) {
			extractions.Add(1)
			return warmupTestDocument(), nil
		}),
		cache,
	)
	require.NoError(t, err)

	service.WarmAttachmentExtraction(4242, "chat-attachments", key)
	require.Eventually(t, func() bool { return cache.savedCount() == 1 }, 10*time.Second, 5*time.Millisecond)

	response := serveAttachment(t, newAttachmentObjectTestServerWithService(t, service), key)
	require.Equal(t, codes.OK, response.Code)
	var document RuntimeAttachmentObjectContext
	require.NoError(t, json.Unmarshal(response.Body, &document))
	require.Equal(t, "warmed text", document.Content)
	require.EqualValues(t, 1, extractions.Load(), "the turn must read the warmed sidecar, not extract again")
}

func TestWarmAttachmentExtractionIsSkippedWithoutASidecarOrPastItsBound(t *testing.T) {
	t.Parallel()
	key := attachmentTestConversation + "/warm.pdf"
	var reads atomic.Int32
	source := attachmentObjectSourceFunc(func(context.Context, int64, string, string, int64) (AttachmentObjectRecord, error) {
		reads.Add(1)
		return AttachmentObjectRecord{}, ErrContentNotFound
	})
	extractor := extractorFunc(func(context.Context, []byte) (extract.Document, error) {
		return warmupTestDocument(), nil
	})

	// No sidecar store: nothing would be filed, so nothing runs.
	uncached, err := NewRuntimeAttachmentObjectService(
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation), source, extractor, nil)
	require.NoError(t, err)
	uncached.WarmAttachmentExtraction(4242, "chat-attachments", key)

	// Every warm-up slot taken: the next one is skipped, not queued.
	cache := &fakeExtractionCache{version: AttachmentObjectVersion{ObjectID: 3, ByteLength: 4, UpdatedAt: 5}}
	full, err := NewRuntimeAttachmentObjectService(
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation), source, extractor, cache)
	require.NoError(t, err)
	for range cap(full.warmups) {
		full.warmups <- struct{}{}
	}
	full.WarmAttachmentExtraction(4242, "chat-attachments", key)

	// An unaddressable key never reaches the source.
	cache2 := &fakeExtractionCache{version: cache.version}
	open, err := NewRuntimeAttachmentObjectService(
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation), source, extractor, cache2)
	require.NoError(t, err)
	open.WarmAttachmentExtraction(4242, "chat-attachments", "../escape")

	time.Sleep(50 * time.Millisecond)
	require.Zero(t, reads.Load())
	cache.mu.Lock()
	defer cache.mu.Unlock()
	require.Zero(t, cache.loads)
}
