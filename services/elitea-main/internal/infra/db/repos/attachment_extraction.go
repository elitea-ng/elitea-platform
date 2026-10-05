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
// (migrations/shared/0137_attachment_extractions.sql and
// 0138_attachment_extraction_source_digest.sql).
//
// A row answers for ONE object as it is NOW. The fast lookup carries the
// object's byte_length and updated_at, and a re-upload under the same key
// changes updated_at, so text extracted from old bytes never answers for new
// ones. The digest lookup answers for a byte-identical re-upload: the row
// records the SHA-256 of the bytes it was made from, and a match is filed
// under the new version. The row is deleted with its object (ON DELETE
// CASCADE).

const attachmentExtractionColumns = `status, coalesce(reason, ''), coalesce(format, ''), coalesce(content, ''),
       units, unit_count, low_text_units, coalesce(partial_reason, ''), token_estimate, extracted_at`

const loadAttachmentExtractionSQL = `
SELECT ` + attachmentExtractionColumns + `
  FROM elitea_storage.attachment_extractions
 WHERE object_id = $1
   AND extractor_version = $2
   AND source_byte_length = $3
   AND source_updated_at = $4`

// The digest lookup files the row under the version it answers for, and only
// while that version is still the object's: a re-upload in between makes it
// a miss.
const loadAttachmentExtractionByDigestSQL = `
UPDATE elitea_storage.attachment_extractions e
   SET source_byte_length = $3,
       source_updated_at  = $4
 WHERE e.object_id = $1
   AND e.extractor_version = $2
   AND e.source_sha256 = $5
   AND EXISTS (
       SELECT 1 FROM elitea_storage.objects o
        WHERE o.id = $1 AND o.byte_length = $3 AND o.updated_at = $4
   )
RETURNING ` + attachmentExtractionColumns

// The INSERT files the extraction under the version it was read for. The
// digest is what makes the row trustworthy: when the object changed between
// the read and this write, the row is labelled with a version the object no
// longer has, so the fast lookup never returns it, and the digest lookup
// returns it only for the same bytes. A row filed for a NEWER version is
// never replaced by an older extraction.
const saveAttachmentExtractionSQL = `
INSERT INTO elitea_storage.attachment_extractions (
    object_id, extractor_version, source_byte_length, source_updated_at,
    status, reason, format, content, units, unit_count, low_text_units,
    partial_reason, token_estimate, extracted_at, source_sha256
)
SELECT $1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), $8, $9, $10, $11,
       nullif($12, ''), $13, now(), $14
  FROM elitea_storage.objects o
 WHERE o.id = $1
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
    extracted_at       = EXCLUDED.extracted_at,
    source_sha256      = EXCLUDED.source_sha256
 WHERE elitea_storage.attachment_extractions.source_updated_at <= EXCLUDED.source_updated_at`

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
	extraction, found, err := scanAttachmentExtraction(repository.pool.QueryRow(
		ctx, loadAttachmentExtractionSQL,
		row.ID, extractorVersion, row.ByteLength, row.UpdatedAt,
	), extractorVersion)
	if err != nil {
		return storage.AttachmentExtraction{}, version, false, fmt.Errorf("load attachment extraction: %w", err)
	}
	return extraction, version, found, nil
}

// LoadAttachmentExtractionByDigest returns the extraction filed for the same
// object and the same bytes under an earlier version, and files it under
// version. It is a miss when version is no longer the object's.
func (repository *CurrentAttachmentObjectRepository) LoadAttachmentExtractionByDigest(
	ctx context.Context,
	version storage.AttachmentObjectVersion,
	extractorVersion string,
	digest []byte,
) (storage.AttachmentExtraction, bool, error) {
	if repository == nil || repository.pool == nil {
		return storage.AttachmentExtraction{}, false, errors.New("attachment extraction store is unavailable")
	}
	if version.ObjectID <= 0 || len(digest) != 32 {
		return storage.AttachmentExtraction{}, false, nil
	}
	extraction, found, err := scanAttachmentExtraction(repository.pool.QueryRow(
		ctx, loadAttachmentExtractionByDigestSQL,
		version.ObjectID, extractorVersion, version.ByteLength,
		time.UnixMicro(version.UpdatedAt).UTC(), digest,
	), extractorVersion)
	if err != nil {
		return storage.AttachmentExtraction{}, false, fmt.Errorf("load attachment extraction by digest: %w", err)
	}
	return extraction, found, nil
}

func scanAttachmentExtraction(row pgx.Row, extractorVersion string) (storage.AttachmentExtraction, bool, error) {
	var (
		status, reason, format, content, partialReason string
		unitsJSON                                      []byte
		unitCount                                      int
		lowText                                        []int32
		tokenEstimate                                  int64
		extractedAt                                    time.Time
	)
	err := row.Scan(&status, &reason, &format, &content, &unitsJSON, &unitCount, &lowText,
		&partialReason, &tokenEstimate, &extractedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.AttachmentExtraction{}, false, nil
	}
	if err != nil {
		return storage.AttachmentExtraction{}, false, err
	}
	if status == extractionStatusRefused {
		return storage.AttachmentExtraction{
			Refused: true, Reason: extract.Reason(reason), ExtractedAt: extractedAt,
		}, true, nil
	}
	var units []extract.Unit
	if err := json.Unmarshal(unitsJSON, &units); err != nil {
		// A row this code cannot read is a miss: extracting again replaces it.
		return storage.AttachmentExtraction{}, false, nil
	}
	low := make([]int, len(lowText))
	for index, number := range lowText {
		low[index] = int(number)
	}
	return storage.AttachmentExtraction{
		Document: extract.Document{
			Format:           extract.Format(format),
			Text:             content,
			Units:            units,
			UnitCount:        unitCount,
			LowTextUnits:     low,
			Partial:          partialReason != "",
			PartialBy:        extract.PartialReason(partialReason),
			TokenEstimate:    tokenEstimate,
			ExtractorVersion: extractorVersion,
		},
		ExtractedAt: extractedAt,
	}, true, nil
}

// SaveAttachmentExtraction files one extraction under the object version it
// was read for, with the SHA-256 of the bytes it was made from (nil when the
// caller has none). It never replaces a row filed for a newer version.
func (repository *CurrentAttachmentObjectRepository) SaveAttachmentExtraction(
	ctx context.Context,
	version storage.AttachmentObjectVersion,
	digest []byte,
	extraction storage.AttachmentExtraction,
) error {
	if repository == nil || repository.pool == nil {
		return errors.New("attachment extraction store is unavailable")
	}
	if version.ObjectID <= 0 {
		return errors.New("attachment extraction needs an object version")
	}
	if digest != nil && len(digest) != 32 {
		return errors.New("attachment extraction digest must be SHA-256")
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
		digest,
	)
	if err != nil {
		return fmt.Errorf("save attachment extraction: %w", err)
	}
	return nil
}

var _ storage.AttachmentExtractionCache = (*CurrentAttachmentObjectRepository)(nil)
