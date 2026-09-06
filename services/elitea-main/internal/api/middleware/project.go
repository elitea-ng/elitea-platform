package middleware

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/publicproject"
)

// projectUserNamePrefix mirrors pylon's projects.constants.PROJECT_USER_NAME_PREFIX.
// System project-user tokens carry a name of the form ":system:project:<id>:".
const projectUserNamePrefix = ":system:project:"

// defaultPublicProjectID mirrors pylon's elitea_config "ai_project_id" default (1).
const defaultPublicProjectID = publicproject.Default

// projectLookupTimeout bounds the personal-project DB lookup so a slow store
// cannot stall the request. Mirrors the intent of pylon's rpc timeout(30) but
// is much tighter for the streaming edge path.
const projectLookupTimeout = 2 * time.Second

// systemUserEmailRe matches pylon's PROJECT_USER_EMAIL_TEMPLATE
// ("system_user_{}@centry.user") used as a fallback source of the project id.
var systemUserEmailRe = regexp.MustCompile(`^system_user_(\d+)@centry\.user$`)

// Project selector headers on the /llm edge (issue #318, ADR-0018).
//
// /llm is a documented external endpoint. The caller authenticates with a
// personal access token, so the token names the user and not the project. A
// caller that works on a team must therefore name the project to bill, or all
// team spend lands on the caller's personal project budget.
//
// Two header names are accepted: X-Project-Id, then OpenAI-Organization. The
// legacy runtime accepted both (runtime_interface_litellm proxy.prepare_request).
const (
	// HeaderProjectSelector is the primary, semantic selector name.
	HeaderProjectSelector = "X-Project-Id"
	// HeaderProjectSelectorOpenAIProject is NOT a selector, and MUST NOT
	// become one. See projectSelectorHeaders for the reason.
	HeaderProjectSelectorOpenAIProject = "OpenAI-Project"
	// HeaderProjectSelectorOpenAIOrg is the legacy OpenAI-compatible name.
	HeaderProjectSelectorOpenAIOrg = "OpenAI-Organization"
)

// Error codes on the /llm project-resolution path (spec-llm-project-scope §8).
// The envelope is the OpenAI-compatible {"error":{"message","type","code"}}
// produced by writeJSONError.
const (
	// errTypeInvalidRequest is the OpenAI-compatible error type for both codes.
	errTypeInvalidRequest = "invalid_request_error"
	// codeProjectNotResolved reports that no project could be determined.
	codeProjectNotResolved = "project_not_resolved"
	// codeProjectScopeConflict reports a selector that contradicts a bound
	// token.
	codeProjectScopeConflict = "project_scope_conflict"
	// errTypePermission is the OpenAI-compatible error type for a refusal the
	// caller cannot correct by changing the request.
	errTypePermission = "permission_error"
	// codeProjectInactive reports a bound project that is no longer usable.
	// The message names no lifecycle column, so it discloses nothing about
	// another tenant's state.
	codeProjectInactive = "project_inactive"
)

// projectSelectorHeaders lists the accepted selector headers in precedence
// order. The first header that carries a non-blank value wins.
//
// OpenAI-Project is absent on purpose, and MUST stay absent (ADR-0018,
// spec-llm-project-scope §6.1). The web UI fills that header from
// model.project_id — the project that OWNS THE MODEL, not the project that
// pays (apps/elitea-web/src/features/settings/lib/ai-configuration/useCodePreview.ts
// and codeExamples.helpers.ts). The models query passes includeShared, so that
// value is frequently the public or shared project. Reading it as the billing
// selector would bill the shared project for every user who copies the UI's own
// generated sample.
var projectSelectorHeaders = []string{
	HeaderProjectSelector,
	HeaderProjectSelectorOpenAIOrg,
}

// projectHeadersStrippedOutbound lists every project header the proxy deletes
// from the outbound request. It is a superset of the accepted selectors:
// OpenAI-Project is never read for billing, but it must not travel onward
// either, because a real provider reads that name.
var projectHeadersStrippedOutbound = []string{
	HeaderProjectSelector,
	HeaderProjectSelectorOpenAIProject,
	HeaderProjectSelectorOpenAIOrg,
}

