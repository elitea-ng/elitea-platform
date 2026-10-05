package runtimecomposition

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"strings"

	indextypesapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indextypes"
)

const (
	currentIndexTypesSnapshotVersion  = "elitea.current-index-types-snapshot.v1"
	currentIndexTypesSnapshotSource   = "elitea_sdk/runtime/langchain/document_loaders/constants.py"
	maxCurrentIndexTypesSnapshotBytes = 64 << 10
	maxCurrentIndexTypesEntries       = 256
	maxCurrentIndexTypeExtensionBytes = 32
	maxCurrentIndexTypeMIMEBytes      = 128
)

var ErrCurrentIndexTypesSnapshotInvalid = errors.New("current index-types snapshot is invalid")

//go:embed current_index_types_snapshot.json
var pinnedCurrentIndexTypesSnapshotJSON []byte

type currentIndexTypesSnapshotDocument struct {
	SchemaVersion  string                          `json:"schema_version"`
	SDKRevision    string                          `json:"sdk_revision"`
	SourcePath     string                          `json:"source_path"`
	SourceDigest   string                          `json:"source_digest"`
	SnapshotDigest string                          `json:"snapshot_digest"`
	Complete       bool                            `json:"complete"`
	CategoryCount  int                             `json:"category_count"`
	EntryCount     int                             `json:"entry_count"`
	Categories     indextypesapi.CurrentIndexTypes `json:"categories"`
}

// CurrentIndexTypesSnapshot is the immutable full projection generated from
// the same pinned SDK constants as the current indexer_worker event producer.
// Returned maps are detached so concurrent requests cannot mutate the snapshot.
type CurrentIndexTypesSnapshot struct {
	sdkRevision string
	digest      [32]byte
	entryCount  int
	categories  indextypesapi.CurrentIndexTypes
}

func LoadPinnedCurrentIndexTypesSnapshot() (*CurrentIndexTypesSnapshot, error) {
	return LoadCurrentIndexTypesSnapshot(pinnedCurrentIndexTypesSnapshotJSON)
}

func LoadCurrentIndexTypesSnapshot(data []byte) (*CurrentIndexTypesSnapshot, error) {
	if len(data) == 0 || len(data) > maxCurrentIndexTypesSnapshotBytes {
		return nil, ErrCurrentIndexTypesSnapshotInvalid
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document currentIndexTypesSnapshotDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, ErrCurrentIndexTypesSnapshotInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrCurrentIndexTypesSnapshotInvalid
	}

	if document.SchemaVersion != currentIndexTypesSnapshotVersion ||
		!validCurrentSDKRevision(document.SDKRevision) ||
		document.SourcePath != currentIndexTypesSnapshotSource ||
		!document.Complete || document.CategoryCount != 3 {
		return nil, ErrCurrentIndexTypesSnapshotInvalid
	}
	if _, ok := parseCurrentSDKConfigurationDigest(document.SourceDigest); !ok {
		return nil, ErrCurrentIndexTypesSnapshotInvalid
	}
	snapshotDigest, ok := parseCurrentSDKConfigurationDigest(document.SnapshotDigest)
	if !ok {
		return nil, ErrCurrentIndexTypesSnapshotInvalid
	}

	categories := document.Categories
	if categories.DocumentTypes == nil || categories.ImageTypes == nil ||
		categories.CodeTypes == nil || len(categories.DocumentTypes) == 0 ||
		len(categories.ImageTypes) == 0 || len(categories.CodeTypes) == 0 {
		return nil, ErrCurrentIndexTypesSnapshotInvalid
	}
	entryCount := len(categories.DocumentTypes) +
		len(categories.ImageTypes) +
		len(categories.CodeTypes)
	if document.EntryCount != entryCount || entryCount > maxCurrentIndexTypesEntries {
		return nil, ErrCurrentIndexTypesSnapshotInvalid
	}
	if !validCurrentIndexTypeCategory(categories.DocumentTypes) ||
		!validCurrentIndexTypeCategory(categories.ImageTypes) ||
		!validCurrentIndexTypeCategory(categories.CodeTypes) {
		return nil, ErrCurrentIndexTypesSnapshotInvalid
	}

	canonical, err := json.Marshal(map[string]map[string]string{
		"document_types": categories.DocumentTypes,
		"image_types":    categories.ImageTypes,
		"code_types":     categories.CodeTypes,
	})
	if err != nil || sha256.Sum256(canonical) != snapshotDigest {
		return nil, ErrCurrentIndexTypesSnapshotInvalid
	}

	return &CurrentIndexTypesSnapshot{
		sdkRevision: document.SDKRevision,
		digest:      snapshotDigest,
		entryCount:  entryCount,
		categories:  cloneCurrentIndexTypes(categories),
	}, nil
}

