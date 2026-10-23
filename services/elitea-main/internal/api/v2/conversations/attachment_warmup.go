package conversations

import "reflect"

// AttachmentExtractionWarmer starts the text extraction of a stored
// attachment in the background and files the result where a turn's read
// looks it up (issue 1062), so the first turn that carries the file does not
// wait for it. It must return at once and never fail the caller.
// *storage.RuntimeAttachmentObjectService implements it.
type AttachmentExtractionWarmer interface {
	WarmAttachmentExtraction(projectID int64, bucket, name string)
}

// WithAttachmentExtractionWarmer wires the upload-time warm-up. Left nil, an
// upload stores the file and the first turn extracts it, as before. A nil
// pointer boxed in the interface is treated as nil (the #86 trap; see
// WithEvents).
func (h *Handler) WithAttachmentExtractionWarmer(warmer AttachmentExtractionWarmer) *Handler {
	if warmer == nil {
		return h
	}
	if v := reflect.ValueOf(warmer); v.Kind() == reflect.Ptr && v.IsNil() {
		return h
	}
	h.warmer = warmer
	return h
}

// warmAttachmentExtraction hands a just-stored document to the warmer. An
// image is not extracted at turn time (it reaches the model as an image), so
// it is not warmed either.
func (h *Handler) warmAttachmentExtraction(projectID int64, bucket, key, fileName string) {
	if h.warmer == nil || isImageAttachment(fileName) {
		return
	}
	h.warmer.WarmAttachmentExtraction(projectID, bucket, key)
}