// ProjectSelectorHeaders returns the accepted selector header names, in
// precedence order.
func ProjectSelectorHeaders() []string {
	names := make([]string, len(projectSelectorHeaders))
	copy(names, projectSelectorHeaders)
	return names
}

// ProjectHeadersStrippedOutbound returns the project headers the proxy deletes
// from the outbound request. The edge consumes the selector, so it must not
// also travel to the gateway and onward to a provider.
func ProjectHeadersStrippedOutbound() []string {
	names := make([]string, len(projectHeadersStrippedOutbound))
	copy(names, projectHeadersStrippedOutbound)
	return names
}

type projectCtxKey struct{}

// ProjectContext holds the project identity resolved at the edge from the
// authenticated caller. It is injected into the request context and later
// surfaced as signed identity headers on the proxied /llm request (BF0.2a).
type ProjectContext struct {
	// ProjectID is the caller's resolved project (virtual-key scope).
	ProjectID int
	// PublicProjectID is the platform's shared/public project used as the
	// model-alias fallback namespace.
	PublicProjectID int
}

// ProjectFromContext returns the resolved project context, if present.
func ProjectFromContext(ctx context.Context) (ProjectContext, bool) {
	pc, ok := ctx.Value(projectCtxKey{}).(ProjectContext)
	return pc, ok
}

// ContextWithProject returns a copy of ctx carrying the resolved project context.
func ContextWithProject(ctx context.Context, pc ProjectContext) context.Context {
	return context.WithValue(ctx, projectCtxKey{}, pc)
}

// PersonalProjectResolver looks up the personal project id for a user, mirroring
// pylon's projects_get_personal_project_id RPC. It returns (0, nil) when no
// personal project can be determined (not an error — the caller decides).
type PersonalProjectResolver interface {
	PersonalProjectID(ctx context.Context, userID string) (int, error)
}

// ProjectConfig configures the Project middleware.
type ProjectConfig struct {
	// Resolver resolves a user's personal project id (DB-backed in production).
	Resolver PersonalProjectResolver
	// PublicProjectID is the platform's shared project id. When zero it is read
	// from the AI_PROJECT_ID environment variable, defaulting to 1.
	PublicProjectID int
	// Membership admits a project the caller names. Two paths ask it: the
	// selector header, and the system project-user name. A nil checker admits
	// neither: the request proceeds on the caller's own project instead.
	Membership ProjectMembershipChecker
}

// Project resolves the project id from the authenticated caller and injects a
// ProjectContext into the request context.
//
// Resolution follows spec-llm-project-scope §5, and no other algorithm:
//
//  1. Token bound, no usable selector → the bound project.
//  2. Token bound, selector equal to the binding → the bound project.
//  3. Token bound, selector naming a different project → HTTP 400
//     project_scope_conflict.
//  4. Token unbound → the caller's own project (a system project-user name that
//     passes the membership check, else the personal-project lookup), which a
//     selector header may then replace after the same membership check (issues
//     #318 and #459). See resolveProjectID and admitProjectSelector.
//
// The binding is read BEFORE the personal-project lookup, so a bound token
// whose owner has no resolvable personal project still succeeds on its binding.
// A bound token also runs no membership query: membership was checked when the
// token was created (spec §4), and the binding is deleted when the owner loses
// membership (spec §7.3).
//
// When no authenticated user is present the request passes through unchanged
// (the Auth middleware is responsible for rejecting unauthenticated callers).
// When a user is present but no project can be resolved, the request is rejected
// with HTTP 400, matching pylon's behaviour.
//
// Row 3 is the only selector refusal here. Every other inadmissible selector is
// ignored in silence. See admitProjectSelector for why. requireRuntimePrincipal
// adds the one other refusal on this path, and it is an authentication refusal
// rather than a selector one.
func Project(cfg ProjectConfig) func(http.Handler) http.Handler {
	publicProjectID := cfg.PublicProjectID
	if publicProjectID == 0 {
		publicProjectID = publicProjectIDFromEnv()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := auth.UserFromContext(r.Context())
			if !ok {
				// Unauthenticated: let downstream auth handling decide.
				next.ServeHTTP(w, r)
				return
			}
			if !requireRuntimePrincipal(w, r) {
				return
			}

			ctx, cancel := context.WithTimeout(r.Context(), projectLookupTimeout)
			defer cancel()

			projectID, ok := resolveEdgeProject(ctx, w, r, cfg, user)
			if !ok {
				return
			}

			pc := ProjectContext{
				ProjectID:       projectID,
				PublicProjectID: publicProjectID,
			}
			next.ServeHTTP(w, r.WithContext(ContextWithProject(r.Context(), pc)))
		})
	}
}

