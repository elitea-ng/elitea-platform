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

// ErrProtected reports that the write targets an account, or names an
// identity, that a SCIM client must never manage. Match it with errors.Is; the
// concrete *ProtectedError carries the reason a client is told.
var ErrProtected = errors.New("scimdirectory: the account is not managed by SCIM")

// ProtectedError is ErrProtected with the reason.
//
// # Why a directory client cannot touch these rows
//
// A SCIM credential can rename an account's address and suspend it. Pointed at
// the WRONG rows, those two verbs are an account takeover and a lock-out:
//
//   - An administrator. Re-address an administrator who has not yet signed in
//     through single sign-on to an address the attacker controls, then sign in
//     through any configured provider asserting it: the first-login email
//     fallback (internal/api/v2/auth/oidc.go joinAccountByEmail) adopts the
//     row, administration role included. Suspending the same rows locks the
//     operators out of the screen that would undo it.
//   - The platform's own principals: `system@centry.user`, every
//     `system_user_<n>@centry.user` and every `:system:project:<n>:` name.
//     The project resolver maps `system_user_<n>@centry.user` to project <n>
//     with no membership check, and the admin Users list hides the
//     `@centry.user` domain, so a row MOVED into that domain both gains a
//     project and disappears from the screen that would show it.
//
// So those rows are refused whatever the verb, and an address or a display
// name in those reserved shapes is refused as a new value. The operator still
// manages them from the admin Users page, where the consequence is on screen.
type ProtectedError struct{ Reason string }

func (e *ProtectedError) Error() string { return "scimdirectory: " + e.Reason }

// Is makes errors.Is(err, ErrProtected) true.
func (e *ProtectedError) Is(target error) bool { return target == ErrProtected }

// reservedAddressDomain is the domain of the platform's own principals
// (`system@centry.user`, `system_user_<n>@centry.user`) and of the fallback
// address internal/application/identity gives a login with no email.
const reservedAddressDomain = "@centry.user"

// reservedNamePrefix starts every system principal's name
// (`:system:project:<n>:`, internal/api/middleware/project.go).
const reservedNamePrefix = ":system:"

// reservedIdentity names why an address or a display name may not be written
// by a SCIM client, or "" when it may.
func reservedIdentity(address, displayName string) string {
	if strings.HasSuffix(NormalizeUserName(address), reservedAddressDomain) {
		return "addresses in the " + reservedAddressDomain + " domain are reserved for platform principals"
	}
	if strings.HasPrefix(strings.TrimSpace(displayName), reservedNamePrefix) {
		return "names starting with " + reservedNamePrefix + " are reserved for platform principals"
	}
	return ""
}

// guardTarget locks the account row and refuses one a SCIM client must not
// manage. It returns ErrNotFound for a missing row.
func guardTarget(ctx context.Context, tx pgx.Tx, id int) error {
	var (
		email, name   string
		administrator bool
	)
	err := tx.QueryRow(ctx,
		`SELECT COALESCE(account.email, ''), COALESCE(account.name, ''),
		        EXISTS (
		            SELECT 1
		              FROM auth_core__user_role AS assignment
		              JOIN auth_core__role AS role ON role.id = assignment.role_id
		             WHERE assignment.user_id = account.id
		               AND role.mode = 'administration'
		        )
		   FROM auth_core__user AS account
		  WHERE account.id = $1
		    FOR UPDATE OF account`, id,
	).Scan(&email, &name, &administrator)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if administrator {
		return &ProtectedError{Reason: "the account holds an administration role; " +
			"manage it from the admin Users page, not through SCIM"}
	}
	if reason := reservedIdentity(email, name); reason != "" {
		return &ProtectedError{Reason: "the account is a platform principal (" + reason + ")"}
	}
	return nil
}

// refuseReservedValue refuses a NEW address or display name in a reserved shape.
func refuseReservedValue(address, displayName string) error {
	if reason := reservedIdentity(address, displayName); reason != "" {
		return &ProtectedError{Reason: reason}
	}
	return nil
}

