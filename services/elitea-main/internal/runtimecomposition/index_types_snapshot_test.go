package runtimecomposition

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	indextypesapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/indextypes"
)

func TestPinnedCurrentIndexTypesSnapshotMatchesCurrentWorkerSDKProjection(
	t *testing.T,
) {
	snapshot, err := LoadPinnedCurrentIndexTypesSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SDKRevision() !=
		"b5113a129329b85d23c2d5c2bf55f18e307414ec" ||
		snapshot.EntryCount() != 66 {
		t.Fatalf(
			"revision=%q entries=%d",
			snapshot.SDKRevision(),
			snapshot.EntryCount(),
		)
	}
	wantDigest, ok := parseCurrentSDKConfigurationDigest(
		"sha256:6a2b8162d64f8164a8e00824142e8a8514a9da0c4944773a8ccce05370c00ccc",
	)
	if !ok || snapshot.Digest() != wantDigest {
		t.Fatalf("digest=%x", snapshot.Digest())
	}

	result, err := snapshot.GetCurrentIndexTypes(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.DocumentTypes) != 19 ||
		len(result.ImageTypes) != 5 ||
		len(result.CodeTypes) != 42 ||
		result.DocumentTypes[".docx"] !=
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document" ||
		result.DocumentTypes[".mdx"] != "text/markdown" ||
		result.ImageTypes[".png"] != "image/png" ||
		result.CodeTypes[".go"] != "text/plain" {
		t.Fatalf("unexpected snapshot projection=%+v", result)
	}
	if _, found := result.ImageTypes[".svg"]; found {
		t.Fatal("converted-only .svg drifted into the current endpoint projection")
	}
	if _, found := result.ImageTypes[".bmp"]; found {
		t.Fatal("converted-only .bmp drifted into the current endpoint projection")
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(
		"../api/v2/indextypes/testdata/current_index_types_ui_response.json",
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != strings.TrimSpace(string(fixture)) {
		t.Fatalf("unchanged UI fixture drifted\ngot:  %s\nwant: %s", encoded, fixture)
	}
}

func TestCurrentIndexTypesSnapshotReturnsDetachedConcurrentResponses(
	t *testing.T,
) {
	snapshot, err := LoadPinnedCurrentIndexTypesSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	first, err := snapshot.GetCurrentIndexTypes(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	first.DocumentTypes[".txt"] = "caller/corruption"
	delete(first.ImageTypes, ".png")
	first.CodeTypes[".new"] = "text/plain"

	again, err := snapshot.GetCurrentIndexTypes(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if again.DocumentTypes[".txt"] != "text/plain" ||
		again.ImageTypes[".png"] != "image/png" {
		t.Fatalf("snapshot aliased caller maps: %+v", again)
	}
	if _, found := again.CodeTypes[".new"]; found {
		t.Fatal("caller mutation reached immutable snapshot")
	}

	var wait sync.WaitGroup
	for index := 0; index < 64; index++ {
		wait.Add(1)
		go func(projectID int32) {
			defer wait.Done()
			result, getErr := snapshot.GetCurrentIndexTypes(
				context.Background(),
				projectID,
			)
			if getErr != nil {
				t.Errorf("project %d: %v", projectID, getErr)
				return
			}
			result.CodeTypes[".go"] = "caller/local"
		}(int32(index + 1))
	}
	wait.Wait()
}

func TestCurrentIndexTypesSnapshotRejectsDriftedOrUnboundedDocuments(
	t *testing.T,
) {
	tests := []struct {
		name   string
		mutate func(*currentIndexTypesSnapshotDocument)
	}{
		{
			name: "wrong schema version",
			mutate: func(document *currentIndexTypesSnapshotDocument) {
				document.SchemaVersion = "v2"
			},
		},
		{
			name: "wrong SDK revision",
			mutate: func(document *currentIndexTypesSnapshotDocument) {
				document.SDKRevision = "sdk-main"
			},
		},
		{
			name: "wrong source",
			mutate: func(document *currentIndexTypesSnapshotDocument) {
				document.SourcePath = "partial-list.json"
			},
		},
		{
			name: "invalid source digest",
			mutate: func(document *currentIndexTypesSnapshotDocument) {
				document.SourceDigest = "sha256:not-a-digest"
			},
		},
		{
			name: "partial snapshot",
			mutate: func(document *currentIndexTypesSnapshotDocument) {
				document.Complete = false
			},
		},
		{
			name: "wrong category count",
			mutate: func(document *currentIndexTypesSnapshotDocument) {
				document.CategoryCount = 2
			},
		},
		{
			name: "wrong entry count",
			mutate: func(document *currentIndexTypesSnapshotDocument) {
				document.EntryCount++
			},
		},
		{
			name: "missing category",
			mutate: func(document *currentIndexTypesSnapshotDocument) {
				document.Categories.CodeTypes = nil
			},
		},
		{
			name: "invalid extension",
			mutate: func(document *currentIndexTypesSnapshotDocument) {
				document.Categories.CodeTypes["../go"] = "text/plain"
			},
		},
		{
			name: "invalid MIME",
			mutate: func(document *currentIndexTypesSnapshotDocument) {
				document.Categories.CodeTypes[".go"] = "text/plain\nsecret"
			},
		},
		{
			name: "digest mismatch",
			mutate: func(document *currentIndexTypesSnapshotDocument) {
				document.Categories.CodeTypes[".go"] = "application/go"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := validCurrentIndexTypesDocumentForTest(t)
			test.mutate(&document)
			data, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := LoadCurrentIndexTypesSnapshot(data); !errors.Is(
				err,
				ErrCurrentIndexTypesSnapshotInvalid,
			) {
				t.Fatalf("error=%v", err)
			}
		})
	}

	valid := validCurrentIndexTypesDocumentForTest(t)
	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCurrentIndexTypesSnapshot(
		append(data, []byte(` {}`)...),
	); !errors.Is(err, ErrCurrentIndexTypesSnapshotInvalid) {
		t.Fatalf("trailing error=%v", err)
	}
	if _, err := LoadCurrentIndexTypesSnapshot(
		[]byte(strings.Repeat("x", maxCurrentIndexTypesSnapshotBytes+1)),
	); !errors.Is(err, ErrCurrentIndexTypesSnapshotInvalid) {
		t.Fatalf("oversized error=%v", err)
	}
	unknown := strings.Replace(
		string(data),
		`"complete":true`,
		`"complete":true,"unknown":true`,
		1,
	)
	if _, err := LoadCurrentIndexTypesSnapshot(
		[]byte(unknown),
	); !errors.Is(err, ErrCurrentIndexTypesSnapshotInvalid) {
		t.Fatalf("unknown-field error=%v", err)
	}
}

func TestCurrentIndexTypesSnapshotRejectsInvalidOrCanceledRequests(t *testing.T) {
	snapshot, err := LoadPinnedCurrentIndexTypesSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct {
		ctx       context.Context
		projectID int32
	}{
		{ctx: nil, projectID: 1},
		{ctx: context.Background(), projectID: 0},
	} {
		if _, err := snapshot.GetCurrentIndexTypes(
			request.ctx,
			request.projectID,
		); !errors.Is(err, ErrCurrentIndexTypesSnapshotInvalid) {
			t.Fatalf("request=%+v error=%v", request, err)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := snapshot.GetCurrentIndexTypes(
		canceled,
		1,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error=%v", err)
	}

	var nilSnapshot *CurrentIndexTypesSnapshot
	if _, err := nilSnapshot.GetCurrentIndexTypes(
		context.Background(),
		1,
	); !errors.Is(err, ErrCurrentIndexTypesSnapshotInvalid) {
		t.Fatalf("nil snapshot error=%v", err)
	}
}

func validCurrentIndexTypesDocumentForTest(
	t *testing.T,
) currentIndexTypesSnapshotDocument {
	t.Helper()
	categories := indextypesapi.CurrentIndexTypes{
		DocumentTypes: map[string]string{".txt": "text/plain"},
		ImageTypes:    map[string]string{".png": "image/png"},
		CodeTypes:     map[string]string{".go": "text/plain"},
	}
	canonical, err := json.Marshal(map[string]map[string]string{
		"document_types": categories.DocumentTypes,
		"image_types":    categories.ImageTypes,
		"code_types":     categories.CodeTypes,
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	return currentIndexTypesSnapshotDocument{
		SchemaVersion:  currentIndexTypesSnapshotVersion,
		SDKRevision:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SourcePath:     currentIndexTypesSnapshotSource,
		SourceDigest:   "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		SnapshotDigest: "sha256:" + fmtDigest(digest),
		Complete:       true,
		CategoryCount:  3,
		EntryCount:     3,
		Categories:     categories,
	}
}

func fmtDigest(digest [32]byte) string {
	const hexadecimal = "0123456789abcdef"
	result := make([]byte, len(digest)*2)
	for index, value := range digest {
		result[index*2] = hexadecimal[value>>4]
		result[index*2+1] = hexadecimal[value&0x0f]
	}
	return string(result)
}