// requireRuntimePrincipal proves that the identity in the request context came
// from the authentication middleware, and not from some other writer. It
// returns false when it has already written HTTP 401 to w.
//
// This restores the rule the deleted runtimecomposition.CurrentLLMCallerResolver
// enforced on the /llm edge (issue #461). That resolver called
// auth.RuntimePrincipalFromContext and refused the request when the call
// failed. The two refusals it pinned by name are restored with it:
//
//   - "plain context identity" — a user placed by auth.ContextWithUser, which
//     records no provenance.
//   - "development provenance" — auth.AuthenticationSourceDevelopment, which
//     ADR-0017 retired and which no producer may reintroduce here.
//
// The signed X-Elitea-Project-Id header does NOT make this check redundant.
// That signature proves the header was written by this process
// (internal/llmproxy/identity.go). It says nothing about how the caller
// authenticated, because the signing input is the project, the user and the
// tenant, and none of them carries the authentication source. An identity with
// no provenance is signed exactly like an identity with provenance.
//
// The check sits here because this is the single place where an identity
// becomes a billing project. auth.UserFromContext keeps its wide behaviour, so
// the many other routes that read it are unaffected.
//
// A request with NO user in the context is a different case, and Project still
// passes it through: the absence of an identity is the Auth middleware's
// decision, not this middleware's. The presence of an identity that no
// authentication path recorded is a claim, and this middleware must not act on
// a claim.
func requireRuntimePrincipal(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := auth.RuntimePrincipalFromContext(r.Context()); ok {
		return true
	}
	writeJSONError(w, http.StatusUnauthorized, "authentication_error", "unauthenticated",
		"the request identity carries no accepted authentication provenance")
	return false
}

// resolveEdgeProject returns the project to bill, or false when it has already
// written the refusal to w.
//
// The two arms are exclusive on purpose. A bound token takes the bound project
// and consults neither the resolver nor the membership checker, because both
// questions are already answered: the binding names the project, and membership
// was verified at token creation.
func resolveEdgeProject(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	cfg ProjectConfig,
	user auth.User,
) (int, bool) {
	if boundID, bound := boundProjectID(user); bound {
		if user.BoundProjectRefused() {
			return refuseInactiveBoundProject(w)
		}
		return admitSelectorAgainstBinding(w, r, boundID)
	}

	projectID, err := resolveProjectID(ctx, cfg, user)
	if err != nil || projectID <= 0 {
		writeJSONError(w, http.StatusBadRequest, errTypeInvalidRequest, codeProjectNotResolved,
			"could not resolve project for caller")
		return 0, false
	}
	return admitProjectSelector(ctx, r, cfg.Membership, user, projectID), true
}

// boundProjectID returns the project the caller's access token is bound to.
//
// Only a credential validator that read the binding from storage may set this
// field (spec §3.2). It is never derived from a header, from the token name, or
// from the principal name, so a bound value is a statement the platform made
// and not one the caller made.
//
// A binding outside the int4 project-id range names no project, so it reads as
// unbound. The stored column is an integer and cannot hold such a value; the
// guard exists so an out-of-range value can only cost the caller a fallback to
// its own project, never a claim on somebody else's.
func boundProjectID(user auth.User) (int, bool) {
	if user.TokenProjectID == nil {
		return 0, false
	}
	id := *user.TokenProjectID
	if id <= 0 || id > math.MaxInt32 {
		return 0, false
	}
	return int(id), true
}

