package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	identity "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

const (
	maxTokenRequestBodyBytes = 4 << 10
	maxTokenNameBytes        = 768
)

var (
	errTokenNotFound  = errors.New("token not found")
	errTokenForbidden = errors.New("token belongs to another user")
	// errTokenProjectForbidden reports that the token owner is not a member of
	// the requested project. The membership check runs inside the creating
	// transaction, so the request creates no token row.
	errTokenProjectForbidden = errors.New("token owner is not a member of the requested project")
)

type Token struct {
	ID      int64      `json:"id"`
	UUID    *string    `json:"uuid"`
	Expires *time.Time `json:"expires"`
	UserID  int64      `json:"user_id"`
	Name    *string    `json:"name"`
	// ProjectID is the project this key bills, or null when the key is unbound.
	// It is reported without omitempty so a client can tell "unbound" from "a
	// server that does not know about bindings" (spec-llm-project-scope §4).
	ProjectID *int64 `json:"project_id"`
	Token     string `json:"token"`
}

type tokenRecord struct {
	ID        int64
	UUID      *string
	Expires   *time.Time
	UserID    int64
	Name      *string
	ProjectID *int64
}

// tokenCreateInput carries the create arguments as one value. ProjectID is nil
// for an unbound token, which is the default and the current behaviour.
type tokenCreateInput struct {
	OwnerID   int64
	Name      *string
	Expires   *time.Time
	ProjectID *int64
}

type tokenRepository interface {
	List(context.Context, int64) ([]tokenRecord, error)
	GetOwned(context.Context, int64, string) (tokenRecord, error)
	Create(context.Context, tokenCreateInput) (tokenRecord, error)
	DeleteOwned(context.Context, int64, string) error
}

type postgresTokenRepository struct {
	pool    *pgxpool.Pool
	queries *sqlcgen.Queries
}

func newPostgresTokenRepository(pool *pgxpool.Pool) *postgresTokenRepository {
	return &postgresTokenRepository{
		pool:    pool,
		queries: sqlcgen.New(pool),
	}
}

