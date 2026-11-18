// unsaved_secret_reference.go redeems `{{secret.NAME}}` references in an
// UNSAVED credential payload before the provider probe (#2095).
//
// The credential form lets a user pick a named project secret instead of
// typing a key, so the payload the browser posts to /check_connection holds
// the literal reference. The probe then asked the provider to authenticate the
// template string and reported a working credential as broken; saving and
// reopening "fixed" it only because the stored check redeems the row's
// references on the server.
package configurations

import (
	"context"
	"log/slog"
	"math"
	"regexp"
	"strconv"
)

// unsavedSecretReferencePattern is the whole-value reference shape the vault
// writes and the form's secret picker produces.
var unsavedSecretReferencePattern = regexp.MustCompile(`^\{\{secret\.[A-Za-z0-9_]+\}\}$`)

// containsSecretReference reports whether any string in the payload, at any
// depth, is a whole-value `{{secret.NAME}}` reference.
func containsSecretReference(value any) bool {
	switch typed := value.(type) {
	case string:
		return unsavedSecretReferencePattern.MatchString(typed)
	case map[string]any:
		for _, nested := range typed {
			if containsSecretReference(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if containsSecretReference(nested) {
				return true
			}
		}
	}
	return false
}

// resolveUnsavedSecretReferences returns the payload with its references
// redeemed through the project's own vault, the same resolver the stored
// checks use. A payload with no reference is returned untouched and never
// reaches the resolver. ok is false when references exist but cannot be
// redeemed; the cause is logged and never returned.
func (h *Handler) resolveUnsavedSecretReferences(
	ctx context.Context, projectID string, data map[string]any,
) (map[string]any, bool) {
	if !containsSecretReference(data) {
		return data, true
	}
	owner, err := strconv.Atoi(projectID)
	if h.storedResolver == nil || err != nil || owner <= 0 || owner > math.MaxInt32 {
		return nil, false
	}
	resolution := StoredConfigurationResolution{ProjectID: int32(owner), Data: data}
	if caller, hasCaller := currentConfigurationMutationAuthorID(ctx); hasCaller && caller > 0 {
		resolution.AuthorID = &caller
	}
	resolved, err := h.storedResolver.ResolveStoredConfiguration(ctx, resolution)
	if err != nil || resolved == nil {
		slog.WarnContext(ctx, "check_connection: a secret reference in the unsaved payload did not resolve",
			"project_id", projectID, "err", err)
		return nil, false
	}
	return resolved, true
}

// unsavedSecretUnresolvedMessage is the 400 body for a reference the vault
// cannot redeem.
const unsavedSecretUnresolvedMessage = "The selected secret could not be resolved. Check that it still exists and has a value."