// refuseInactiveBoundProject refuses a bound token whose project is no longer
// usable: suspended, not created, or gone.
//
// DEFECT this closes: the bound arm consulted neither the resolver nor any
// project state, and suspension revokes no binding. A token bound to project P
// therefore kept billing P's budget and spending P's provider credentials
// after an operator suspended P. Every other caller was already cut off,
// because the unbound arm runs IsCurrentUserProjectMember, which requires
// suspended IS FALSE.
//
// It REFUSES and does not fall through to the unbound path. A fallback would
// resolve the caller's personal project. It would then silently move the
// spend that the suspension was meant to stop. ADR-0018 names that
// accounting discrepancy.
//
// The message names no lifecycle column, following the token routes: the
// caller learns that the binding is unusable, not the tenant's state.
func refuseInactiveBoundProject(w http.ResponseWriter) (int, bool) {
	writeJSONError(w, http.StatusForbidden, errTypePermission, codeProjectInactive,
		"the project this access token is bound to is not available. "+
			"Ask an administrator, or use a token that is not bound to it.")
	return 0, false
}

// admitSelectorAgainstBinding applies spec §5 rows 1-3 for a bound token.
//
//   - No usable selector: proceed on the binding (row 1).
//   - Selector equal to the binding: proceed on the binding (row 2).
//   - Selector naming a different project: refuse with HTTP 400 (row 3).
//
// Row 3 is the only refusal the /llm project path produces, and it fires only
// when the caller actually named a project. A selector that parses to nothing —
// blank, non-numeric, non-positive, wider than int4, or a repeated header — is
// absent by §6.2, for a bound token exactly as for an unbound one. No project
// was named, so there is nothing to contradict.
//
// The refusal exists because ignoring the header would bill the bound project
// while the caller believes it redirected the spend. That divergence surfaces
// later as an accounting discrepancy nobody can attribute (ADR-0018). A 400 is
// legible at the call site.
func admitSelectorAgainstBinding(w http.ResponseWriter, r *http.Request, boundID int) (int, bool) {
	requestedID, present := projectSelectorFromHeaders(r.Header)
	if !present || requestedID == boundID {
		return boundID, true
	}

	// The message names both projects, because the whole reason this rejects
	// instead of ignoring is that the caller learns which two statements
	// disagreed (spec §8). It says nothing about whether the caller belongs to
	// the selector project: no membership query ran, and the answer would tell
	// an unbound-token holder which projects exist and who is in them.
	writeJSONError(w, http.StatusBadRequest, errTypeInvalidRequest, codeProjectScopeConflict,
		fmt.Sprintf(
			"the access token is bound to project %d, and the request selects project %d. "+
				"Send no project selector, or select project %d.",
			boundID, requestedID, boundID,
		))
	return 0, false
}

// admitProjectSelector applies the caller-supplied project selector to
// resolvedID and returns the project to bill. It never fails the request.
//
// The outcomes, from spec-llm-project-scope §5 rows 4-7:
//   - No selector: return resolvedID unchanged. This is the whole behaviour of
//     the path before issue #318, so a caller that sends no selector sees no
//     change at all.
//   - Selector names the caller's own resolved project: admit it without a
//     membership query. The caller is already entitled to that project.
//   - Selector names another project the caller belongs to: admit it.
//   - Selector names a project the caller does NOT belong to: return resolvedID,
//     in silence.
//   - The membership check cannot run, or it errors: return resolvedID, in
//     silence.
//
// An inadmissible selector is ignored and is never refused. Issue #318 requires
// this: its "done means" says a non-member selector must be "not honoured, not
// 500", because the fix must not add a failure mode for an existing caller. The
// UI's Node and Python samples pass the OpenAI SDK "project" option today, and
// both SDKs send that option as the OpenAI-Project header. Every caller of those
// samples would break on a 403.
//
// The silent path always falls back to the CALLER'S OWN project, never to the
// selector. A membership check that times out must not authorize spend on an
// arbitrary project (spec §7 invariant 5).
//
// Refusal is reserved for one case, which this function never reaches: a token
// explicitly BOUND to a project, plus a selector that contradicts the binding
// (spec §5 row 3). That is a conflict between two explicit statements, and
// admitSelectorAgainstBinding handles it. A bound token never arrives here.
func admitProjectSelector(
	ctx context.Context,
	r *http.Request,
	membership ProjectMembershipChecker,
	user auth.User,
	resolvedID int,
) int {
	requestedID, present := projectSelectorFromHeaders(r.Header)
	if !present {
		return resolvedID
	}
	if requestedID == resolvedID {
		return resolvedID
	}
	if !isEntitledToProject(ctx, membership, user, requestedID) {
		return resolvedID
	}
	return requestedID
}

