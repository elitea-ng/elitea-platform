// Package scimdirectory is the account directory a SCIM 2.0 client provisions.
//
// # What it owns, and what it does not
//
// It owns the JOIN between the platform's account row (`auth_core__user`) and
// the SCIM-specific facts that have nowhere to live on it: the identity
// provider's `externalId`, and the resource timestamps a SCIM client reads
// (shared migration 0096). It does NOT own the account: the same row is created
// by a first federated login, listed by the admin Users page and suspended by
// the admin suspend route, and this package is one more writer of it rather than
// its keeper.
//
// That is why an account provisioned here and an account created by a first
// login are the same account. A directory push for someone who has already
// signed in updates their row; it does not create a second one.
//
// # Deactivation, not deletion
//
// `Deactivate` suspends. A SCIM DELETE is documented as removing the resource,
// and this package does not remove it, for a reason the HTTP layer states in
// full: the account id is the author of every agent, prompt and conversation
// that person made, and deleting the row would either cascade that work away or
// orphan it. Suspension is what the platform's own admin surface does, and it is
// reversible — a re-hired person's account comes back with their work attached.
package scimdirectory

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound reports that no account carries the identifier.
var ErrNotFound = errors.New("scimdirectory: no such user")

// ErrConflict reports that the write would collide with another account: two
// accounts cannot share an address, and two cannot share an external id.
var ErrConflict = errors.New("scimdirectory: the identifier is already in use")

// ErrNoPool reports that the store was built without a database pool. It is a
// composition failure, not a request failure.
var ErrNoPool = errors.New("scimdirectory: no database pool")

