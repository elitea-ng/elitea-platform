package sharedchat

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// UnlockCookieName is the cookie the unlock endpoint sets and the view endpoint
// reads. HttpOnly, SameSite=Lax, Path-scoped to the anonymous surface: it is
// not readable by page script and is not attached to any authenticated route.
const UnlockCookieName = "elitea_shared_chat_grant"

// unlockCookiePath scopes the grant cookie to the anonymous API surface. A
// cookie on "/" would ride every request the browser makes to this origin,
// including the authenticated API, for no benefit.
const unlockCookiePath = "/api/v2/elitea_core/shared_chat_view"

// maxLinkLifetime caps how long a published conversation stays readable.
//
// DIVERGENCE FROM THE REFERENCE, TOWARDS THE SAFER OPTION. The SPA this port
// follows offers an expiry SELECT and, since its list view renders "Never
// expires" for a link with no expires_at, admits a link with no end of life at
// all. A bearer credential for conversation content with no expiry is one the
// issuer stops thinking about within a day and cannot be reminded of; the row's
// expires_at is therefore NOT NULL (migrations/shared/0100) and this constant
// is the ceiling the API will accept.
const maxLinkLifetime = 30 * 24 * time.Hour

const defaultLinkLifetime = 7 * 24 * time.Hour

// minPasswordLength mirrors the reference dialog's client-side rule. It is
// enforced HERE as well because the dialog is not the only possible caller and
// a client-side minimum is a suggestion.
const minPasswordLength = 8

// maxSharedMessages bounds one anonymous response.
//
// The view is unauthenticated, so the work one request can ask for is the work
// an anonymous caller can ask for. A conversation is unbounded in length; this
// is not.
const maxSharedMessages = 500

// Handler serves both halves of the feature. The owner-facing methods (List,
// Create, Revoke) are mounted behind authentication and a project permission;
// View and Unlock are mounted where no authentication middleware runs.
type Handler struct {
	store      Store
	transcript TranscriptStore
	// grantSecret keys the unlock cookie's HMAC. Empty means password-
	// protected links cannot be unlocked at all — Unlock refuses rather than
	// falling back to an unkeyed or guessable grant. Creating one is still
	// allowed: the link simply cannot be opened until the deployment has a
	// session secret, which is a visible failure rather than a silent
	// downgrade to no password.
	grantSecret []byte
	now         func() time.Time

	// Unlock admission (admission.go): a process-wide cap on password
	// verifications running at once, and a per-client attempt budget.
	verify   func(password string, hash, salt []byte) bool
	gate     *verifyGate
	attempts *attemptBudget
	clients  ClientAddressResolver
}

func NewHandler(store Store, transcript TranscriptStore, grantSecret []byte) *Handler {
	h := &Handler{
		store: store, transcript: transcript, grantSecret: grantSecret, now: time.Now,
		verify:   verifyPassword,
		gate:     newVerifyGate(currentVerifySlots()),
		attempts: newAttemptBudget(unlockAttemptsPerWindow, unlockAttemptWindow, maxTrackedUnlockClients),
	}
	h.attempts.now = h.clock
	return h
}

// WithClientAddresses sets how Unlock names the caller for its attempt
// budget. Without one the socket peer is used; X-Forwarded-For is never read
// by this package.
func (h *Handler) WithClientAddresses(r ClientAddressResolver) *Handler {
	h.clients = r
	return h
}

// WithVerifySlots replaces the cap on concurrent password verifications.
// Test-only; production uses the GOMAXPROCS-derived default.
func (h *Handler) WithVerifySlots(n int) *Handler {
	h.gate = newVerifyGate(n)
	return h
}

// WithAttemptBudget replaces the per-client unlock budget. Test-only.
func (h *Handler) WithAttemptBudget(attempts int, window time.Duration, maxClients int) *Handler {
	h.attempts = newAttemptBudget(attempts, window, maxClients)
	h.attempts.now = h.clock
	return h
}

// WithPasswordVerifier replaces the key derivation Unlock runs. Test-only.
func (h *Handler) WithPasswordVerifier(verify func(password string, hash, salt []byte) bool) *Handler {
	h.verify = verify
	return h
}

// WithClock replaces the handler's clock. Test-only; production leaves it.
func (h *Handler) WithClock(now func() time.Time) *Handler {
	h.now = now
	return h
}