// isEntitledToProject reports whether user may spend inside projectID.
//
// It is the single membership decision on the /llm edge. Both callers use it:
// the selector header (admitProjectSelector) and the system project-user name
// (resolveProjectID). One function keeps the two paths on one predicate, so a
// project can never enter the signed identity through a weaker test than the
// other path applies.
//
// It fails closed on every uncertainty, per spec-llm-project-scope §7
// invariant 5:
//
//   - No membership checker composed: false. A missing checker is not a licence.
//   - The owning user is unresolved: false. Membership is a property of the
//     owning user, never of the token. OwningUserID refuses to answer for a
//     token principal whose owner was not resolved.
//   - The query errors: false. A database outage must not authorize spend on an
//     arbitrary project.
func isEntitledToProject(
	ctx context.Context,
	membership ProjectMembershipChecker,
	user auth.User,
	projectID int,
) bool {
	if membership == nil {
		return false
	}
	owningUserID, resolved := user.OwningUserID()
	if !resolved {
		return false
	}
	member, err := membership.IsProjectMember(ctx, owningUserID, projectID)
	return err == nil && member
}

// projectSelectorFromHeaders returns the project named by the first accepted
// selector header that is present.
//
// A selector that does not name a project is reported as absent (spec §6.2):
// a blank value, a value that is not a positive int32, or a header that appears
// more than once. The repeated-header rule follows uniqueForwardedIdentityHeader
// in auth.go, the posture this service already applies to an identity-bearing
// header: when two copies disagree, no rule can say which copy the caller meant.
//
// A present header that names no project does not fall through to the next
// header. §6.1 picks the selector by the first header present, and §6.2 then
// judges that one value.
func projectSelectorFromHeaders(h http.Header) (int, bool) {
	for _, name := range projectSelectorHeaders {
		value, present, unique := uniqueProjectSelectorHeader(h, name)
		if !present {
			continue
		}
		if !unique {
			return 0, false
		}
		return positiveProjectID(value)
	}
	return 0, false
}

// uniqueProjectSelectorHeader returns the single trimmed value of the header
// name, whether the header is present, and whether it appears exactly once. A
// blank value counts as absent.
func uniqueProjectSelectorHeader(h http.Header, name string) (string, bool, bool) {
	var values []string
	for key, current := range h {
		if strings.EqualFold(key, name) {
			values = append(values, current...)
		}
	}
	if len(values) > 1 {
		return "", true, false
	}
	if len(values) == 0 {
		return "", false, true
	}
	value := strings.TrimSpace(values[0])
	if value == "" {
		return "", false, true
	}
	return value, true, true
}

// positiveProjectID parses a selector as a positive project id. Project ids are
// int4 in PostgreSQL, so a larger value is invalid and never reaches the query.
func positiveProjectID(value string) (int, bool) {
	id, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || id <= 0 || id > math.MaxInt32 {
		return 0, false
	}
	return id, true
}

