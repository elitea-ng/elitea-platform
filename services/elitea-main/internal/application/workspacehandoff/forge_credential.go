package workspacehandoff

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
)

// The thread's pull-request credential (ADR-0034 decision 10, amendment
// 2026-10-26). The cloud executor opens a pull request through a GitHub
// credential the thread's owner chose for the thread: a REFERENCE to one of
// the project's GitHub configurations (or one of the owner's private ones),
// never its secret. Main redeems it only for a running cloud turn of the
// thread, over the claim-bound runtime plane, and only for the worker, which
// keeps it outside the sandbox. The thread's upstream remote (recorded at
// send) scopes it: the worker refuses a credential for another host.

// ForgeCredential references one GitHub configuration by its title, the way
// a toolkit setting does ({elitea_title, private}).
type ForgeCredential struct {
	EliteaTitle string `json:"elitea_title"`
	Private     bool   `json:"private"`
}

// MaxForgeCredentialTitleBytes bounds a referenced configuration title.
const MaxForgeCredentialTitleBytes = 256

// ForgeCredentialType is the configuration type a pull-request credential
// must have: GitHub is the only forge the cloud executor opens pull
// requests on (decision 10).
const ForgeCredentialType = "github"

// EventForgeCredentialSet and EventForgeCredentialCleared audit the owner's
// choice: the configuration's title and scope, never a secret.
const (
	EventForgeCredentialSet     = "pull_request.credential_set"
	EventForgeCredentialCleared = "pull_request.credential_cleared"
)

var (
	// ErrPullRequestsDisabled: the effective policy does not allow pull
	// requests from the cloud for the project.
	ErrPullRequestsDisabled = errors.New("pull requests from the cloud are disabled")
	// ErrForgeCredentialInvalid: the referenced configuration does not exist
	// for the owner, or is not a GitHub configuration.
	ErrForgeCredentialInvalid = errors.New("the configuration is not a GitHub configuration of the project")
	// ErrNoForgeCredential: the thread has no pull-request credential.
	ErrNoForgeCredential = errors.New("the thread has no pull-request credential")
)

// ForgeCredentialStore persists the reference on the handoff row (shared
// 0179). nil clears it.
type ForgeCredentialStore interface {
	SetForgeCredential(ctx context.Context, thread Thread, ownerUserID int64, credential *ForgeCredential) (Handoff, error)
}

// ForgeCredentialResolver expands a reference for userID in projectID.
// With unsecret false it only checks that the reference resolves; with true
// it answers the configuration's data with its secrets redeemed. Either way
// it answers ErrForgeCredentialInvalid for a reference that does not resolve
// to a GitHub configuration.
type ForgeCredentialResolver interface {
	ResolveForgeCredential(ctx context.Context, projectID, userID int64, credential ForgeCredential, unsecret bool) (map[string]any, error)
}

// WithForgeCredentials supplies the pull-request credential plumbing.
// Without it SetForgeCredential and ForgeCredentialForTurn answer
// ErrUnavailable; every other path works.
func (s *Service) WithForgeCredentials(store ForgeCredentialStore, resolver ForgeCredentialResolver) *Service {
	if store == nil || resolver == nil {
		return s
	}
	s.forgeStore, s.forgeResolver = store, resolver
	return s
}

// ForgeCredentialsAvailable reports whether the deployment can set a
// pull-request credential.
func (s *Service) ForgeCredentialsAvailable() bool {
	return s != nil && s.forgeStore != nil && s.forgeResolver != nil
}

// SetForgeCredentialRequest chooses (or clears, with a nil Credential) the
// thread's pull-request credential.
type SetForgeCredentialRequest struct {
	Thread      Thread
	ActorUserID int64
	Credential  *ForgeCredential
	AuditRoute  string
}

// ValidForgeCredential checks a reference's shape.
func ValidForgeCredential(credential ForgeCredential) bool {
	title := credential.EliteaTitle
	return title != "" && strings.TrimSpace(title) == title && len(title) <= MaxForgeCredentialTitleBytes &&
		utf8.ValidString(title) && !strings.ContainsFunc(title, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// SetForgeCredential stores the owner's choice. Only the thread owner sets
// it, only while the thread has a cloud copy (any state but LOCAL), and a
// choice (not a clear) only while the policy allows pull requests.
func (s *Service) SetForgeCredential(ctx context.Context, request SetForgeCredentialRequest) (Handoff, error) {
	started := s.now()
	if !validThread(request.Thread) || !validActor(request.ActorUserID) ||
		(request.Credential != nil && !ValidForgeCredential(*request.Credential)) {
		return Handoff{}, ErrInvalid
	}
	if !s.ForgeCredentialsAvailable() {
		return Handoff{}, ErrUnavailable
	}
	handoff, ok, err := s.repo.Read(ctx, request.Thread)
	if err != nil {
		return Handoff{}, err
	}
	if !ok || handoff.OwnerUserID != request.ActorUserID {
		return Handoff{}, ErrNotFound
	}
	if handoff.DeletedAt != nil {
		return Handoff{}, ErrErased
	}
	if handoff.State == workspacestore.StateLocal {
		return Handoff{}, ErrNotInCloud
	}
	if request.Credential != nil {
		policy, err := s.policy.Effective(ctx, request.Thread.ProjectID)
		if err != nil {
			return Handoff{}, err
		}
		if !policy.PullRequests {
			return Handoff{}, ErrPullRequestsDisabled
		}
		if _, err := s.forgeResolver.ResolveForgeCredential(ctx, request.Thread.ProjectID, request.ActorUserID,
			*request.Credential, false); err != nil {
			return Handoff{}, err
		}
	}
	updated, err := s.forgeStore.SetForgeCredential(ctx, request.Thread, request.ActorUserID, request.Credential)
	if err != nil {
		return Handoff{}, err
	}
	action := EventForgeCredentialCleared
	if request.Credential != nil {
		scope := "project"
		if request.Credential.Private {
			scope = "private"
		}
		action = EventForgeCredentialSet + ": configuration " + request.Credential.EliteaTitle + " (" + scope + ")"
	}
	s.record(ctx, request.Thread, request.ActorUserID, "PUT", request.AuditRoute, started, action, false)
	return updated, nil
}

// ForgeCredentialForTurn redeems the thread's pull-request credential for
// its running cloud turn (the worker's claim-bound call; the caller has
// already bound the claim to this handoff). The policy is recomputed here,
// so turning pull requests off applies to the next call, and the reference
// is redeemed as the OWNER, whose choice it is.
func (s *Service) ForgeCredentialForTurn(ctx context.Context, handoff Handoff) (map[string]any, error) {
	if !s.ForgeCredentialsAvailable() {
		return nil, ErrUnavailable
	}
	if handoff.ForgeCredential == nil {
		return nil, ErrNoForgeCredential
	}
	policy, err := s.policy.Effective(ctx, handoff.ProjectID)
	if err != nil {
		return nil, err
	}
	if !policy.Enabled || !policy.PullRequests {
		return nil, ErrPullRequestsDisabled
	}
	return s.forgeResolver.ResolveForgeCredential(ctx, handoff.ProjectID, handoff.OwnerUserID, *handoff.ForgeCredential, true)
}