func (snapshot *CurrentIndexTypesSnapshot) SDKRevision() string {
	if snapshot == nil {
		return ""
	}
	return snapshot.sdkRevision
}

func (snapshot *CurrentIndexTypesSnapshot) Digest() [32]byte {
	if snapshot == nil {
		return [32]byte{}
	}
	return snapshot.digest
}

func (snapshot *CurrentIndexTypesSnapshot) EntryCount() int {
	if snapshot == nil {
		return 0
	}
	return snapshot.entryCount
}

func (snapshot *CurrentIndexTypesSnapshot) GetCurrentIndexTypes(
	ctx context.Context,
	projectID int32,
) (indextypesapi.CurrentIndexTypes, error) {
	if snapshot == nil || ctx == nil || projectID <= 0 {
		return indextypesapi.CurrentIndexTypes{}, ErrCurrentIndexTypesSnapshotInvalid
	}
	if err := ctx.Err(); err != nil {
		return indextypesapi.CurrentIndexTypes{}, err
	}
	return cloneCurrentIndexTypes(snapshot.categories), nil
}

// AttachmentExtensions is every extension the pinned SDK loaders read
// (document, image and code categories) plus `.svg`, the one the web
// composer adds to the catalogue it derives from the same three maps
// (apps/elitea-web/src/entities/attachment/api/allowedTypes.ts). It is what
// the discovery document's attachment policy offers a client when this
// catalogue is served.
func (snapshot *CurrentIndexTypesSnapshot) AttachmentExtensions() []string {
	if snapshot == nil {
		return nil
	}
	extensions := []string{".svg"}
	for _, category := range []map[string]string{
		snapshot.categories.DocumentTypes, snapshot.categories.ImageTypes, snapshot.categories.CodeTypes,
	} {
		for extension := range category {
			extensions = append(extensions, extension)
		}
	}
	return extensions
}

func validCurrentIndexTypeCategory(category map[string]string) bool {
	for extension, mimeType := range category {
		if !validCurrentIndexTypeExtension(extension) ||
			!validCurrentIndexTypeMIME(mimeType) {
			return false
		}
	}
	return true
}

func validCurrentIndexTypeExtension(extension string) bool {
	if len(extension) < 2 || len(extension) > maxCurrentIndexTypeExtensionBytes ||
		extension[0] != '.' {
		return false
	}
	for index := 1; index < len(extension); index++ {
		character := extension[index]
		if (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') &&
			character != '_' && character != '-' && character != '+' {
			return false
		}
	}
	return true
}

func validCurrentIndexTypeMIME(mimeType string) bool {
	if len(mimeType) < 3 || len(mimeType) > maxCurrentIndexTypeMIMEBytes ||
		!strings.Contains(mimeType, "/") {
		return false
	}
	for index := range len(mimeType) {
		character := mimeType[index]
		if character <= ' ' || character >= 0x7f {
			return false
		}
	}
	return true
}

func cloneCurrentIndexTypes(
	source indextypesapi.CurrentIndexTypes,
) indextypesapi.CurrentIndexTypes {
	return indextypesapi.CurrentIndexTypes{
		DocumentTypes: cloneCurrentIndexTypeCategory(source.DocumentTypes),
		ImageTypes:    cloneCurrentIndexTypeCategory(source.ImageTypes),
		CodeTypes:     cloneCurrentIndexTypeCategory(source.CodeTypes),
	}
}

func cloneCurrentIndexTypeCategory(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for extension, mimeType := range source {
		result[extension] = mimeType
	}
	return result
}

var _ indextypesapi.CurrentIndexTypesReader = (*CurrentIndexTypesSnapshot)(nil)
