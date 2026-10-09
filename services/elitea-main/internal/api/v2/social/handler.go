package social

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/personalproject"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/contextsettings"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/publicproject"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type Handler struct {
	pool *pgxpool.Pool
	// personalProject creates the caller's personal project when they have
	// none. EnsureAsync returns immediately: provisioning applies a whole
	// tenant migration corpus, and this is a read endpoint the SPA polls. See
	// GetAuthor.
	personalProject personalproject.AsyncEnsurer

	// personalProjectWait bounds the wait GetAuthor puts on an ensure it just
	// started. Zero means defaultPersonalProjectWait, the production value.
	personalProjectWait time.Duration

	// projectAccess answers the project-membership gate in front of the
	// {projectID} routes. Nil means the database-backed gate over pool.
	projectAccess apimw.ProjectAccessQuerier

	// permissions resolves the feedback list/create grants. Nil fails the
	// feedback routes closed (503).
	permissions auth.PermissionResolver
}

// Option configures a Handler at construction time.
type Option func(*Handler)

// WithPersonalProjectEnsurer wires the personal-project provisioner.
//
// It is optional in the constructor and REQUIRED in practice: without it
// `GET /social/author` reports the personal project a fresh account does not
// have, forever, and the SPA parks that account on `/onboarding` waiting for a
// project nothing will create. It is an option only because a composition
// without a database pool cannot build one — the same gate the project-create
// route uses.
func WithPersonalProjectEnsurer(ensurer personalproject.AsyncEnsurer) Option {
	return func(h *Handler) { h.personalProject = ensurer }
}

// WithPersonalProjectWait replaces the bounded wait GetAuthor puts on an
// ensure it started. Zero and negative values are ignored.
//
// It exists for the integration suite, which must be able to state the two
// outcomes SEPARATELY: a wait long enough to prove the first read reports the
// project, and a wait short enough to prove the endpoint still answers "" and
// lets the poll finish the job. A test that took the production value would
// measure how fast the machine applies a migration corpus.
func WithPersonalProjectWait(wait time.Duration) Option {
	return func(h *Handler) {
		if wait > 0 {
			h.personalProjectWait = wait
		}
	}
}

// WithProjectAccessQuerier replaces the query behind the project-membership
// gate. Production composes none (the gate reads the pool); router-level tests
// inject the membership answer, as they do for the /elitea_core group. A nil
// querier is ignored.
func WithProjectAccessQuerier(querier apimw.ProjectAccessQuerier) Option {
	return func(h *Handler) {
		if querier != nil {
			h.projectAccess = querier
		}
	}
}

// WithPermissionResolver wires the RBAC resolver behind the feedback routes
// (models.social.feedbacks.list / .create). Without it they answer 503.
func WithPermissionResolver(resolver auth.PermissionResolver) Option {
	return func(h *Handler) { h.permissions = resolver }
}

func NewHandler(pool *pgxpool.Pool, options ...Option) *Handler {
	handler := &Handler{pool: pool}
	for _, option := range options {
		option(handler)
	}
	return handler
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/author/", h.GetAuthor)
	r.Get("/author", h.GetAuthor)
	r.Put("/author/", h.UpdateAuthor)
	r.Put("/author", h.UpdateAuthor)
	r.Group(func(r chi.Router) {
		if h.projectAccess != nil {
			r.Use(apimw.RequireProjectAccessWith(h.projectAccess))
		} else {
			r.Use(apimw.RequireProjectAccess(h.pool))
		}
		r.Get("/authors/{projectID}", h.ListAuthors)
		r.Get("/trending_authors/prompt_lib/{projectID}", h.TrendingAuthors)
		r.Post("/like/prompt_lib/{projectID}/application/{applicationID}", h.Like)
		r.Delete("/like/prompt_lib/{projectID}/application/{applicationID}", h.Unlike)
		r.Post("/like/prompt_lib/{projectID}/{entityType}/{entityID}", h.Like)
		r.Delete("/like/prompt_lib/{projectID}/{entityType}/{entityID}", h.Unlike)
		r.Post("/pin/prompt_lib/{projectID}/{entityType}/{entityID}", h.Pin)
		r.Delete("/pin/prompt_lib/{projectID}/{entityType}/{entityID}", h.Unpin)
		list, create := feedbackHandlers(h.pool)
		listGate := requireFeedbackPermission(h.permissions, CurrentFeedbackListPermission)
		createGate := requireFeedbackPermission(h.permissions, CurrentFeedbackCreatePermission)
		for _, path := range []string{CurrentFeedbackListSubPath, CurrentFeedbackListAliasSubPath} {
			r.With(listGate).Method(http.MethodGet, path, list)
			r.With(createGate).Method(http.MethodPost, path, create)
		}
	})
	return r
}

