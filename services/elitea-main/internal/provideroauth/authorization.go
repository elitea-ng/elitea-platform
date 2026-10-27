package provideroauth

import (
	"context"
	"time"
)

// AuthorizationRequirement is a governed use refused because its user has
// not consented to the provider for the grant key (#1123): the chat turn
// pauses and the user is sent to consent. It holds no credential.
type AuthorizationRequirement struct {
	Key               GrantKey
	ExecutionID       string
	Generation        uint64
	ConversationUUID  string
	ResponseMessageID string
	// ExpiresAt is set when a reference is opened.
	ExpiresAt time.Time
}

// AuthorizationReferences mints the opaque authorization_reference of an
// AUTHORIZATION_REQUIRED denial for a provider grant. A provider it does
// not serve answers ErrProviderNotConfigured, and the use is denied as
// before (provider_consent_required).
type AuthorizationReferences interface {
	Mint(ctx context.Context, r AuthorizationRequirement) (string, error)
}