// User is one account as SCIM sees it.
type User struct {
	ID         int
	ExternalID string
	// UserName is the SCIM `userName`, and it is the address. The platform has
	// no separate login name: `auth_core__user` carries an email and nothing
	// else that identifies a person, and inventing a second identifier would
	// create an account attribute nothing else in this service reads.
	UserName string
	// DisplayName is the SCIM `displayName`, stored as `auth_core__user.name`.
	DisplayName string
	// Active is the inverse of `suspended`.
	Active bool
	// ActiveStated reports whether the CLIENT said anything about `active`.
	//
	// It exists because an omitted flag and an explicit `true` mean different
	// things on the adoption branch of Create. A full re-sync from an identity
	// provider re-sends a profile as a create with no `active` attribute, and
	// treating that as "make this person active" silently undid a suspension an
	// operator had applied by hand.
	ActiveStated bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Store reads and writes the directory.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// NormalizeUserName reduces an address to the form it is stored and matched in.
//
// Case folding is applied because SCIM `userName` is case-insensitive by
// RFC 7643's own definition, and because an identity provider that pushes
// `Alice@Corp.com` and later filters on `alice@corp.com` must find the same
// account. Storing both spellings would be two accounts for one person.
func NormalizeUserName(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

const userColumns = `account.id, account.email, account.name, account.suspended,
	COALESCE(scim.external_id, ''), COALESCE(scim.created_at, now()), COALESCE(scim.updated_at, now())`

const userSource = `auth_core__user AS account
	LEFT JOIN elitea_auth.scim_users AS scim ON scim.user_id = account.id`

// List returns one page of the directory, with the total count.
//
// The total is the count of the WHOLE result, not of the page: a SCIM client
// pages by `startIndex` until it has seen `totalResults`, and a total that
// counted only the page would stop it after the first request.
func (s *Store) List(ctx context.Context, filter Filter, startIndex, count int) ([]User, int, error) {
	if s == nil || s.pool == nil {
		return nil, 0, ErrNoPool
	}
	where, arguments := filter.clause()

	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM `+userSource+where, arguments...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Ordered by id, which is stable and monotonic. Ordering by address would
	// move a resource between pages when somebody's address changed mid-scan,
	// and the client would either see them twice or not at all.
	rows, err := s.pool.Query(ctx,
		`SELECT `+userColumns+` FROM `+userSource+where+
			` ORDER BY account.id OFFSET $`+argumentIndex(len(arguments)+1)+
			` LIMIT $`+argumentIndex(len(arguments)+2),
		append(arguments, startIndex-1, count)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	users := make([]User, 0, count)
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return users, total, nil
}

// Get resolves one account by its platform id.
func (s *Store) Get(ctx context.Context, id int) (User, error) {
	if s == nil || s.pool == nil {
		return User{}, ErrNoPool
	}
	row := s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM `+userSource+` WHERE account.id = $1`, id)
	user, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return user, err
}

// Create provisions an account, or ADOPTS the one that already carries the
// address.
//
// Adoption is the important half. A person who signed in through single sign-on
// before the directory push reaches them already has an account, and creating a
// second one would split their work across two identities that can never be
// merged. The client is told 201 either way, because from its side the resource
// now exists and it has the id.
func (s *Store) Create(ctx context.Context, user User) (User, error) {
	if s == nil || s.pool == nil {
		return User{}, ErrNoPool
	}
	userName := NormalizeUserName(user.UserName)
	if userName == "" {
		return User{}, errors.New("scimdirectory: userName is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// ON THE ADOPTION BRANCH, `suspended` MOVES ONLY IF THE CLIENT SAID SO.
	//
	// A create for an address that already exists is a re-sync, not a new
	// joiner: the identity provider lost its local id mapping, or a connector
	// was re-installed, and it is replaying profiles it has already sent. Those
	// replays routinely omit `active`. Writing the default over the stored value
	// reactivated any account an operator had suspended by hand — silently, and
	// with a 201 that looked like an ordinary success.
	//
	// An EXPLICIT `"active": true` still reactivates. The directory is the
	// authority once it is connected, and a client that states the flag has made
	// a statement about the person.
	//
	// THE LOOKUP IS CASE-INSENSITIVE. The unique index on `email` is not, so an
	// account a first login (or the admin page) stored as `Alice@Corp.com` is
	// invisible to `ON CONFLICT (email)` against the folded `alice@corp.com`,
	// and the insert would create a second account for one person. The existing
	// row is found by lower(email) first, as joinAccountByEmail does for a
	// federated login, and adopted under the spelling it already has.
	var id int
	err = tx.QueryRow(ctx,
		`SELECT id FROM auth_core__user WHERE lower(email) = $1 ORDER BY id LIMIT 1 FOR UPDATE`,
		userName).Scan(&id)
	switch {
	case err == nil:
		_, err = tx.Exec(ctx,
			`UPDATE auth_core__user
			    SET name = COALESCE(NULLIF($2, ''), name),
			        suspended = CASE WHEN $3 THEN $4 ELSE suspended END
			  WHERE id = $1`,
			id, user.DisplayName, user.ActiveStated, !user.Active)
		if err != nil {
			return User{}, err
		}
	case errors.Is(err, pgx.ErrNoRows):
		// Still ON CONFLICT: two concurrent creates both miss the lookup, and
		// the exact-match index settles the race.
		err = tx.QueryRow(ctx,
			`INSERT INTO auth_core__user (email, name, suspended)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (email) DO UPDATE
			     SET name = COALESCE(NULLIF(EXCLUDED.name, ''), auth_core__user.name),
			         suspended = CASE WHEN $4 THEN EXCLUDED.suspended
			                          ELSE auth_core__user.suspended END
			 RETURNING id`,
			userName, user.DisplayName, !user.Active, user.ActiveStated).Scan(&id)
		if err != nil {
			return User{}, err
		}
	default:
		return User{}, err
	}

	if err := upsertSCIMFacts(ctx, tx, id, user.ExternalID); err != nil {
		return User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, err
	}
	return s.Get(ctx, id)
}

// Replace applies a SCIM PUT: the resource becomes exactly what was sent.
func (s *Store) Replace(ctx context.Context, id int, user User) (User, error) {
	if s == nil || s.pool == nil {
		return User{}, ErrNoPool
	}
	userName := NormalizeUserName(user.UserName)
	if userName == "" {
		return User{}, errors.New("scimdirectory: userName is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE auth_core__user SET email = $2, name = $3, suspended = $4 WHERE id = $1`,
		id, userName, user.DisplayName, !user.Active)
	if isUniqueViolation(err) {
		// Another account already holds the address. Only an operator can
		// decide which of the two survives, so the write stops rather than
		// picking one.
		return User{}, ErrConflict
	}
	if err != nil {
		return User{}, err
	}
	if tag.RowsAffected() == 0 {
		return User{}, ErrNotFound
	}

	if err := upsertSCIMFacts(ctx, tx, id, user.ExternalID); err != nil {
		return User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, err
	}
	return s.Get(ctx, id)
}

// SetActive applies the one PATCH operation an identity provider actually
// sends: `active` on or off.
func (s *Store) SetActive(ctx context.Context, id int, active bool) (User, error) {
	if s == nil || s.pool == nil {
		return User{}, ErrNoPool
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE auth_core__user SET suspended = $2 WHERE id = $1`, id, !active)
	if err != nil {
		return User{}, err
	}
	if tag.RowsAffected() == 0 {
		return User{}, ErrNotFound
	}
	if err := s.touch(ctx, id); err != nil {
		return User{}, err
	}
	return s.Get(ctx, id)
}

// UserChanges is the result of a PATCH interpreted against a user. A nil field
// is "leave alone"; a non-nil pointer to "" on DisplayName or ExternalID clears
// it.
type UserChanges struct {
	UserName    *string
	DisplayName *string
	ExternalID  *string
	Active      *bool
}

// ApplyUserChanges persists a whole PATCH in ONE transaction, holding the
// account row. A changed address is re-checked for uniqueness
// CASE-INSENSITIVELY — the unique index is case-sensitive, so the database
// alone would let `Bob@corp.com` and `bob@corp.com` coexist.
func (s *Store) ApplyUserChanges(ctx context.Context, id int, changes UserChanges) (User, error) {
	if s == nil || s.pool == nil {
		return User{}, ErrNoPool
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current string
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(email, '') FROM auth_core__user WHERE id = $1 FOR UPDATE`, id,
	).Scan(&current); errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	} else if err != nil {
		return User{}, err
	}

	if changes.UserName != nil {
		userName := NormalizeUserName(*changes.UserName)
		if userName == "" {
			return User{}, errors.New("scimdirectory: userName is required")
		}
		var taken bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM auth_core__user WHERE lower(email) = $1 AND id <> $2)`,
			userName, id).Scan(&taken); err != nil {
			return User{}, err
		}
		if taken {
			return User{}, ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE auth_core__user SET email = $2 WHERE id = $1`, id, userName); err != nil {
			if isUniqueViolation(err) {
				return User{}, ErrConflict
			}
			return User{}, err
		}
	}
	if changes.DisplayName != nil {
		if _, err := tx.Exec(ctx, `UPDATE auth_core__user SET name = $2 WHERE id = $1`,
			id, *changes.DisplayName); err != nil {
			return User{}, err
		}
	}
	if changes.Active != nil {
		if _, err := tx.Exec(ctx, `UPDATE auth_core__user SET suspended = $2 WHERE id = $1`,
			id, !*changes.Active); err != nil {
			return User{}, err
		}
	}
	if changes.ExternalID != nil {
		if err := upsertSCIMFacts(ctx, tx, id, *changes.ExternalID); err != nil {
			return User{}, err
		}
	} else if _, err := tx.Exec(ctx,
		`INSERT INTO elitea_auth.scim_users (user_id) VALUES ($1)
		 ON CONFLICT (user_id) DO UPDATE SET updated_at = now()`, id); err != nil {
		return User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, err
	}
	return s.Get(ctx, id)
}

// Deactivate is what a SCIM DELETE performs. See the package comment for why it
// is not a deletion.
func (s *Store) Deactivate(ctx context.Context, id int) error {
	_, err := s.SetActive(ctx, id, false)
	return err
}

// touch moves `meta.lastModified`, creating the SCIM row when the account was
// made by a first login rather than by a directory push.
func (s *Store) touch(ctx context.Context, id int) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO elitea_auth.scim_users (user_id) VALUES ($1)
		 ON CONFLICT (user_id) DO UPDATE SET updated_at = now()`, id)
	return err
}

func upsertSCIMFacts(ctx context.Context, tx pgx.Tx, id int, externalID string) error {
	externalID = strings.TrimSpace(externalID)

	// Clear a STALE CLAIM on this external id before inserting.
	//
	// `scim_users.user_id` is not a foreign key — shared migration 0096 explains
	// why it cannot be — so an operator who hard-deletes an account from the
	// admin Users page leaves a row behind. Such a row is invisible to every
	// read, because they all join from `auth_core__user` outwards. The one thing
	// it can still do is hold an external id, and the unique index would then
	// refuse the identity provider's next push of the same person with a
	// conflict nobody can explain or clear from any screen.
	//
	// Only a row whose account is GONE is removed. A live account holding this
	// external id is a real collision and must still be refused.
	if externalID != "" {
		if _, err := tx.Exec(ctx,
			`DELETE FROM elitea_auth.scim_users AS stale
			  WHERE stale.external_id = $1
			    AND stale.user_id <> $2
			    AND NOT EXISTS (
			        SELECT 1 FROM auth_core__user AS account WHERE account.id = stale.user_id
			    )`,
			externalID, id,
		); err != nil {
			return err
		}
	}

	_, err := tx.Exec(ctx,
		`INSERT INTO elitea_auth.scim_users (user_id, external_id)
		 VALUES ($1, $2)
		 ON CONFLICT (user_id) DO UPDATE
		     SET external_id = EXCLUDED.external_id, updated_at = now()`,
		id, externalID)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func isUniqueViolation(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23505"
}

type rowScanner interface {
	Scan(destination ...any) error
}

func scanUser(row rowScanner) (User, error) {
	var (
		user      User
		email     *string
		name      *string
		suspended bool
	)
	if err := row.Scan(&user.ID, &email, &name, &suspended,
		&user.ExternalID, &user.CreatedAt, &user.UpdatedAt); err != nil {
		return User{}, err
	}
	// email and name are NULLABLE on `auth_core__user`, and a row created by
	// something other than this package may carry either as NULL. They are read
	// through pointers so a NULL is an empty string rather than a scan failure
	// that would make the whole listing 500.
	if email != nil {
		user.UserName = *email
	}
	if name != nil {
		user.DisplayName = *name
	}
	user.Active = !suspended
	return user, nil
}

// argumentIndex renders a positional parameter number, so the paging arguments
// can be appended after a variable-length filter.
//
// strconv, not rune arithmetic: `'0' + position` is correct for one digit and
// silently produces a control character for ten or more, which would become a
// query that fails at the database with an error naming none of this.
func argumentIndex(position int) string {
	return strconv.Itoa(position)
}