type AuthorResponse struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Email             string `json:"email"`
	Avatar            string `json:"avatar"`
	Description       string `json:"description"`
	PersonalProjectID string `json:"personal_project_id"`
	Personalization   any    `json:"personalization,omitempty"`

	// The user's context-management defaults, from the two jsonb columns of
	// the same name on centry.social_users. They are the author record's
	// second and third settings blocks, exactly as in pylon's UserModel
	// (legacy/plugins/social/models/pd/users.py) — and, before this, the only
	// two the Go handler neither read nor wrote. Settings › Memory saves them
	// through this endpoint, so dropping them here made every save on that
	// page vanish.
	//
	// Omitted rather than sent as `null` when the user has never saved them:
	// absence is what the client's own defaults are for.
	DefaultContextManagement *contextsettings.ContextManagement `json:"default_context_management,omitempty"`
	DefaultSummarization     *contextsettings.Summarization     `json:"default_summarization,omitempty"`

	// ProviderRefs are the caller's OWN federated identity references, exactly
	// as `auth_core__user_provider.provider_ref` stores them.
	//
	// WHY A PROFILE ENDPOINT CARRIES AN OPERATIONS VALUE. `identity.
	// initial_global_admins` names a login by this reference, it is read at
	// boot, and with Azure AD or Okta the OIDC subject inside it is an opaque
	// identifier that nobody knows in advance. Making the first administrator
	// of a fresh deployment therefore meant `SELECT provider_ref FROM
	// auth_core__user_provider` against the production database. This is that
	// same value, served to the person it belongs to, over the endpoint the SPA
	// already calls for "who am I".
	//
	// IT IS THE CALLER'S OWN, AND ONLY THEIR OWN. The lookup is keyed on the
	// authenticated principal's user id, never on the joined social row, which
	// is matched on email OR id and could name somebody else.
	//
	// Omitted when the account holds none. A password login through the Form
	// plane creates no `auth_core__user_provider` row, and `initial_global_
	// admins` has nothing to name on such an account.
	ProviderRefs []string `json:"provider_refs,omitempty"`
}