func (h *Handler) clock() time.Time {
	if h.now == nil {
		return time.Now()
	}
	return h.now()
}

// ---------------------------------------------------------------- owner side

// List answers the share links on one conversation.
//
// It is the OWNER's view and returns no token, in any form. The reference SPA's
// "Manage links" dialog rebuilds a copyable URL from a `token` field this
// response deliberately does not have; the divergence is recorded on
// CreateResult.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	conversationID, err := strconv.ParseInt(chi.URLParam(r, "conversationID"), 10, 64)
	if err != nil || conversationID <= 0 {
		apierr.Write(w, apierr.BadRequest("conversation id must be a positive integer"))
		return
	}
	links, err := h.store.ListByConversation(r.Context(), projectID, conversationID)
	if err != nil {
		apierr.Write(w, apierr.Internal("failed to list share links"))
		return
	}
	if links == nil {
		links = []Link{}
	}
	writeJSON(w, http.StatusOK, links)
}

type createRequest struct {
	Expiry          string  `json:"expiry"`
	Scope           string  `json:"scope"`
	Password        string  `json:"password"`
	MessageGroupIDs []int64 `json:"message_group_ids"`
}

// CreateResult is the ONLY response that ever carries a token.
//
// DIVERGENCE FROM THE REFERENCE, TOWARDS THE SAFER OPTION. The reference's list
// endpoint returns `token` on every row, so its store necessarily holds the
// token in a recoverable form and an owner can re-copy a link forever. Here the
// database holds only SHA-256 of the token (migrations/shared/0100), so the
// plaintext exists in exactly one response and then nowhere. The cost is real
// and is the intended trade: an owner who loses the link revokes it and issues
// another, instead of the deployment carrying a table of live credentials for
// every conversation anyone ever shared.
type CreateResult struct {
	Link
	// Token is the bearer credential. Shown once.
	Token string `json:"token"`
}

// Create publishes a conversation behind a fresh token.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	conversationID, err := strconv.ParseInt(chi.URLParam(r, "conversationID"), 10, 64)
	if err != nil || conversationID <= 0 {
		apierr.Write(w, apierr.BadRequest("conversation id must be a positive integer"))
		return
	}

	var body createRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}

	scope := body.Scope
	if scope == "" {
		scope = "all"
	}
	if scope != "all" && scope != "partial" {
		apierr.Write(w, apierr.BadRequest("scope must be 'all' or 'partial'"))
		return
	}
	groupIDs := body.MessageGroupIDs
	if scope == "partial" {
		if len(groupIDs) == 0 {
			apierr.Write(w, apierr.BadRequest("a partial share must name at least one message group"))
			return
		}
	} else {
		// An 'all' link stores no group list. Keeping one would create two
		// sources of truth for what the link exposes, and the wider one wins
		// silently on every read.
		groupIDs = nil
	}

	lifetime, ok := parseExpiry(body.Expiry)
	if !ok {
		apierr.Write(w, apierr.BadRequest("expiry must be one of 1h, 1d, 7d, 30d"))
		return
	}

	var passwordHash, passwordSalt []byte
	if password := strings.TrimSpace(body.Password); password != "" {
		if len(password) < minPasswordLength {
			apierr.Write(w, apierr.BadRequest("password must be at least 8 characters"))
			return
		}
		passwordHash, passwordSalt, err = hashPassword(password)
		if err != nil {
			apierr.Write(w, apierr.Internal("failed to create share link"))
			return
		}
	}

	token, tokenHash, err := newToken()
	if err != nil {
		apierr.Write(w, apierr.Internal("failed to create share link"))
		return
	}

	createdBy := ""
	if user, ok := auth.UserFromContext(r.Context()); ok {
		createdBy = user.ID
	}

	link, err := h.store.Create(r.Context(), CreateInput{
		ProjectID:       projectID,
		ConversationID:  conversationID,
		Scope:           scope,
		MessageGroupIDs: groupIDs,
		TokenHash:       tokenHash,
		PasswordHash:    passwordHash,
		PasswordSalt:    passwordSalt,
		CreatedBy:       createdBy,
		ExpiresAt:       h.clock().Add(lifetime).UTC(),
	})
	if err != nil {
		apierr.Write(w, apierr.Internal("failed to create share link"))
		return
	}

	writeJSON(w, http.StatusCreated, CreateResult{Link: link, Token: token})
}