// resolveProjectID derives the caller's project id, mirroring pylon's
// prepare_request token branch:
//   - system project-user name (":system:project:<id>:") → parse the id from the
//     name, and admit it only after the membership check
//   - otherwise, or when the membership check refuses → personal project lookup
//     via the resolver
//
// It returns 0 (with no error) when the project cannot be determined.
//
// # The membership check on the name branch (issue #459)
//
// The name branch used to return its id with no check. That id became the
// resolved project, and the resolved project becomes the signed
// X-Elitea-Project-Id header (internal/llmproxy/identity.go). The gateway
// spends that project's budget and decrypts that project's provider
// credentials. So the branch was an unchecked claim on another project. Any
// writer of auth.User.Name could make that claim.
//
// The branch was latent and not reachable: authsvc.New has no non-test caller,
// RouterConfig.AuthClient is never assigned, and the Pylon Redis RPC client is
// the one non-test writer of the field. Latent is not safe. deploy/centry-hybrid
// composes a Pylon auth plane, and a hybrid deployment that composes that client
// makes the branch live at once.
//
// The check is the same predicate the selector header passes, through the same
// function. A name-derived project therefore has exactly the power an
// X-Project-Id header has, and no more: it names a project the caller belongs
// to, or it is ignored.
//
// # Why the branch is checked and not deleted
//
// ADR-0018 and spec-llm-project-scope §5.1 delete this branch. That deletion is
// rollout Stage 3, and spec §11 states that Stage 3 MUST NOT run before Stage 2
// (the token binding) is live in production. This change cannot prove that
// production condition, so it makes the branch safe now and leaves the deletion
// to Stage 3. A checked branch is not the end state. It is the end state minus
// one scheduled removal.
func resolveProjectID(ctx context.Context, cfg ProjectConfig, user auth.User) (int, error) {
	if id, ok := projectIDFromUserName(user.Name); ok {
		if isEntitledToProject(ctx, cfg.Membership, user, id) {
			return id, nil
		}
		// The name asked for a project the caller may not use. Fall through to
		// the caller's own project. The refusal is silent, for the same reason the
		// selector refusal is silent (spec §5 row 5): a real Pylon system
		// project-user resolves to the same project through the
		// system_user_(\d+)@centry.user email fallback inside the resolver, so
		// the fallback is the parity path and not a failure.
	}

	if cfg.Resolver == nil {
		return 0, nil
	}
	return cfg.Resolver.PersonalProjectID(ctx, user.ID)
}

// projectIDFromUserName parses the project id from a system project-user name of
// the form ":system:project:<id>:". The trailing colon means the id is the
// second-to-last colon-separated field, matching pylon's name.split(":")[-2].
func projectIDFromUserName(name string) (int, bool) {
	if !strings.HasPrefix(name, projectUserNamePrefix) {
		return 0, false
	}
	parts := strings.Split(name, ":")
	if len(parts) < 2 {
		return 0, false
	}
	id, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// PublicProjectID resolves the platform's shared project — the project whose
// `shared = true` configuration rows every other project may use.
//
// EXPORTED because it now has a second caller. The Project middleware needs it
// to fill `ProjectContext`, and the composition root needs it to tell the
// configurations router which schema the shared block reads and which schema
// the admin provider surface writes. Two copies of "read AI_PROJECT_ID, default
// to 1" is exactly the drift that would let one surface publish a credential
// into a schema another surface never reads.
func PublicProjectID() int {
	return publicProjectIDFromEnv()
}

// publicProjectIDFromEnv resolves the shared project id.
//
// FOUR NAMES resolve to ONE project, and that is a correction rather than a
// convenience. This service read `AI_PROJECT_ID`, the LLM gateway reads
// `ELITEA_AI_PROJECT_ID`, and internal/api/v2/eliteacore read
// `PUBLIC_PROJECT_ID` and `SHARED_PROJECT_ID` — four variables for one project,
// with three different defaults and nothing checking that they agreed.
//
// That divergence is silent and total for anything published INTO the shared
// project. The admin panel's platform-provider surface writes a credential into
// `p_{one value}`; the gateway resolves shared credentials out of
// `p_{another}`. When the two differ, the credential is stored correctly,
// listed correctly, reported healthy, and resolves for nobody.
//
// internal/publicproject holds the single resolution, and cmd/elitea-main
// REFUSES TO START when two of the names disagree, so a running process has
// already proved that every surface below reads one project. The DEFAULT is
// unchanged at 1: this value also drives the configurations list's shared block
// and the project middleware's ProjectContext, both of which have behaved as
// project 1 since before the gateway existed.
func publicProjectIDFromEnv() int {
	return publicproject.ID()
}