func (h *Handler) GetAuthor(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		apierr.WriteStatus(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if h.pool == nil {
		writeJSON(w, http.StatusOK, AuthorResponse{
			ID:    user.ID,
			Name:  principalDisplayName(user, ""),
			Email: user.Email,
		})
		return
	}

	ctx := r.Context()

	// Query centry.social_users joined with auth_core__user
	var resp AuthorResponse

	var contextManagement, summarization []byte
	err := h.pool.QueryRow(ctx, `
		SELECT
			COALESCE(au.name, ''),
			COALESCE(au.email, ''),
			COALESCE(su.avatar, ''),
			COALESCE(su.description, ''),
			su.personalization,
			su.default_context_management,
			su.default_summarization
		FROM centry.social_users su
		LEFT JOIN auth_core__user au ON au.id = su.user_id
		WHERE au.email = $1 OR su.user_id::text = $2
		ORDER BY (su.user_id::text = $2) DESC
		LIMIT 1
	`, user.Email, user.ID).Scan(
		&resp.Name,
		&resp.Email,
		&resp.Avatar,
		&resp.Description,
		&resp.Personalization,
		&contextManagement,
		&summarization,
	)

	switch {
	case err == nil:
		resp.DefaultContextManagement, resp.DefaultSummarization =
			readMemoryDefaults(contextManagement, summarization, resp.Personalization)
	case errors.Is(err, pgx.ErrNoRows):
		// No social_users row: fall back to the account itself. The
		// personal project is resolved below either way — it does not depend
		// on the social profile existing.
		resp = AuthorResponse{Email: user.Email}
	default:
		// A failed read is not "no profile". Answering 200 with the defaults
		// made the SPA treat empty personalization and memory settings as the
		// user's stored ones for the whole session.
		slog.ErrorContext(ctx, "social: read author profile", "err", err, "user_id", user.ID)
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to read the author profile")
		return
	}
	if strings.TrimSpace(resp.Name) == "" {
		// The account's own name before the email (UI-UX-1a). An account with
		// no social profile yet used to be greeted as "Hello, <email>!" even
		// though auth_core__user held the person's name.
		resp.Name = principalDisplayName(user, h.accountName(ctx, user))
	}

	// The identity always comes from the authenticated principal, never from
	// the joined row (which is matched on email OR id and could in principle
	// select a different user's profile).
	resp.ID = user.ID
	resp.ProviderRefs = h.resolveProviderRefs(ctx, user)
	resp.PersonalProjectID = h.resolvePersonalProjectID(ctx, user.ID)
	if resp.PersonalProjectID == "" && h.ensurePersonalProject(ctx, user) {
		// The attempt this request started has FINISHED, so the answer this
		// request can give has changed. Re-reading is the whole point: the
		// value below is what the SPA routes on, and reporting "" for a
		// project that now exists sends a first-time user to the onboarding
		// screen for five minutes of nothing.
		resp.PersonalProjectID = h.resolvePersonalProjectID(ctx, user.ID)
	}

	writeJSON(w, http.StatusOK, resp)
}

// accountName reads auth_core__user.name for the caller's own account, or ""
// when it has none or the read fails. Keyed on the owning user id, never on the
// email, for the same reason the provider references are.
func (h *Handler) accountName(ctx context.Context, user auth.User) string {
	id, ok := user.OwningUserID()
	if !ok {
		return ""
	}
	var name string
	if err := h.pool.QueryRow(ctx,
		`SELECT COALESCE(name, '') FROM auth_core__user WHERE id = $1`, id,
	).Scan(&name); err != nil {
		// Only a display name rides on this read, and the principal's own
		// name or email is a correct fallback. A real failure is still
		// logged rather than read as "the account has no name".
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.WarnContext(ctx, "social: read account name", "err", err, "user_id", id)
		}
		return ""
	}
	return strings.TrimSpace(name)
}

// principalDisplayName is the first non-blank of the account's stored name,
// the authenticated principal's name and its email.
func principalDisplayName(user auth.User, accountName string) string {
	for _, candidate := range []string{accountName, user.Name} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return user.Email
}

// startedEnsurer is the completion-aware half of *personalproject.Ensurer,
// declared at the consumer.
//
// The field stays `personalproject.AsyncEnsurer`, because the OTHER reader of
// "the caller's personal project" — api/middleware's project resolver — wants
// exactly the fire-and-forget method and nothing more. Widening the shared
// interface would force a wait it has no request to hold open for.
type startedEnsurer interface {
	EnsureStarted(userID int64) <-chan struct{}
}

// defaultPersonalProjectWait bounds how long this read waits for a personal
// project it just asked for.
//
// SHORT ON PURPOSE. The request must still answer; the wait only decides
// whether it answers with the id or with "". Three seconds is inside the
// browser's patience and well under the SPA's five-second poll, so a slower
// provisioning run degrades to exactly the behaviour that shipped before —
// "" now, the real id on a later poll — rather than to a hung request.
const defaultPersonalProjectWait = 3 * time.Second