// Revoke stops a link working, immediately and permanently.
func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	conversationID, err := strconv.ParseInt(chi.URLParam(r, "conversationID"), 10, 64)
	if err != nil || conversationID <= 0 {
		apierr.Write(w, apierr.BadRequest("conversation id must be a positive integer"))
		return
	}
	linkID, err := strconv.ParseInt(chi.URLParam(r, "linkID"), 10, 64)
	if err != nil || linkID <= 0 {
		apierr.Write(w, apierr.BadRequest("link id must be a positive integer"))
		return
	}
	// Scoped by project AND conversation, not by id alone: the caller's
	// authorisation was resolved against {projectID}/{conversationID}, so the
	// row the statement is allowed to touch has to be constrained by the same
	// pair. A `WHERE id = $1` would let a member of one conversation revoke a
	// link on another by guessing a serial.
	if err := h.store.Revoke(r.Context(), projectID, conversationID, linkID); err != nil {
		if errors.Is(err, ErrNoLink) {
			apierr.Write(w, apierr.NotFound("share link not found"))
			return
		}
		apierr.Write(w, apierr.Internal("failed to revoke share link"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------ anonymous side

type viewResponse struct {
	ConversationName string          `json:"conversation_name"`
	ExpiresAt        time.Time       `json:"expires_at"`
	Messages         []SharedMessage `json:"messages"`
}

type lockedResponse struct {
	PasswordRequired bool `json:"password_required"`
}

// View serves a shared conversation to a caller holding nothing but a token.
//
// # THREAT MODEL
//
// This is the only route in this service that returns a project's own content
// to a caller with no session, no principal, no membership and no permission.
// Everything below follows from that.
//
//  1. THE TOKEN IS THE ENTIRE CREDENTIAL, so it must be unguessable. It is 256
//     bits from crypto/rand (token.go), base64url-encoded. It is NOT the
//     conversation's serial id, NOT its uuid, and NOT anything derived from
//     either: those already travel through logs, referrers and support tickets
//     attached to the very conversation the link exposes, and a uuid derived
//     from the conversation would make one leak into a permanent share.
//
//  2. WRONG, REVOKED AND EXPIRED ARE ONE ANSWER. All three return 404 with the
//     same body. Distinguishing them would turn this route into an oracle: a
//     403 "revoked" or a 410 "expired" confirms that a guessed token was once
//     REAL, which is precisely the bit an enumerating attacker wants, and it
//     also tells a former holder of a revoked link that the conversation still
//     exists. The Store collapses the three cases before they reach here
//     (ErrNoLink) so that no later edit can accidentally split them.
//
//     This is a DIVERGENCE from the reference SPA, which branches on 410 to
//     render "Link expired". The page in this repository renders the single
//     "no longer available" state instead. The safer behaviour won.
//
//  3. THE RESPONSE IS AN ENUMERATED PROJECTION, NEVER A PASS-THROUGH. What is
//     returned: the conversation's display name, the link's own expiry, and for
//     each in-scope message group its ordinal, author type and display name,
//     participant type, timestamp, error flag, and its text/canvas items plus
//     attachment NAMES.
//
//     What is deliberately absent, each because an anonymous holder has no use
//     for it and every one of them is an identifier or a disclosure:
//     the project id, the conversation id or uuid, the real message-group and
//     item ids (the `id` field is an ordinal within the response), participant
//     ids, participant emails, agent/model/credential names beyond the
//     participant's display name, storage buckets and object keys, message
//     metadata, task ids, and execution traces. The error FLAG is passed but
//     the error TEXT is not: an upstream error routinely quotes the offending
//     fragment of the request back, which is the same reasoning
//     migrations/shared/0099 records for the gateway's request log.
//
//     ATTACHMENT BYTES ARE NOT SERVED. The reference builds an anonymous
//     download URL per attachment; this port does not mount that route, and
//     emits the file's name and type only. A second unauthenticated byte-
//     serving surface is a larger risk than the feature is worth, and the name
//     is enough for the page to say what was attached.
//
//  4. THE LINK IS A POINTER, NOT A COPY. The transcript is read live through
//     the project id stored on the row, so deleting a message really does
//     un-share it. The project id comes from the ROW, never from the request:
//     an anonymous caller must not be able to steer which tenant schema is
//     queried.
//
//  5. PASSWORD-PROTECTED LINKS answer with `{"password_required": true}` and
//     nothing else — not the conversation name, not the message count, not the
//     expiry. A locked link discloses only that a lock exists, which the URL
//     already implies.
//
//  6. WORK BOUNDS. The unlock route is bounded in-process (admission.go): a
//     per-client attempt budget and a cap on concurrent verifications. This
//     read has no KDF, so its bounds are structural: the token space makes
//     guessing infeasible, the response is capped at maxSharedMessages
//     groups, and the token lookup is a single indexed equality on one central
//     table. An edge rate limit on `/api/v2/elitea_core/shared_chat_view*`
//     remains worth adding at the ingress, and is called out here rather than
//     silently assumed.
func (h *Handler) View(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if !validToken(token) {
		// Same body as a token that simply does not exist. A distinct
		// "malformed" answer would let a caller probe the token FORMAT.
		refuseAnonymous(w)
		return
	}
	tokenHash := hashToken(token)

	link, err := h.store.ResolveByTokenHash(r.Context(), tokenHash)
	if err != nil {
		if errors.Is(err, ErrNoLink) {
			refuseAnonymous(w)
			return
		}
		apierr.Write(w, apierr.Internal("failed to load shared conversation"))
		return
	}

	if len(link.PasswordHash) > 0 {
		cookie, _ := r.Cookie(UnlockCookieName)
		presented := ""
		if cookie != nil {
			presented = cookie.Value
		}
		if !grantValid(h.grantSecret, tokenHash, presented) {
			// 401 with password_required, matching the reference page's
			// unlock prompt. It discloses only that this URL is locked.
			writeJSON(w, http.StatusUnauthorized, lockedResponse{PasswordRequired: true})
			return
		}
	}

	if h.transcript == nil {
		apierr.Write(w, apierr.Internal("failed to load shared conversation"))
		return
	}
	name, messages, err := h.transcript.SharedTranscript(r.Context(), link.ProjectID, link.ConversationID, link.MessageGroupIDs)
	if err != nil {
		apierr.Write(w, apierr.Internal("failed to load shared conversation"))
		return
	}
	if messages == nil {
		messages = []SharedMessage{}
	}
	if len(messages) > maxSharedMessages {
		messages = messages[:maxSharedMessages]
	}
	for i := range messages {
		messages[i].ID = i
	}

	// Best-effort, and AFTER the read has succeeded. A failure to count an
	// access must never turn a working link into an error; the counter is
	// owner-facing accounting, not part of the authorisation decision.
	_ = h.store.RecordAccess(r.Context(), link.ID)

	writeJSON(w, http.StatusOK, viewResponse{
		ConversationName: name,
		ExpiresAt:        link.ExpiresAt,
		Messages:         messages,
	})
}

type unlockRequest struct {
	Password string `json:"password"`
}

// Unlock exchanges a link password for a grant cookie.
//
// # THREAT MODEL
//
//  1. A WRONG PASSWORD AND A NONEXISTENT LINK ARE ONE ANSWER: 403, same body.
//     A 404 for the second would confirm which guessed tokens are real, which
//     is the enumeration oracle View is built to deny — and it would be
//     absurd to close it on the GET and open it on the POST.
//
//  2. NEITHER IS A TIMING ORACLE. The expensive part of this handler is the
//     KDF, so a path that skipped it would be measurably faster and would
//     announce "no such link" (or "this link has no password") to anyone with
//     a stopwatch. So every path that can name a link derives a key: a
//     missing link and a password-less link are both verified against a decoy
//     salt, and the comparison itself is constant-time (token.go's
//     verifyPassword). The one exception is a token whose SHAPE is wrong
//     (validToken): the shape is public, no link can have it, and refusing it
//     early tells a caller nothing it did not know.
//
//     BOUNDED WORK. The KDF is the cost of this route, and the caller has no
//     session, so admission is checked before the store is read and before
//     any key is derived: a per-client attempt budget and a process-wide cap
//     on verifications in flight (admission.go). Either one answers 429 with
//     Retry-After and a fixed message.
//
//  3. THE GRANT IS BOUND TO ONE TOKEN. It is an HMAC over that token's hash
//     keyed by the deployment's session secret, so unlocking one shared
//     conversation grants nothing on another, and a cookie stolen from one
//     page is useless against a different link. It carries no authority beyond
//     "this password was entered": View re-checks revocation and expiry on
//     every request regardless of the cookie, so a grant cannot outlive the
//     link it was issued for.
//
//  4. THE COOKIE IS HttpOnly, SameSite=Lax and Path-scoped to the anonymous
//     view surface. Secure is set whenever the request arrived over TLS, so a
//     plain-HTTP development stack still works while a real deployment never
//     emits a non-Secure grant.
//
//  5. WITH NO SESSION SECRET CONFIGURED, THIS REFUSES rather than issuing an
//     unkeyed grant. An unkeyed or guessable grant is worse than no unlock at
//     all, because the password would appear to be enforced.
func (h *Handler) Unlock(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")

	var body unlockRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}

	// A token of the wrong shape cannot name a link. Its shape is public, so
	// skipping the KDF leaks nothing; the answer is the one a well-formed
	// unknown token gets.
	if !validToken(token) {
		apierr.Write(w, apierr.Forbidden("incorrect password"))
		return
	}

	// Admission: before the store and before the KDF. The client is charged
	// first so one client cannot hold the verification slots; a request
	// refused only for lack of a slot is given its attempt back.
	key := clientKey(r, h.clients)
	if ok, retry := h.attempts.take(key); !ok {
		writeTooMany(w, retry, tooManyAttemptsMessage)
		return
	}
	release, ok := h.gate.tryAcquire()
	if !ok {
		h.attempts.refund(key)
		writeTooMany(w, verifyBusyRetryAfter, busyMessage)
		return
	}
	defer release()

	// decoySalt makes the "no such link" and "no password on this link" paths
	// cost the same as a real verification. It is a fixed value rather than a
	// random one on purpose: it is never stored and never compared against
	// anything, so its only job is to make the KDF run.
	decoySalt := []byte("shared-chat-unlock-decoy-salt-16")

	hash, salt := []byte(nil), decoySalt
	var resolved Resolved
	var resolvedOK bool

	link, err := h.store.ResolveByTokenHash(r.Context(), hashToken(token))
	switch {
	case err == nil:
		resolved, resolvedOK = link, true
		if len(link.PasswordHash) > 0 {
			hash, salt = link.PasswordHash, link.PasswordSalt
		}
	case errors.Is(err, ErrNoLink):
		// fall through to the decoy verification below
	default:
		apierr.Write(w, apierr.Internal("failed to unlock shared conversation"))
		return
	}

	// Runs on every well-formed token, including the ones that cannot succeed.
	verified := h.verify(body.Password, hash, salt)
	if !verified || !resolvedOK || len(resolved.PasswordHash) == 0 {
		apierr.Write(w, apierr.Forbidden("incorrect password"))
		return
	}

	if len(h.grantSecret) == 0 {
		apierr.Write(w, apierr.Internal("shared conversation unlock is not configured"))
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     UnlockCookieName,
		Value:    grantValue(h.grantSecret, hashToken(token)),
		Path:     unlockCookiePath,
		HttpOnly: true,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// writeTooMany answers a request refused by admission: 429, Retry-After and a
// fixed message, never anything about the link.
func writeTooMany(w http.ResponseWriter, retryAfter time.Duration, message string) {
	w.Header().Set("Retry-After", retryAfterSeconds(retryAfter))
	apierr.WriteStatus(w, http.StatusTooManyRequests, message)
}

// refuseAnonymous is the single refusal for every "this token buys nothing"
// case: unknown, malformed, revoked, expired. One function so the four can
// never drift apart.
func refuseAnonymous(w http.ResponseWriter) {
	apierr.Write(w, apierr.NotFound("shared conversation not found"))
}

// parseExpiry maps the reference dialog's expiry vocabulary onto a duration.
//
// A value the server does not recognise is REFUSED, not defaulted: silently
// substituting a default for an unparsed expiry is how a caller that meant
// "one hour" gets thirty days.
func parseExpiry(value string) (time.Duration, bool) {
	switch strings.TrimSpace(value) {
	case "":
		return defaultLinkLifetime, true
	case "1h":
		return time.Hour, true
	case "1d":
		return 24 * time.Hour, true
	case "7d":
		return 7 * 24 * time.Hour, true
	case "30d":
		return maxLinkLifetime, true
	default:
		return 0, false
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// The anonymous view must never be cached by a shared proxy: the URL is
	// the credential, so a cached body is a copy of private content sitting
	// under a key an intermediary already knows.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
