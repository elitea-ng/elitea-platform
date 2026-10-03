package api

import (
	"context"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
)

// successfulOnlyRecorder wraps an audit recorder so it drops every event whose
// status is not 2xx.
//
// It serves the SCIM token endpoint, which answers callers that have no
// session. Its successes are worth a row (a credential was exchanged, and the
// row names the SCIM client). Its refusals are not: recording them would let
// any anonymous caller write one audit row per request.
//
// A nil or typed-nil recorder is returned unchanged, so apimw.Audit still
// recognises "no recorder" and does not mount itself.
func successfulOnlyRecorder(recorder audit.Recorder) audit.Recorder {
	if recorder == nil {
		return nil
	}
	if typed, ok := recorder.(*audit.PostgresRecorder); ok && typed == nil {
		return recorder
	}
	return successfulOnly{next: recorder}
}

type successfulOnly struct{ next audit.Recorder }

func (s successfulOnly) Record(ctx context.Context, event audit.Event) {
	if event.StatusCode == nil || *event.StatusCode < 200 || *event.StatusCode > 299 {
		return
	}
	s.next.Record(ctx, event)
}