// ensurePersonalProject asks for the caller's personal project to be created
// and waits a bounded moment for the answer. It reports whether the attempt for
// this user finished, which is the caller's cue to re-resolve.
//
// THE ATTEMPT IS NOT NECESSARILY THIS REQUEST'S. The SPA sends two of these
// requests at boot, inside the same second. EnsureStarted gives BOTH of them
// the running attempt's channel, so both wait for the same work and both
// answer the same id. It used to give the second one nil, and that request
// answered "" while its twin answered the real project — one boot, two
// contradictory answers, on the field the SPA routes on.
//
// WHY HERE. This endpoint is the one that answers "which project do your
// private things live in", it is authenticated on every plane, and the SPA
// calls it on boot and then polls it every five seconds from the onboarding
// screen while it waits — so it is both the place that observes the gap and the
// place that reports it closed. pylon triggers the same work from its auth
// layer, for every authenticated request, through the `auth_visitor` event
// (legacy/plugins/projects/events/projects.py:8).
//
// WHY IT WAITS AT ALL. The account is provisioned BY THIS REQUEST, and the
// request then reported that it had no personal project. The SPA routes on
// that field, so a first login landed on `/onboarding` ("about 5 minutes")
// while the project it was waiting for already existed; a reload went straight
// to chat. The wait closes the gap between the two answers.
//
// WHAT IT DOES NOT DO. It never cancels the provisioning — EnsureStarted keeps
// the detached deadline — and it never queues behind a full slot budget. A
// dropped attempt returns a nil channel and this function falls back to the
// previous behaviour: answer "" and let the poll ask again.
//
// It costs nothing on an account that HAS a personal project, because it is
// reached only when the resolver answered "".
func (h *Handler) ensurePersonalProject(ctx context.Context, user auth.User) bool {
	if h.personalProject == nil {
		return false
	}
	// `OwningUserID`, not a fresh parse of `user.ID`: it is this repository's
	// reviewed answer to "which auth_core__user owns this principal". It reads
	// the validated `UserID` field first and refuses a principal whose id is a
	// TOKEN id, neither of which a parse of the compatibility field can do.
	// `project_user_<token id>` would name a project nobody could be a member
	// of.
	id, ok := user.OwningUserID()
	if !ok {
		return false
	}

	awaitable, canWait := h.personalProject.(startedEnsurer)
	if !canWait {
		h.personalProject.EnsureAsync(id)
		return false
	}
	done := awaitable.EnsureStarted(id)
	if done == nil {
		// No attempt exists for this user at all — the slot budget dropped it.
		// There is nothing to wait for, so answer as this endpoint always did
		// and let the SPA's poll ask again.
		return false
	}

	wait := h.personalProjectWait
	if wait <= 0 {
		wait = defaultPersonalProjectWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// resolveProviderRefs reads the caller's own `auth_core__user_provider`
// references. See AuthorResponse.ProviderRefs for why this endpoint carries
// them.
//
// FAILURE IS SILENCE, NOT AN ERROR. This is one extra field on a response the
// SPA calls on every boot, so a database fault here must not take out the
// profile, the personal-project id, or the memory defaults beside it. The
// caller gets the field omitted, and the cause goes to the log.
//
// `OwningUserID`, not a parse of `user.ID`: it is this repository's reviewed
// answer to "which auth_core__user owns this principal", and it refuses a
// principal whose id is a TOKEN id — which would key this read on the wrong
// row entirely.
func (h *Handler) resolveProviderRefs(ctx context.Context, user auth.User) []string {
	if h.pool == nil {
		return nil
	}
	id, ok := user.OwningUserID()
	if !ok {
		return nil
	}
	rows, err := h.pool.Query(ctx, `
		SELECT provider_ref
		FROM public.auth_core__user_provider
		WHERE user_id = $1::bigint AND provider_ref IS NOT NULL
		ORDER BY provider_ref
	`, id)
	if err != nil {
		slog.Warn("social: read provider references", "err", err, "user_id", id)
		return nil
	}
	defer rows.Close()

	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			slog.Warn("social: scan provider reference", "err", err, "user_id", id)
			return nil
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("social: iterate provider references", "err", err, "user_id", id)
		return nil
	}
	return refs
}

// resolvePersonalProjectID answers "which project do this user's private
// things live in" — the value the SPA stores as `personal_project_id` and
// then uses as its default project scope (legacy parity: `slices/settings.js`
// seeds `project = {id: personal_project_id, name: 'Private'}` when nothing
// is selected, and `NotificationButton.jsx` opens its notification
// subscription against it).
//
// Resolution order, first hit wins:
//
//  1. The canonical personal project `project_user_<uid>` that the user holds
//     a project-role in — pylon's `projects_get_personal_project_id` decision
//     tree, also implemented as the `ResolveCurrentPersonalProjectID` query.
//  2. The system-user email fallback `system_user_<n>@centry.user` → <n>,
//     the second branch of that same pylon tree.
//
// AND NOTHING ELSE. There used to be a third branch — "the lowest-id project
// the user actually holds a role in" — and it is the defect issue 843 reports.
// It answered an ORDINARY SHARED PROJECT as the caller's personal one, so the
// switcher labelled a team project "Private", chat and `/llm` scoped private
// work into it, and, worse, the answer was not "": provisioning re-arms only
// when this function answers "", so an account that got a shared project here
// was never given a personal project at all. An account that is a member of
// nothing was answered "" and recovered on its next request; a member of a
// shared project never did.
//
// THE RULE THE REST OF THE PRODUCT ALREADY USES is the NAME:
// apps/elitea-web's `isPersonalProjectName` accepts `project_user_<uid>` and
// nothing else, because "is this my private project?" written as an id
// comparison answered yes for a shared project. A membership-only project is
// therefore accepted here only when it carries the caller's own personal
// name — which is exactly branch 1, so there is no branch to add: what
// branch 3 could legitimately have answered, branch 1 already answers.
//
// Every branch is membership-checked, which is the point: this value is used
// as an authorization scope by the caller, so returning a project the user is
// not a member of produces a 403 the SPA cannot recover from (issue #166).
// The previous implementation returned a hardcoded "1" in EVERY fallback
// branch (issue #167) — correct only by accident on a single-project
// deployment, and wrong the moment project 1 is not the caller's project.
//
// Returns "" when no project can be resolved. That is a truthful answer, and
// the SPA treats it as "no personal project yet".
//
// NOT covered here: PROVISIONING. This function reads; it never creates. When
// it answers "" for a live account, GetAuthor asks
// internal/application/personalproject to create the missing
// `project_user_<uid>` project in the background, and a later call resolves it
// through branch 1. Before that package existed, branch 1 could only ever fire
// for data migrated from pylon, and every account on a fresh deployment was
// answered "" for good.
func (h *Handler) resolvePersonalProjectID(ctx context.Context, userID string) string {
	uid, convErr := strconv.Atoi(userID)
	if convErr != nil || uid <= 0 {
		return ""
	}

	// One ranked candidate list rather than two sequential queries: the
	// `priority` column makes the precedence explicit and total, so the result
	// does not depend on UNION ALL branch ordering (which Postgres does not
	// guarantee), and `id IS NOT NULL` keeps a non-matching branch from
	// producing a NULL that the Scan below could not hold.
	//
	// The name comes in as a parameter built from personalproject.NamePrefix,
	// not from a `project_user_` literal spelled again here: the package that
	// WRITES that name and the two places that read it have to agree, and an
	// inline literal is how they would come to disagree silently.
	var projectID int
	err := h.pool.QueryRow(ctx, `
		SELECT candidate.id
		FROM (
		    SELECT 1 AS priority, project.id AS id
		    FROM centry.project AS project
		    WHERE project.name = $2
		      AND EXISTS (
		          SELECT 1
		          FROM public.auth_core__project_user_role AS assignment
		          WHERE assignment.project_id = project.id
		            AND assignment.user_id = $1::integer
		      )

		    UNION ALL

		    SELECT 2, substring(
		                  user_account.email
		                  FROM '^system_user_([0-9]+)@centry[.]user$'
		              )::integer
		    FROM public.auth_core__user AS user_account
		    WHERE user_account.id = $1::integer
		      AND user_account.email ~ '^system_user_[0-9]+@centry[.]user$'

		) AS candidate
		WHERE candidate.id IS NOT NULL
		ORDER BY candidate.priority, candidate.id
		LIMIT 1
	`, uid, personalproject.Name(int64(uid))).Scan(&projectID)
	if err != nil || projectID <= 0 {
		return ""
	}
	return intToStr(projectID)
}

func (h *Handler) UpdateAuthor(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		apierr.WriteStatus(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if h.pool == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx := r.Context()

	contextManagement, summarization, fieldErr := memoryDefaultsFromBody(body)
	if fieldErr != nil {
		writeFieldError(w, fieldErr)
		return
	}

	// Upsert social_users.
	//
	// The two memory columns are COALESCEd against the row's existing value
	// rather than taken from EXCLUDED outright, because a request that does
	// not mention them must not erase them: Settings › AI Personality and
	// Settings › Memory are two pages over ONE record, and the personality
	// page's payload carries no context settings at all. `personalization`,
	// `title`, `description` and `avatar` keep their prior replace-outright
	// behaviour — the SPA carries those forward itself
	// (apps/elitea-web .../settingsProfileForm.ts `buildAuthorUpdate`).
	_, err := h.pool.Exec(ctx, `
		INSERT INTO centry.social_users (user_id, title, description, avatar, personalization,
			default_context_management, default_summarization)
		SELECT au.id, $2, $3, $4, $5, $6, $7
		FROM auth_core__user au WHERE au.email = $1
		ON CONFLICT (user_id) DO UPDATE SET
			title = EXCLUDED.title,
			description = EXCLUDED.description,
			avatar = EXCLUDED.avatar,
			personalization = EXCLUDED.personalization,
			default_context_management = COALESCE(EXCLUDED.default_context_management,
				social_users.default_context_management),
			default_summarization = COALESCE(EXCLUDED.default_summarization,
				social_users.default_summarization)
	`, user.Email,
		strVal(body, "name"),
		strVal(body, "description"),
		strVal(body, "avatar"),
		jsonVal(body, "personalization"),
		contextManagement,
		summarization,
	)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to update author")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// memoryDefaultsFromBody extracts, validates and re-encodes the author
// record's two context-settings blocks.
//
// TWO ACCEPTED PLACEMENTS. Top level is the contract, and it is what pylon
// took (UserUpdateModel's own fields). NESTED INSIDE `personalization` is
// accepted too, because that is where apps/elitea-web put them while this
// handler dropped every other top-level key: its `deserializeSettingsProfile`
// wrote them into the personalization blob precisely so that something would
// survive the round trip. Refusing that shape now would silently orphan every
// setting saved by a client that has not yet shipped, so the nested placement
// is read as a fallback and rewritten into the columns on the next save.
//
// A nil result means "the request said nothing about this block", which the
// upsert turns into "keep what is stored" — NOT "clear it".
func memoryDefaultsFromBody(body map[string]any) (contextManagement, summarization []byte, fieldErr *contextsettings.FieldError) {
	rawContext, rawSummary := memoryDefaultsSource(body)

	decodedContext, fieldErr := contextsettings.DecodeContextManagement(rawContext)
	if fieldErr != nil {
		return nil, nil, fieldErr
	}
	decodedSummary, fieldErr := contextsettings.DecodeSummarization(rawSummary)
	if fieldErr != nil {
		return nil, nil, fieldErr
	}

	// Re-encoded from the DECODED value, so what reaches the column is the
	// validated contract shape rather than whatever extra keys the caller sent.
	// A nil block stays nil, which the upsert reads as "keep what is stored".
	if decodedContext != nil {
		encoded, err := json.Marshal(decodedContext)
		if err != nil {
			return nil, nil, &contextsettings.FieldError{
				Field: "default_context_management", Message: "default_context_management is not encodable"}
		}
		contextManagement = encoded
	}
	if decodedSummary != nil {
		encoded, err := json.Marshal(decodedSummary)
		if err != nil {
			return nil, nil, &contextsettings.FieldError{
				Field: "default_summarization", Message: "default_summarization is not encodable"}
		}
		summarization = encoded
	}
	return contextManagement, summarization, nil
}

// memoryDefaultsSource picks each block out of the request body, preferring
// the top-level key and falling back to the personalization-nested one.
func memoryDefaultsSource(body map[string]any) (contextManagement, summarization []byte) {
	nested, _ := body["personalization"].(map[string]any)
	return pickJSON(body, nested, "default_context_management"),
		pickJSON(body, nested, "default_summarization")
}

func pickJSON(top, nested map[string]any, key string) []byte {
	for _, source := range []map[string]any{top, nested} {
		if source == nil {
			continue
		}
		value, present := source[key]
		if !present || value == nil {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			continue
		}
		return encoded
	}
	return nil
}

// readMemoryDefaults answers GET with the columns, falling back to the
// personalization blob for a profile last written by a client that nested them
// there (see memoryDefaultsFromBody). A stored block that will not decode is
// reported as absent rather than failing the whole author read — the SPA boots
// on this endpoint.
func readMemoryDefaults(contextManagement, summarization []byte, personalization any) (
	*contextsettings.ContextManagement, *contextsettings.Summarization,
) {
	nested, _ := personalization.(map[string]any)
	if len(contextManagement) == 0 {
		contextManagement = pickJSON(nil, nested, "default_context_management")
	}
	if len(summarization) == 0 {
		summarization = pickJSON(nil, nested, "default_summarization")
	}

	decodedContext, err := contextsettings.DecodeContextManagement(contextManagement)
	if err != nil {
		decodedContext = nil
	}
	decodedSummary, summaryErr := contextsettings.DecodeSummarization(summarization)
	if summaryErr != nil {
		decodedSummary = nil
	}
	return decodedContext, decodedSummary
}

// writeFieldError answers with this API's validation shape — the same
// `{"error": ..., "field": ...}` body internal/api/v2/configurations writes.
func writeFieldError(w http.ResponseWriter, fieldErr *contextsettings.FieldError) {
	writeJSON(w, http.StatusBadRequest, struct {
		Error string `json:"error"`
		Field string `json:"field"`
	}{Error: fieldErr.Message, Field: fieldErr.Field})
}

// maxProjectAuthors bounds one project's author listing.
const maxProjectAuthors = 1000

// ListAuthors lists the people who hold a role in the named project. The
// route gate has already established that the caller is one of them. The list
// carries e-mail addresses, so it must never reach past the project's members.
// In the public project, which sign-up enrolment can give every user, only the
// caller's own row carries one.
func (h *Handler) ListAuthors(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	ctx := r.Context()
	project, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 32)
	if err != nil || project <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return
	}
	rows, err := h.pool.Query(ctx, `
		SELECT au.id, COALESCE(au.name, ''), COALESCE(au.email, ''), COALESCE(su.avatar, ''), COALESCE(su.description, '')
		FROM (SELECT DISTINCT user_id FROM auth_core__project_user_role WHERE project_id = $1) member
		JOIN auth_core__user au ON au.id = member.user_id
		LEFT JOIN centry.social_users su ON su.user_id = au.id
		WHERE au.email IS DISTINCT FROM ('system_user_' || $1::text || '@centry.user')
		ORDER BY au.id
		LIMIT $2
	`, int32(project), maxProjectAuthors)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to list authors"})
		return
	}
	defer rows.Close()

	caller := int64(-1)
	if user, ok := auth.UserFromContext(ctx); ok {
		if owner, ok := user.OwningUserID(); ok {
			caller = owner
		}
	}
	publicListing := int(project) == publicproject.ID()

	items := make([]map[string]any, 0)
	for rows.Next() {
		var id int
		var name, email, avatar, desc string
		if err := rows.Scan(&id, &name, &email, &avatar, &desc); err != nil {
			continue
		}
		item := map[string]any{
			"id": intToStr(id), "name": name, "email": email,
			"avatar": avatar, "description": desc,
		}
		if publicListing && int64(id) != caller {
			delete(item, "email")
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to list authors"})
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *Handler) TrendingAuthors(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if h.pool == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	ctx := r.Context()

	schema, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	// tenantSchema accepted the id, so it is a plain positive decimal.
	project, err := strconv.ParseInt(projectID, 10, 32)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return
	}

	// Only the project's own members are ranked: the listing carries e-mail
	// addresses, and a social profile exists for every user on the platform.
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT su.user_id, COALESCE(au.name, ''), COALESCE(au.email, ''),
			COALESCE(su.avatar, ''), COUNT(sl.id) as like_count
		FROM centry.social_users su
		JOIN auth_core__user au ON au.id = su.user_id
		LEFT JOIN %s.social_likes sl ON sl.user_id = su.user_id
		WHERE EXISTS (SELECT 1 FROM auth_core__project_user_role member
		              WHERE member.project_id = $1 AND member.user_id = su.user_id)
		  AND au.email IS DISTINCT FROM ('system_user_' || $1::text || '@centry.user')
		GROUP BY su.user_id, au.name, au.email, su.avatar
		ORDER BY like_count DESC, su.user_id
		LIMIT 10`, schema), int32(project))

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to list trending authors"})
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var id int
		var name, email, avatar string
		var likes int
		if err := rows.Scan(&id, &name, &email, &avatar, &likes); err != nil {
			continue
		}
		items = append(items, map[string]any{
			"id": intToStr(id), "name": name, "email": email,
			"avatar": avatar, "likes": likes,
		})
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to list trending authors"})
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *Handler) Like(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	entityType := chi.URLParam(r, "entityType")
	entityID := chi.URLParam(r, "entityID")
	if entityType == "" {
		entityType = "application"
	}
	if entityID == "" {
		entityID = chi.URLParam(r, "applicationID")
	}
	ctx := r.Context()

	user, ok := auth.UserFromContext(ctx)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	if h.pool == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	schema, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}

	_, err := h.pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.social_likes (entity_name, user_id, entity_id, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (entity_name, user_id, entity_id) DO NOTHING`, schema),
		entityType, user.ID, entityID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "failed to like"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) Unlike(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	entityType := chi.URLParam(r, "entityType")
	entityID := chi.URLParam(r, "entityID")
	if entityType == "" {
		entityType = "application"
	}
	if entityID == "" {
		entityID = chi.URLParam(r, "applicationID")
	}
	ctx := r.Context()

	user, ok := auth.UserFromContext(ctx)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	if h.pool == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	schema, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}

	if _, err := h.pool.Exec(ctx, fmt.Sprintf(`
		DELETE FROM %s.social_likes
		WHERE entity_name = $1 AND user_id = $2 AND entity_id = $3`, schema),
		entityType, user.ID, entityID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "failed to unlike"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) Pin(w http.ResponseWriter, r *http.Request) {
	pins := repos.NewCurrentSocialPinsRepository(h.pool)
	if err := pins.Pin(r.Context(), chi.URLParam(r, "projectID"), chi.URLParam(r, "entityType"), chi.URLParam(r, "entityID")); err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) Unpin(w http.ResponseWriter, r *http.Request) {
	pins := repos.NewCurrentSocialPinsRepository(h.pool)
	if err := pins.Unpin(r.Context(), chi.URLParam(r, "projectID"), chi.URLParam(r, "entityType"), chi.URLParam(r, "entityID")); err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func intToStr(i int) string {
	return fmt.Sprintf("%d", i)
}

func strVal(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func jsonVal(m map[string]any, key string) []byte {
	if v, ok := m[key]; ok {
		b, _ := json.Marshal(v)
		return b
	}
	return []byte("null")
}
