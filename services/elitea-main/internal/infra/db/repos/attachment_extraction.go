package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/extract"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// The sidecar store for extracted attachment text
// (migrations/shared/0137_attachment_extractions.sql).
//
// A row answers for ONE object as it is NOW: the lookup and the write both
// carry the object's byte_length and updated_at, and a re-upload under the
// same key changes updated_at, so text extracted from old bytes never answers
// for new ones. The row is deleted with its object (ON DELETE CASCADE).

const loadAttachmentExtractionSQL = `
SELECT status, coalesce(reason, ''), coalesce(format, ''), coalesce(content, ''),
       units, unit_count, low_text_units, coalesce(partial_reason, ''), token_estimate
  FROM elitea_storage.attachment_extractions
 WHERE object_id = $1
   AND extractor_version = $2
   AND source_byte_length = $3
   AND source_updated_at = $4`

// The INSERT is conditional on the object still being the version the
// extraction was made from. A re-upload between the read and this write makes
// it a no-op instead of filing old text under the new object.
const saveAttachmentExtractionSQL = `
INSERT INTO elitea_storage.attachment_extractions (
    object_id, extractor_version, source_byte_length, source_updated_at,
    status, reason, format, content, units, unit_count, low_text_units,
    partial_reason, token_estimate, extracted_at
)
SELECT $1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), $8, $9, $10, $11,
       nullif($12, ''), $13, now()
  FROM elitea_storage.objects o
 WHERE o.id = $1 AND o.byte_length = $3 AND o.updated_at = $4
ON CONFLICT (object_id) DO UPDATE SET
    extractor_version  = EXCLUDED.extractor_version,
    source_byte_length = EXCLUDED.source_byte_length,
    source_updated_at  = EXCLUDED.source_updated_at,
    status             = EXCLUDED.status,
    reason             = EXCLUDED.reason,
    format             = EXCLUDED.format,
    content            = EXCLUDED.content,
    units              = EXCLUDED.units,
    unit_count         = EXCLUDED.unit_count,
    low_text_units     = EXCLUDED.low_text_units,
    partial_reason     = EXCLUDED.partial_reason,
    token_estimate     = EXCLUDED.token_estimate,
    extracted_at       = EXCLUDED.extracted_at`

const (
	extractionStatusExtracted = "extracted"
	extractionStatusRefused   = "refused"
)

// LoadAttachmentExtraction returns the filed extraction for the object the
// triple resolves now, under the same four gates as ReadAttachmentObject's
// lookup (project bucket, system bucket, metadata row). The version is
// returned on a miss too, for SaveAttachmentExtraction.
func (repository *CurrentAttachmentObjectRepository) LoadAttachmentExtraction(
	ctx context.Context,
	projectID int64,
	bucket string,
	name string,
	extractorVersion string,
) (storage.AttachmentExtraction, storage.AttachmentObjectVersion, bool, error) {
	if repository == nil || repository.pool == nil {
		return storage.AttachmentExtraction{}, storage.AttachmentObjectVersion{}, false,
			errors.New("attachment extraction store is unavailable")
	}
	row, err := repository.attachmentObjectRow(ctx, projectID, bucket, name)
	if err != nil {
		return storage.AttachmentExtraction{}, storage.AttachmentObjectVersion{}, false, err
	}
	version := storage.AttachmentObjectVersion{
		ObjectID:   row.ID,
		ByteLength: row.ByteLength,
		UpdatedAt:  row.UpdatedAt.UnixMicro(),
		MediaType:  row.MediaType,
	}
	var (
		status, reason, format, content, partialReason string
		unitsJSON                                      []byte
		unitCount                                      int
		lowText                                        []int32
		tokenEstimate                                  int64
	)
	err = repository.pool.QueryRow(
		ctx, loadAttachmentExtractionSQL,
		row.ID, extractorVersion, row.ByteLength, row.UpdatedAt,
	).Scan(&status, &reason, &format, &content, &unitsJSON, &unitCount, &lowText, &partialReason, &tokenEstimate)
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.AttachmentExtraction{}, version, false, nil
	}
	if err != nil {
		return storage.AttachmentExtraction{}, version, false, fmt.Errorf("load attachment extraction: %w", err)
	}
	if status == extractionStatusRefused {
		return storage.AttachmentExtraction{Refused: true, Reason: extract.Reason(reason)}, version, true, nil
	}
	var units []extract.Unit
	if err := json.Unmarshal(unitsJSON, &units); err != nil {
		// A row this code cannot read is a miss: extracting again replaces it.
		return storage.AttachmentExtraction{}, version, false, nil
	}
	low := make([]int, len(lowText))
	for index, number := range lowText {
		low[index] = int(number)
	}
	return storage.AttachmentExtraction{Document: extract.Document{
		Format:           extract.Format(format),
		Text:             content,
		Units:            units,
		UnitCount:        unitCount,
		LowTextUnits:     low,
		Partial:          partialReason != "",
		PartialBy:        extract.PartialReason(partialReason),
		TokenEstimate:    tokenEstimate,
		ExtractorVersion: extractorVersion,
	}}, version, true, nil
}

// SaveAttachmentExtraction files one extraction under the object version it
// was made from. It is a no-op when that object has changed since.
func (repository *CurrentAttachmentObjectRepository) SaveAttachmentExtraction(
	ctx context.Context,
	version storage.AttachmentObjectVersion,
	extraction storage.AttachmentExtraction,
) error {
	if repository == nil || repository.pool == nil {
		return errors.New("attachment extraction store is unavailable")
	}
	if version.ObjectID <= 0 {
		return errors.New("attachment extraction needs an object version")
	}
	status := extractionStatusExtracted
	var content *string
	units := []byte("[]")
	lowText := []int32{}
	document := extraction.Document
	extractorVersion := document.ExtractorVersion
	if extraction.Refused {
		status = extractionStatusRefused
		extractorVersion = extract.Version
		document = extract.Document{}
	} else {
		text := document.Text
		content = &text
		encoded, err := json.Marshal(document.Units)
		if err != nil {
			return fmt.Errorf("encode attachment extraction units: %w", err)
		}
		units = encoded
		for _, number := range document.LowTextUnits {
			lowText = append(lowText, int32(number)) //nolint:gosec // bounded by the page limit
		}
	}
	_, err := repository.pool.Exec(
		ctx, saveAttachmentExtractionSQL,
		version.ObjectID, extractorVersion, version.ByteLength,
		time.UnixMicro(version.UpdatedAt).UTC(),
		status, string(extraction.Reason), string(document.Format), content, units,
		document.UnitCount, lowText, string(document.PartialBy), document.TokenEstimate,
	)
	if err != nil {
		return fmt.Errorf("save attachment extraction: %w", err)
	}
	return nil
}

var _ storage.AttachmentExtractionCache = (*CurrentAttachmentObjectRepository)(nil)
