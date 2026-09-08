package eliteacore

import (
	"context"
	"time"
)

// PublishValidationTokenFor mints the approval token the publish route accepts
// for a version, the way a passing pre-publish check does.
//
// It exists so an integration test can skip the quality gate the way a client
// does after a check, WITHOUT a hard-coded string: the token is signed and
// bound to the version's content now (publish_validation_token.go), so a
// literal in a test would only prove that the check it stands for can no longer
// be skipped by typing one.
//
// This file is compiled into the package only when its tests run, so nothing
// that ships can call it.
func (h *Handler) PublishValidationTokenFor(ctx context.Context, schema, versionID string) string {
	return issuePublishValidationToken(versionID,
		h.publishTokenContentHash(ctx, schema, versionID),
		time.Now().Add(publishValidationTokenTTL))
}