// addressTaken reports whether ANOTHER account holds the address, compared
// case-insensitively. The unique index on `email` is case-sensitive, so the
// database alone would let `Bob@corp.com` and `bob@corp.com` coexist.
func addressTaken(ctx context.Context, tx pgx.Tx, userName string, id int) (bool, error) {
	var taken bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM auth_core__user WHERE lower(email) = $1 AND id <> $2)`,
		userName, id).Scan(&taken)
	return taken, err
}

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
	// DisplayNameDerived reports that DisplayName was COMPOSED from the `name`
	// parts because the client sent no `displayName`.
	//
	// The difference decides whether it may OVERWRITE a stored display name.
	// Entra ID manages `displayName` directly ("Smith, John (Contractor)") and
	// maps given and family name separately, so a name composed from the parts
	// ("John Smith") replacing the stored one would rewrite the operator's
	// choice on every sync. A composed name only fills an EMPTY display name.
	DisplayNameDerived bool
	// Active is the inverse of `suspended`.
	Active bool
	// GivenName, FamilyName and FormattedName are the SCIM `name`
	// sub-attributes, stored on the SCIM side table (shared migration 0134) and
	// returned as sent. They never stand in for a stored DisplayName; see
	// internal/api/scim/patch_user.go for why.
	GivenName     string
	FamilyName    string
	FormattedName string
	// NameStated reports whether the client sent a `name` object at all. A
	// create that omits it leaves stored parts alone (a re-sync); a replace
	// clears them, because a PUT is the whole resource.
	NameStated bool
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
	COALESCE(scim.external_id, ''), COALESCE(scim.created_at, now()), COALESCE(scim.updated_at, now()),
	COALESCE(scim.given_name, ''), COALESCE(scim.family_name, ''), COALESCE(scim.formatted_name, '')`

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
	if err := refuseReservedValue(userName, user.DisplayName); err != nil {
		return User{}, err
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
		// An adoption is a write to an EXISTING row, and it is held to the
		// same guard as every other write: an administrator's row or a
		// platform principal's is never taken over by a directory push.
		if err := guardTarget(ctx, tx, id); err != nil {
			return User{}, err
		}
		// A name DERIVED from `name` parts fills an empty display name only;
		// an explicit displayName replaces it. See User.DisplayNameDerived.
		_, err = tx.Exec(ctx,
			`UPDATE auth_core__user
			    SET name = CASE WHEN NOT $5 OR COALESCE(name, '') = ''
			                    THEN COALESCE(NULLIF($2, ''), name) ELSE name END,
			        suspended = CASE WHEN $3 THEN $4 ELSE suspended END
			  WHERE id = $1`,
			id, user.DisplayName, user.ActiveStated, !user.Active, user.DisplayNameDerived)
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
			     SET name = CASE WHEN NOT $5 OR COALESCE(auth_core__user.name, '') = ''
			                     THEN COALESCE(NULLIF(EXCLUDED.name, ''), auth_core__user.name)
			                     ELSE auth_core__user.name END,
			         suspended = CASE WHEN $4 THEN EXCLUDED.suspended
			                          ELSE auth_core__user.suspended END
			 RETURNING id`,
			userName, user.DisplayName, !user.Active, user.ActiveStated, user.DisplayNameDerived).Scan(&id)
		if err != nil {
			return User{}, err
		}
	default:
		return User{}, err
	}

	if err := upsertSCIMFacts(ctx, tx, id, user.ExternalID); err != nil {
		return User{}, err
	}
	if user.NameStated {
		if err := writeNameParts(ctx, tx, id, user.GivenName, user.FamilyName, user.FormattedName); err != nil {
			return User{}, err
		}
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

	if err := guardTarget(ctx, tx, id); err != nil {
		return User{}, err
	}
	if err := refuseReservedValue(userName, user.DisplayName); err != nil {
		return User{}, err
	}
	// The same CASE-INSENSITIVE check PATCH makes. The unique index alone
	// would let a PUT store `bob@corp.com` beside an existing `Bob@corp.com`.
	taken, err := addressTaken(ctx, tx, userName, id)
	if err != nil {
		return User{}, err
	}
	if taken {
		return User{}, ErrConflict
	}

	// The display name: an explicit displayName replaces it; one derived from
	// `name` parts fills an EMPTY one and never overwrites a stored one.
	_, err = tx.Exec(ctx,
		`UPDATE auth_core__user
		    SET email = $2,
		        name = CASE WHEN NOT $5 OR COALESCE(name, '') = '' THEN $3 ELSE name END,
		        suspended = $4
		  WHERE id = $1`,
		id, userName, user.DisplayName, !user.Active, user.DisplayNameDerived)
	if isUniqueViolation(err) {
		// Another account already holds the address. Only an operator can
		// decide which of the two survives, so the write stops rather than
		// picking one.
		return User{}, ErrConflict
	}
	if err != nil {
		return User{}, err
	}

	if err := upsertSCIMFacts(ctx, tx, id, user.ExternalID); err != nil {
		return User{}, err
	}
	// A PUT is the whole resource: a replace without `name` clears the parts.
	if err := writeNameParts(ctx, tx, id, user.GivenName, user.FamilyName, user.FormattedName); err != nil {
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Suspension is held to the guard too: a directory client that could
	// suspend the administrators could lock the operators out of the screen
	// that undoes it.
	if err := guardTarget(ctx, tx, id); err != nil {
		return User{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE auth_core__user SET suspended = $2 WHERE id = $1`, id, !active); err != nil {
		return User{}, err
	}
	// Moves `meta.lastModified`, creating the SCIM row when the account was
	// made by a first login rather than by a directory push.
	if _, err := tx.Exec(ctx,
		`INSERT INTO elitea_auth.scim_users (user_id) VALUES ($1)
		 ON CONFLICT (user_id) DO UPDATE SET updated_at = now()`, id); err != nil {
		return User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
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
	// The stored `name` sub-attributes. Each is written on its own; a nil one
	// keeps its stored value.
	GivenName     *string
	FamilyName    *string
	FormattedName *string
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

	if err := guardTarget(ctx, tx, id); err != nil {
		return User{}, err
	}

	newAddress, newDisplayName := "", ""
	if changes.UserName != nil {
		newAddress = *changes.UserName
	}
	if changes.DisplayName != nil {
		newDisplayName = *changes.DisplayName
	}
	if err := refuseReservedValue(newAddress, newDisplayName); err != nil {
		return User{}, err
	}

	if changes.UserName != nil {
		userName := NormalizeUserName(*changes.UserName)
		if userName == "" {
			return User{}, errors.New("scimdirectory: userName is required")
		}
		taken, err := addressTaken(ctx, tx, userName, id)
		if err != nil {
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
	if changes.GivenName != nil || changes.FamilyName != nil || changes.FormattedName != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE elitea_auth.scim_users
			    SET given_name     = COALESCE($2, given_name),
			        family_name    = COALESCE($3, family_name),
			        formatted_name = COALESCE($4, formatted_name),
			        updated_at     = now()
			  WHERE user_id = $1`,
			id, changes.GivenName, changes.FamilyName, changes.FormattedName); err != nil {
			return User{}, err
		}
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

// writeNameParts stores the three `name` sub-attributes exactly. The SCIM row
// exists by now: every caller has upserted it in the same transaction.
func writeNameParts(ctx context.Context, tx pgx.Tx, id int, given, family, formatted string) error {
	_, err := tx.Exec(ctx,
		`UPDATE elitea_auth.scim_users
		    SET given_name = $2, family_name = $3, formatted_name = $4, updated_at = now()
		  WHERE user_id = $1`,
		id, strings.TrimSpace(given), strings.TrimSpace(family), strings.TrimSpace(formatted))
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
		&user.ExternalID, &user.CreatedAt, &user.UpdatedAt,
		&user.GivenName, &user.FamilyName, &user.FormattedName); err != nil {
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