func (r *postgresTokenRepository) List(ctx context.Context, userID int64) ([]tokenRecord, error) {
	databaseUserID, err := patDatabaseID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.queries.ListOwnedPATs(ctx, databaseUserID)
	if err != nil {
		return nil, err
	}
	records := make([]tokenRecord, 0, len(rows))
	for _, row := range rows {
		record, err := patRecord(row.ID, row.Uuid, row.Expires, row.UserID, row.Name, row.ProjectID)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func (r *postgresTokenRepository) GetOwned(ctx context.Context, userID int64, tokenUUID string) (tokenRecord, error) {
	databaseUserID, err := patDatabaseID(userID)
	if err != nil {
		return tokenRecord{}, err
	}
	row, err := r.queries.GetOwnedPAT(ctx, sqlcgen.GetOwnedPATParams{
		Uuid:   tokenUUID,
		UserID: databaseUserID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return tokenRecord{}, errTokenNotFound
	}
	if err != nil {
		return tokenRecord{}, err
	}
	return patRecord(row.ID, row.Uuid, row.Expires, row.UserID, row.Name, row.ProjectID)
}

// Create issues a token and, when the request names a project, binds the token
// to it.
//
// The membership check and both INSERTs run in ONE transaction. That ordering
// is the whole point: a check outside the transaction could pass while the
// owner's assignment is removed before the insert commits, which would leave a
// key bound to a project its owner cannot reach. A refused project therefore
// creates no token row at all.
func (r *postgresTokenRepository) Create(ctx context.Context, input tokenCreateInput) (tokenRecord, error) {
	databaseUserID, err := patDatabaseID(input.OwnerID)
	if err != nil {
		return tokenRecord{}, err
	}
	var boundProjectID *int32
	if input.ProjectID != nil {
		databaseProjectID, err := patDatabaseProjectID(*input.ProjectID)
		if err != nil {
			return tokenRecord{}, err
		}
		boundProjectID = &databaseProjectID
	}
	tokenUUID, err := generateUUID()
	if err != nil {
		return tokenRecord{}, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return tokenRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := r.queries.WithTx(tx)

	if boundProjectID != nil {
		member, err := queries.IsCurrentUserProjectMember(ctx, sqlcgen.IsCurrentUserProjectMemberParams{
			UserID:    databaseUserID,
			ProjectID: *boundProjectID,
		})
		if err != nil {
			return tokenRecord{}, err
		}
		if !member {
			return tokenRecord{}, errTokenProjectForbidden
		}
	}

	row, err := queries.CreatePATForActiveUser(
		ctx,
		sqlcgen.CreatePATForActiveUserParams{
			Uuid:    tokenUUID,
			Expires: patDatabaseTimestamp(input.Expires),
			Name:    input.Name,
			UserID:  databaseUserID,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return tokenRecord{}, errTokenForbidden
	}
	if err != nil {
		return tokenRecord{}, err
	}
	if boundProjectID != nil {
		if err := queries.CreateTokenProjectBinding(ctx, sqlcgen.CreateTokenProjectBindingParams{
			TokenID:   row.ID,
			ProjectID: *boundProjectID,
		}); err != nil {
			return tokenRecord{}, err
		}
	}
	// The issue stamp goes in the SAME transaction, so a token can never exist
	// without one. It is what lets the expiry notice tell a key that has been
	// alive for a month and has a day left from a key whose WHOLE life is
	// twelve hours — the second must not be warned about the moment it is
	// minted (ELITEA-0755). auth_core__token is pylon-owned and carries no
	// creation timestamp, so the stamp lives in this corpus's own side table
	// (shared migration 0125, and 0071's header for why a side table).
	if err := repos.RecordPATIssued(ctx, tx, int64(row.ID), time.Now().UTC()); err != nil {
		return tokenRecord{}, err
	}
	record, err := patRecord(row.ID, row.Uuid, row.Expires, row.UserID, row.Name, boundProjectID)
	if err != nil {
		return tokenRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return tokenRecord{}, err
	}
	return record, nil
}

func (r *postgresTokenRepository) DeleteOwned(ctx context.Context, userID int64, tokenUUID string) error {
	databaseUserID, err := patDatabaseID(userID)
	if err != nil {
		return err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	queries := r.queries.WithTx(tx)
	locked, err := queries.LockPATByUUID(ctx, tokenUUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errTokenNotFound
	}
	if err != nil {
		return err
	}
	if locked.UserID != databaseUserID {
		return errTokenForbidden
	}
	// Delete the binding explicitly rather than leaning on ON DELETE CASCADE.
	// Migration 0071 guards its foreign key, and a guard that skips is never
	// revisited, so the cascade is absent on any database that ran the
	// migration before auth_core existed (spec-llm-project-scope §3.1). This
	// runs inside the same transaction as the token delete, so the two rows go
	// together or neither goes. It is a no-op for an unbound token.
	if err := queries.DeleteTokenProjectBinding(ctx, locked.ID); err != nil {
		return err
	}
	deleted, err := queries.DeletePATByID(ctx, locked.ID)
	if err != nil {
		return err
	}
	if deleted != 1 {
		return fmt.Errorf("delete PAT: affected %d rows, want 1", deleted)
	}
	return tx.Commit(ctx)
}

func patDatabaseID(id int64) (int32, error) {
	if id <= 0 || id > math.MaxInt32 {
		return 0, fmt.Errorf("PAT owner ID is outside the baseline integer range")
	}
	return int32(id), nil
}

// patDatabaseProjectID repeats the range check the handler already applied.
// The handler answers a bad project id with 400 invalid_project_id; this guard
// exists so a future caller of the repository cannot write a binding that names
// project 0 or an identifier the integer column cannot hold.
func patDatabaseProjectID(id int64) (int32, error) {
	if id <= 0 || id > math.MaxInt32 {
		return 0, fmt.Errorf("token project ID is outside the baseline integer range")
	}
	return int32(id), nil
}

func patDatabaseTimestamp(value *time.Time) pgtype.Timestamp {
	if value == nil {
		return pgtype.Timestamp{}
	}
	return pgtype.Timestamp{Time: *value, Valid: true}
}

func patRecord(
	id int32,
	uuid *string,
	expires pgtype.Timestamp,
	userID int32,
	name *string,
	projectID *int32,
) (tokenRecord, error) {
	if id <= 0 || userID <= 0 || (uuid != nil && !validTokenUUID(*uuid)) {
		return tokenRecord{}, errors.New("PAT row contains invalid identity data")
	}
	var expiration *time.Time
	if expires.Valid {
		value := expires.Time
		expiration = &value
	}
	// A stored binding that does not name a project reads as unbound. It never
	// reads as project 0, which is not a project any caller can reach.
	var boundProject *int64
	if projectID != nil && *projectID > 0 {
		value := int64(*projectID)
		boundProject = &value
	}
	return tokenRecord{
		ID:        int64(id),
		UUID:      uuid,
		Expires:   expiration,
		UserID:    int64(userID),
		Name:      name,
		ProjectID: boundProject,
	}, nil
}

func (h *Handler) TokenList(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := tokenOwnerID(r)
	if !ok {
		apierr.Write(w, apierr.Unauthorized("not authenticated"))
		return
	}
	if !h.tokenServiceAvailable(w) {
		return
	}
	records, err := h.tokens.List(r.Context(), ownerID)
	if err != nil {
		apierr.Write(w, apierr.Internal("failed to list tokens"))
		return
	}
	tokens := make([]Token, 0, len(records))
	for _, record := range records {
		token, err := h.presentToken(record, false)
		if err != nil {
			apierr.Write(w, apierr.Internal("failed to encode token metadata"))
			return
		}
		tokens = append(tokens, token)
	}
	writeJSON(w, http.StatusOK, tokens)
}

func (h *Handler) TokenGet(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := tokenOwnerID(r)
	if !ok {
		apierr.Write(w, apierr.Unauthorized("not authenticated"))
		return
	}
	if !h.tokenServiceAvailable(w) {
		return
	}
	tokenUUID := chi.URLParam(r, "tokenUUID")
	if !validTokenUUID(tokenUUID) {
		apierr.Write(w, apierr.BadRequest("invalid token UUID"))
		return
	}
	record, err := h.tokens.GetOwned(r.Context(), ownerID, tokenUUID)
	if errors.Is(err, errTokenNotFound) {
		apierr.Write(w, apierr.BadRequest("token not found"))
		return
	}
	if err != nil {
		apierr.Write(w, apierr.Internal("failed to get token"))
		return
	}
	token, err := h.presentToken(record, false)
	if err != nil {
		apierr.Write(w, apierr.Internal("failed to encode token metadata"))
		return
	}
	writeJSON(w, http.StatusOK, token)
}

type tokenCreateRequest struct {
	Name    tokenNameField   `json:"name"`
	Expires *tokenExpiration `json:"expires"`
	// ProjectID stays raw through decoding so a bad value produces the typed
	// 400 invalid_project_id below, and not the generic "invalid request body"
	// a failing UnmarshalJSON would produce.
	ProjectID json.RawMessage `json:"project_id"`
}

// resolveTokenProjectID reads the optional project binding out of the create
// request.
//
// Absent and explicit null both mean "unbound". Unbound is the current
// behaviour and stays the default (spec-llm-project-scope §4). Every other
// shape is refused: a JSON string, a float, a boolean, zero, a negative number,
// and any value the integer column cannot hold. The refusal is deliberate
// rather than a silent fallback to unbound, because a caller that typed a
// project must not receive a key that bills somewhere else.
func resolveTokenProjectID(raw json.RawMessage) (*int64, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	// A JSON string is refused before json.Number sees it. json.Number accepts
	// a quoted number, so `"42"` would otherwise bind silently while `"abc"`
	// failed — one field with two parsing rules. project_id is a JSON number.
	if trimmed[0] == '"' {
		return nil, errInvalidTokenProjectID
	}
	var number json.Number
	if err := json.Unmarshal(trimmed, &number); err != nil {
		return nil, errInvalidTokenProjectID
	}
	value, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return nil, errInvalidTokenProjectID
	}
	if value <= 0 || value > math.MaxInt32 {
		return nil, errInvalidTokenProjectID
	}
	return &value, nil
}

var errInvalidTokenProjectID = errors.New("project_id must be a positive integer")

// tokenNameField preserves the current distinction between a missing name
// (400) and an explicit JSON null (stored and returned as SQL/JSON null).
type tokenNameField struct {
	present bool
	value   *string
}

func (f *tokenNameField) UnmarshalJSON(data []byte) error {
	var value *string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	f.present = true
	f.value = value
	return nil
}

type tokenExpiration struct {
	Measure string `json:"measure"`
	Value   *int64 `json:"value"`
}

func (h *Handler) TokenCreate(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := tokenOwnerID(r)
	if !ok {
		apierr.Write(w, apierr.Unauthorized("not authenticated"))
		return
	}
	if !h.tokenServiceAvailable(w) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxTokenRequestBodyBytes)
	var request tokenCreateRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&request); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	// Flask's current request.json contract consumes one complete JSON
	// document. Requiring EOF preserves that behavior and prevents ambiguous
	// first-document parsing across clients and language implementations.
	if err := requireJSONDocumentEOF(decoder); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	if !request.Name.present {
		apierr.Write(w, apierr.BadRequest("Name is required"))
		return
	}
	name := request.Name.value
	if name != nil && len(*name) > maxTokenNameBytes {
		apierr.Write(w, apierr.BadRequest("invalid token name"))
		return
	}
	expires, err := request.Expires.resolve(time.Now().UTC())
	if err != nil {
		apierr.Write(w, apierr.BadRequest(err.Error()))
		return
	}
	projectID, err := resolveTokenProjectID(request.ProjectID)
	if err != nil {
		writeTokenJSONError(
			w,
			http.StatusBadRequest,
			"invalid_request_error",
			"invalid_project_id",
			"project_id must be a positive integer",
		)
		return
	}
	record, err := h.tokens.Create(r.Context(), tokenCreateInput{
		OwnerID:   ownerID,
		Name:      name,
		Expires:   expires,
		ProjectID: projectID,
	})
	if errors.Is(err, errTokenForbidden) {
		apierr.Write(w, apierr.Unauthorized("authenticated principal is inactive"))
		return
	}
	if errors.Is(err, errTokenProjectForbidden) {
		// The message names the project the caller asked for. It reports no
		// other fact about that project, and it is the same message whether the
		// project exists, is suspended, or was never created — a probing caller
		// learns nothing it did not already supply.
		writeTokenJSONError(
			w,
			http.StatusForbidden,
			"permission_error",
			"project_forbidden",
			fmt.Sprintf("token owner is not a member of project %d", *projectID),
		)
		return
	}
	if err != nil {
		apierr.Write(w, apierr.Internal("failed to create token"))
		return
	}
	token, err := h.presentToken(record, true)
	if err != nil {
		apierr.Write(w, apierr.Internal("failed to encode token"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, token)
}

func requireJSONDocumentEOF(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body contains more than one JSON value")
		}
		return err
	}
	return nil
}

func (h *Handler) TokenDelete(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := tokenOwnerID(r)
	if !ok {
		apierr.Write(w, apierr.Unauthorized("not authenticated"))
		return
	}
	if !h.tokenServiceAvailable(w) {
		return
	}
	tokenUUID := chi.URLParam(r, "tokenUUID")
	if !validTokenUUID(tokenUUID) {
		apierr.Write(w, apierr.BadRequest("invalid token UUID"))
		return
	}
	err := h.tokens.DeleteOwned(r.Context(), ownerID, tokenUUID)
	switch {
	case errors.Is(err, errTokenNotFound):
		apierr.Write(w, apierr.BadRequest("token not found"))
	case errors.Is(err, errTokenForbidden):
		apierr.Write(w, apierr.Forbidden("token belongs to another user"))
	case err != nil:
		apierr.Write(w, apierr.Internal("failed to delete token"))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func tokenOwnerID(r *http.Request) (int64, bool) {
	user, ok := identity.UserFromContext(r.Context())
	if !ok {
		return 0, false
	}
	return user.OwningUserID()
}

func (h *Handler) tokenServiceAvailable(w http.ResponseWriter) bool {
	if h.tokens != nil && (h.tokenSigner != nil || len(h.tokenSigningKey) != 0) {
		return true
	}
	apierr.WriteStatus(w, http.StatusServiceUnavailable, "token service is not configured")
	return false
}

func (h *Handler) presentToken(record tokenRecord, reveal bool) (Token, error) {
	encoded, err := h.signBaselineToken(record)
	if err != nil {
		return Token{}, err
	}
	if !reveal {
		encoded = "..." + encoded[len(encoded)-7:]
	}
	return Token{
		ID:        record.ID,
		UUID:      record.UUID,
		Expires:   record.Expires,
		UserID:    record.UserID,
		Name:      record.Name,
		ProjectID: record.ProjectID,
		Token:     encoded,
	}, nil
}

// tokenJSONError is the nested error envelope spec-llm-project-scope §8
// prescribes for the binding failures. The other token errors keep the flat
// apierr envelope they answer with today, because changing them would break
// every existing client of this route.
type tokenJSONError struct {
	Error tokenJSONErrorFields `json:"error"`
}

type tokenJSONErrorFields struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

func writeTokenJSONError(w http.ResponseWriter, status int, errType, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(tokenJSONError{
		Error: tokenJSONErrorFields{Message: message, Type: errType, Code: code},
	})
}

type baselineTokenClaims struct {
	UUID    *string `json:"uuid"`
	Expires *string `json:"expires"`
	jwt.RegisteredClaims
}

// signBaselineToken signs the bearer this route returns.
//
// DEFECT this method fixes: the route always signed with the raw key that
// WithTokenSigningKey carried, which the composition root filled from
// APPLICATION_SECRET_KEY. A deployment with an authentication configuration
// file validates a personal access token with a DIFFERENT key: the bytes of
// credentials.pat_signing_key_file. Every token that deployment issued failed
// the signature check on first use, and the plaintext is shown one time only.
//
// The signer therefore wins when the composition root supplies one, because it
// holds the key the same deployment's validator reads back.
func (h *Handler) signBaselineToken(record tokenRecord) (string, error) {
	if h.tokenSigner != nil {
		return h.tokenSigner.SignPAT(record.UUID, record.Expires)
	}
	return authsvc.SignBaselinePAT(h.tokenSigningKey, record.UUID, record.Expires)
}

func (expiration *tokenExpiration) resolve(now time.Time) (*time.Time, error) {
	if expiration == nil {
		return nil, nil
	}
	if expiration.Value == nil {
		return nil, errors.New("expires must have value")
	}
	unit, ok := map[string]time.Duration{
		"seconds": time.Second,
		"minutes": time.Minute,
		"hours":   time.Hour,
		"days":    24 * time.Hour,
		"weeks":   7 * 24 * time.Hour,
	}[expiration.Measure]
	if !ok {
		return nil, errors.New("invalid expires measure")
	}
	unitValue := int64(unit)
	if *expiration.Value > math.MaxInt64/unitValue || *expiration.Value < math.MinInt64/unitValue {
		return nil, errors.New("expires value is out of range")
	}
	result := now.Add(time.Duration(*expiration.Value * unitValue))
	return &result, nil
}

func validTokenUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, char := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}
