package scimdirectory

// The GROUP half of the directory: an identity provider group bound to one
// project role, and the membership a push of that group grants.
//
// # A SCIM group carries half of what a membership needs
//
// Membership on this platform is always (user, PROJECT, ROLE) — there is no
// roleless membership, and `auth_core__project_user_role` has no shape that
// would express one. A SCIM group carries a name and a list of members. It says
// nothing about a project and nothing about a role.
//
// The missing half is AUTHORED, in `elitea_auth.scim_group_bindings` (shared
// migration 0098), by an administrator, before any push arrives. The binding
// names the project and the role; the identity provider supplies the members.
// Neither side can invent the other's half:
//
//   - A push of a group with no binding is REFUSED and named. It does not
//     create a project, and it does not guess a role.
//   - A binding cannot add anybody. Only a push does that.
//
// # The ledger, and why a manually added member survives a sync
//
// `elitea_auth.scim_group_members` records every membership this group applied,
// and whether the push CREATED it. A sync removes only rows this group created.
// A person an administrator added to the project by hand is not in the ledger,
// so no push can take their access away — and a person who was already a member
// when the group first pushed is in the ledger with `granted = false`, so
// removing them from the group withdraws the group's claim and leaves the
// membership they already had.
//
// Two groups bound to the same project role hold their claims independently: a
// membership row is deleted only when no other binding still claims it.
//
// # The project owner keeps their role
//
// `centry.project.owner_id` is the account a project belongs to, and a project
// whose owner holds no role on it is one nobody can administer. A revoke that
// would strip the owner's membership leaves it in place. The ledger row goes,
// so the group stops claiming them; the access does not.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNoBinding reports that no binding names the pushed group. It is what makes
// `POST /Groups` refuse instead of provisioning a project.
var ErrNoBinding = errors.New("scimdirectory: the group is not bound to a project")

// RoleMissingError reports that the binding names a role the project does not
// have. It is a distinct type because the operator needs both halves to fix it:
// a role can be authored on a binding and later removed from the project.
type RoleMissingError struct {
	ProjectID int
	RoleName  string
}

func (e RoleMissingError) Error() string {
	return fmt.Sprintf("scimdirectory: project %d has no role %q", e.ProjectID, e.RoleName)
}

// UnknownMemberError reports a member value that names no account.
//
// It is REFUSED rather than skipped. A push that dropped the members it could
// not resolve and answered 200 would tell the identity provider that everybody
// in the group has access, and the people it silently dropped would have none.
type UnknownMemberError struct {
	Value string
}

func (e UnknownMemberError) Error() string {
	return fmt.Sprintf("scimdirectory: no account carries the member value %q", e.Value)
}

// UnknownProjectError reports a binding written against a project that does not
// exist. Only the admin surface can produce one; a push never chooses a project.
type UnknownProjectError struct {
	ProjectID int
}

func (e UnknownProjectError) Error() string {
	return fmt.Sprintf("scimdirectory: no project %d", e.ProjectID)
}

// Group is one binding as SCIM sees it: the resource, plus the two authored
// values a SCIM group cannot carry.
type Group struct {
	ID          int64
	ExternalID  string
	DisplayName string
	// ProjectID and RoleName are the authored half. They are returned on the
	// SCIM resource too, under this service's own schema extension, so an
	// operator reading their identity provider's log can see which project a
	// push affected without opening the admin screen.
	ProjectID   int
	ProjectName string
	RoleName    string
	Members     []GroupMember
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// GroupMember is one account in a group.
type GroupMember struct {
	UserID      int
	UserName    string
	DisplayName string
	// Granted reports whether the push created the membership. A member with
	// `false` held the role before the group claimed them, and keeps it when the
	// group lets go.
	Granted bool
}

const groupColumns = `binding.id, binding.display_name, binding.external_id,
	binding.project_id, binding.role_name, COALESCE(project.name, ''),
	binding.created_at, binding.updated_at`

const groupSource = `elitea_auth.scim_group_bindings AS binding
	LEFT JOIN centry.project AS project ON project.id = binding.project_id`

/* ── read ──────────────────────────────────────────────────────────────── */

// ListGroups returns one page of the bound groups, with the total count.
func (s *Store) ListGroups(ctx context.Context, filter Filter, startIndex, count int) ([]Group, int, error) {
	if s == nil || s.pool == nil {
		return nil, 0, ErrNoPool
	}
	where, arguments := filter.clause()

	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM `+groupSource+where, arguments...).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.pool.Query(ctx,
		`SELECT `+groupColumns+` FROM `+groupSource+where+
			` ORDER BY binding.id OFFSET $`+argumentIndex(len(arguments)+1)+
			` LIMIT $`+argumentIndex(len(arguments)+2),
		append(arguments, startIndex-1, count)...)
	if err != nil {
		return nil, 0, err
	}
	groups, err := scanGroups(rows)
	if err != nil {
		return nil, 0, err
	}
	if err := s.attachMembers(ctx, groups); err != nil {
		return nil, 0, err
	}
	return groups, total, nil
}

// GetGroup resolves one binding by its resource id.
func (s *Store) GetGroup(ctx context.Context, id int64) (Group, error) {
	if s == nil || s.pool == nil {
		return Group{}, ErrNoPool
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+groupColumns+` FROM `+groupSource+` WHERE binding.id = $1`, id)
	if err != nil {
		return Group{}, err
	}
	groups, err := scanGroups(rows)
	if err != nil {
		return Group{}, err
	}
	if len(groups) == 0 {
		return Group{}, ErrNotFound
	}
	if err := s.attachMembers(ctx, groups); err != nil {
		return Group{}, err
	}
	return groups[0], nil
}

// LookupGroup finds the binding a pushed group belongs to.
//
// The EXTERNAL ID WINS when the client sent one and a binding already carries
// it. A group renamed at the identity provider keeps its external id, so
// matching on it lands the rename on the binding it belongs to; matching on the
// name first would report the renamed group as unbound and an operator would
// author a second binding for a group that already had one.
//
// The display name is what the FIRST push matches on, because that is the only
// thing an administrator can know before a push has ever arrived.
func (s *Store) LookupGroup(ctx context.Context, externalID, displayName string) (Group, error) {
	if s == nil || s.pool == nil {
		return Group{}, ErrNoPool
	}
	externalID = strings.TrimSpace(externalID)
	if externalID != "" {
		var id int64
		err := s.pool.QueryRow(ctx,
			`SELECT id FROM elitea_auth.scim_group_bindings WHERE external_id = $1`,
			externalID).Scan(&id)
		switch {
		case err == nil:
			return s.GetGroup(ctx, id)
		case !errors.Is(err, pgx.ErrNoRows):
			return Group{}, err
		}
	}

	name := strings.TrimSpace(displayName)
	if name == "" {
		return Group{}, ErrNoBinding
	}
	var id int64
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM elitea_auth.scim_group_bindings WHERE lower(display_name) = lower($1)`,
		name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Group{}, ErrNoBinding
	}
	if err != nil {
		return Group{}, err
	}
	return s.GetGroup(ctx, id)
}

/* ── the authored binding ──────────────────────────────────────────────── */

// CreateBinding authors the half a SCIM group cannot carry.
//
// The project and the role are both VERIFIED here, against the tables that own
// them. A binding that named a missing project or a role the project does not
// have would be accepted by the admin screen and refused later, on a push, in a
// log an operator does not read.
func (s *Store) CreateBinding(ctx context.Context, displayName string, projectID int, roleName string) (Group, error) {
	if s == nil || s.pool == nil {
		return Group{}, ErrNoPool
	}
	displayName = strings.TrimSpace(displayName)
	roleName = strings.TrimSpace(roleName)
	if displayName == "" {
		return Group{}, errors.New("scimdirectory: displayName is required")
	}
	if roleName == "" {
		return Group{}, errors.New("scimdirectory: a role name is required")
	}
	if err := s.verifyProjectRole(ctx, s.pool, projectID, roleName); err != nil {
		return Group{}, err
	}

	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO elitea_auth.scim_group_bindings (display_name, project_id, role_name)
		 VALUES ($1, $2, $3) RETURNING id`,
		displayName, projectID, roleName).Scan(&id)
	if isUniqueViolation(err) {
		return Group{}, ErrConflict
	}
	if err != nil {
		return Group{}, err
	}
	return s.GetGroup(ctx, id)
}

// UpdateBinding re-authors a binding, and MOVES the access it granted.
//
// A binding whose project or role changes has already granted memberships under
// the old pair. They are revoked and re-applied under the new one in a single
// transaction, because the alternative is a group whose members hold a role the
// binding no longer names — access an operator has no screen to find and no
// reason to expect.
func (s *Store) UpdateBinding(ctx context.Context, id int64, displayName string, projectID int, roleName string) (Group, error) {
	if s == nil || s.pool == nil {
		return Group{}, ErrNoPool
	}
	displayName = strings.TrimSpace(displayName)
	roleName = strings.TrimSpace(roleName)
	if displayName == "" {
		return Group{}, errors.New("scimdirectory: displayName is required")
	}
	if roleName == "" {
		return Group{}, errors.New("scimdirectory: a role name is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Group{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	binding, err := lockBinding(ctx, tx, id)
	if err != nil {
		return Group{}, err
	}
	if err := s.verifyProjectRole(ctx, tx, projectID, roleName); err != nil {
		return Group{}, err
	}

	moved := binding.ProjectID != projectID || !strings.EqualFold(binding.RoleName, roleName)
	var members []int
	if moved {
		if members, err = ledgerMembers(ctx, tx, id); err != nil {
			return Group{}, err
		}
		if err := revokeMembers(ctx, tx, binding, members); err != nil {
			return Group{}, err
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE elitea_auth.scim_group_bindings
		    SET display_name = $2, project_id = $3, role_name = $4, updated_at = now()
		  WHERE id = $1`,
		id, displayName, projectID, roleName); isUniqueViolation(err) {
		return Group{}, ErrConflict
	} else if err != nil {
		return Group{}, err
	}

	if moved {
		binding.ProjectID, binding.RoleName = projectID, roleName
		if err := grantMembers(ctx, tx, binding, members); err != nil {
			return Group{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Group{}, err
	}
	return s.GetGroup(ctx, id)
}

// AdoptGroup records the identity provider's own identifier on a binding, and
// applies a rename.
//
// It is how a binding authored by name becomes addressable by external id. The
// external id is written only when the binding does not already carry one: an
// identity provider that changed its mind about a group's identifier is a
// collision an operator must resolve, not something to overwrite silently.
func (s *Store) AdoptGroup(ctx context.Context, id int64, externalID, displayName string) error {
	if s == nil || s.pool == nil {
		return ErrNoPool
	}
	externalID = strings.TrimSpace(externalID)
	displayName = strings.TrimSpace(displayName)
	_, err := s.pool.Exec(ctx,
		`UPDATE elitea_auth.scim_group_bindings
		    SET external_id = CASE WHEN external_id = '' THEN $2 ELSE external_id END,
		        display_name = CASE WHEN $3 = '' THEN display_name ELSE $3 END,
		        updated_at = now()
		  WHERE id = $1`,
		id, externalID, displayName)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// DeleteGroup withdraws everything the group granted and removes the binding.
//
// It does NOT delete the project. See the package comment on the users side for
// the same decision about accounts: a SCIM DELETE means "this no longer exists
// at the identity provider", and a project holds the work of everyone in it.
func (s *Store) DeleteGroup(ctx context.Context, id int64) error {
	if s == nil || s.pool == nil {
		return ErrNoPool
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	binding, err := lockBinding(ctx, tx, id)
	if err != nil {
		return err
	}
	members, err := ledgerMembers(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := revokeMembers(ctx, tx, binding, members); err != nil {
		return err
	}
	// The ledger rows go with the binding: the foreign key in 0098 carries ON
	// DELETE CASCADE, and both tables are created by that file, so the cascade
	// is one this corpus is allowed to have.
	if _, err := tx.Exec(ctx,
		`DELETE FROM elitea_auth.scim_group_bindings WHERE id = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ProjectRoleNames lists the roles a project really has, in the order they were
// created.
//
// It exists because the admin screen must offer the roles of the project the
// operator chose, and the general role listing
// (`/admin/roles/{mode}/{projectID}`, internal/api/v2/eliteacore/handler.go)
// answers a HARDCODED admin/editor/viewer when a project carries no role rows.
// That fallback is fine where it came from and wrong here: a picker fed by it
// offers roles the project does not have, and the save is then refused by a
// value the control itself supplied.
//
// An empty result is a true answer — the project has no roles — and the screen
// says so rather than filling the gap.
func (s *Store) ProjectRoleNames(ctx context.Context, projectID int) ([]string, error) {
	if s == nil || s.pool == nil {
		return nil, ErrNoPool
	}
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT true FROM centry.project WHERE id = $1`, projectID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, UnknownProjectError{ProjectID: projectID}
		}
		return nil, err
	}

	rows, err := s.pool.Query(ctx,
		`SELECT name FROM public.auth_core__project_role
		  WHERE project_id = $1 ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	names := make([]string, 0, 4)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

/* ── membership ────────────────────────────────────────────────────────── */

// ReplaceGroupMembers makes the group's membership exactly what was sent.
//
// It is authoritative over ITS OWN grants and nothing else: members in the
// ledger that the request omits are revoked, and members of the project that
// this group never granted are untouched.
func (s *Store) ReplaceGroupMembers(ctx context.Context, id int64, members []int) (Group, error) {
	return s.applyMembers(ctx, id, func(ctx context.Context, tx pgx.Tx, binding Group) error {
		return replaceMembers(ctx, tx, binding, members)
	})
}

// replaceMembers is the body of a replace, so ApplyGroupOperations and
// ReplaceGroupMembers cannot come to mean different things.
func replaceMembers(ctx context.Context, tx pgx.Tx, binding Group, members []int) error {
	existing, err := ledgerMembers(ctx, tx, binding.ID)
	if err != nil {
		return err
	}
	wanted := make(map[int]struct{}, len(members))
	for _, member := range members {
		wanted[member] = struct{}{}
	}
	var removed []int
	for _, member := range existing {
		if _, keep := wanted[member]; !keep {
			removed = append(removed, member)
		}
	}
	if err := revokeMembers(ctx, tx, binding, removed); err != nil {
		return err
	}
	return grantMembers(ctx, tx, binding, members)
}

// AddGroupMembers grants the members named, and leaves the rest of the group
// alone.
func (s *Store) AddGroupMembers(ctx context.Context, id int64, members []int) (Group, error) {
	return s.applyMembers(ctx, id, func(ctx context.Context, tx pgx.Tx, binding Group) error {
		return grantMembers(ctx, tx, binding, members)
	})
}

// RemoveGroupMembers withdraws the members named.
func (s *Store) RemoveGroupMembers(ctx context.Context, id int64, members []int) (Group, error) {
	return s.applyMembers(ctx, id, func(ctx context.Context, tx pgx.Tx, binding Group) error {
		return revokeMembers(ctx, tx, binding, members)
	})
}

// GroupOperationKind names one step of a PATCH.
type GroupOperationKind int

const (
	// GroupAddMembers grants the members named and leaves the rest alone.
	GroupAddMembers GroupOperationKind = iota
	// GroupReplaceMembers makes the membership exactly the members named.
	GroupReplaceMembers
	// GroupRemoveMembers withdraws the members named.
	GroupRemoveMembers
	// GroupRename applies a displayName change.
	GroupRename
	// GroupSetExternalID replaces the identity provider's identifier; an empty
	// ExternalID clears it.
	GroupSetExternalID
)

// GroupOperation is one understood step of a PATCH.
type GroupOperation struct {
	Kind        GroupOperationKind
	Members     []int
	DisplayName string
	ExternalID  string
}

// ApplyGroupOperations applies a whole PATCH in ONE transaction.
//
// # Why the whole request, and not a call per operation
//
// An identity provider sends a membership delta as several operations in one
// request — Entra ID's is `[{add members …}, {remove members …}]` — and it
// sends that request once. Applying the operations through separate calls means
// a failure in the second commits the first and reports the whole PATCH as
// failed, so the group keeps somebody it was told to drop and nothing will say
// so again.
//
// Here the binding is locked once and every operation runs inside that
// transaction, so the request either lands whole or leaves nothing behind.
func (s *Store) ApplyGroupOperations(ctx context.Context, id int64, operations []GroupOperation) (Group, error) {
	return s.applyMembers(ctx, id, func(ctx context.Context, tx pgx.Tx, binding Group) error {
		for _, operation := range operations {
			switch operation.Kind {
			case GroupRename:
				if err := renameBinding(ctx, tx, id, operation.DisplayName); err != nil {
					return err
				}

			case GroupSetExternalID:
				if err := setBindingExternalID(ctx, tx, id, operation.ExternalID); err != nil {
					return err
				}

			case GroupAddMembers:
				if err := grantMembers(ctx, tx, binding, operation.Members); err != nil {
					return err
				}

			case GroupReplaceMembers:
				if err := replaceMembers(ctx, tx, binding, operation.Members); err != nil {
					return err
				}

			case GroupRemoveMembers:
				if err := revokeMembers(ctx, tx, binding, operation.Members); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// RenameGroup applies a `displayName` change from the identity provider.
func (s *Store) RenameGroup(ctx context.Context, id int64, displayName string) (Group, error) {
	if s == nil || s.pool == nil {
		return Group{}, ErrNoPool
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return Group{}, errors.New("scimdirectory: displayName is required")
	}
	if err := renameBinding(ctx, s.pool, id, displayName); err != nil {
		return Group{}, err
	}
	return s.GetGroup(ctx, id)
}

// execer is the write half of a pool and a transaction, so a rename is one
// statement whether it runs alone or inside a PATCH.
type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func renameBinding(ctx context.Context, db execer, id int64, displayName string) error {
	tag, err := db.Exec(ctx,
		`UPDATE elitea_auth.scim_group_bindings
		    SET display_name = $2, updated_at = now() WHERE id = $1`,
		id, displayName)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func setBindingExternalID(ctx context.Context, db execer, id int64, externalID string) error {
	tag, err := db.Exec(ctx,
		`UPDATE elitea_auth.scim_group_bindings
		    SET external_id = $2, updated_at = now() WHERE id = $1`,
		id, strings.TrimSpace(externalID))
	if isUniqueViolation(err) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// applyMembers runs one membership change inside a transaction that holds the
// binding row.
//
// The lock is what makes two pushes of the same group safe. Both would
// otherwise read the ledger, decide independently what to revoke, and apply the
// difference between a state neither of them still sees.
func (s *Store) applyMembers(
	ctx context.Context, id int64,
	apply func(context.Context, pgx.Tx, Group) error,
) (Group, error) {
	if s == nil || s.pool == nil {
		return Group{}, ErrNoPool
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Group{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	binding, err := lockBinding(ctx, tx, id)
	if err != nil {
		return Group{}, err
	}
	if err := apply(ctx, tx, binding); err != nil {
		return Group{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE elitea_auth.scim_group_bindings SET updated_at = now() WHERE id = $1`,
		id); err != nil {
		return Group{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Group{}, err
	}
	return s.GetGroup(ctx, id)
}

// grantMembers gives each member the bound role, and records what it created.
func grantMembers(ctx context.Context, tx pgx.Tx, binding Group, members []int) error {
	if len(members) == 0 {
		return nil
	}
	roleID, err := resolveRoleID(ctx, tx, binding.ProjectID, binding.RoleName)
	if err != nil {
		return err
	}
	for _, member := range members {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT true FROM auth_core__user WHERE id = $1`, member).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return UnknownMemberError{Value: fmt.Sprint(member)}
			}
			return err
		}

		// The row count is the ONLY honest source of "did this push create the
		// membership". Reading first and inserting after would race another
		// writer between the two statements, and the ledger would claim a
		// membership somebody else made.
		tag, err := tx.Exec(ctx,
			`INSERT INTO auth_core__project_user_role (project_id, user_id, role_id)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (project_id, user_id, role_id) DO NOTHING`,
			binding.ProjectID, member, roleID)
		if err != nil {
			return err
		}
		created := tag.RowsAffected() == 1

		// A membership ANOTHER binding granted is still a membership SCIM
		// created, so this binding records it as granted too. Without this, the
		// second group to push a shared member would record `granted = false` —
		// the same value a person added by hand carries — and the membership
		// would outlive both groups letting go of it.
		if !created {
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (
				     SELECT 1 FROM elitea_auth.scim_group_members
				      WHERE user_id = $1 AND role_id = $2 AND granted
				 )`, member, roleID).Scan(&created); err != nil {
				return err
			}
		}

		// `granted` only ever goes false → true. A re-push of a member this
		// group already granted inserts nothing, and must not downgrade the
		// ledger to "was already a member" — that would make the membership
		// permanent, because the revoke path leaves such rows alone.
		if _, err := tx.Exec(ctx,
			`INSERT INTO elitea_auth.scim_group_members (binding_id, user_id, role_id, granted)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (binding_id, user_id) DO UPDATE
			     SET role_id = EXCLUDED.role_id,
			         granted = elitea_auth.scim_group_members.granted OR EXCLUDED.granted`,
			binding.ID, member, roleID, created); err != nil {
			return err
		}
	}
	return nil
}

// revokeMembers withdraws what this group gave, and only that.
func revokeMembers(ctx context.Context, tx pgx.Tx, binding Group, members []int) error {
	if len(members) == 0 {
		return nil
	}
	var ownerID int
	// A project row is expected, but a binding can outlive the project it names:
	// nothing in this corpus may hold a foreign key to `centry.project`. A
	// missing row leaves ownerID zero, which matches no account, so the revoke
	// proceeds rather than failing on a project that is already gone.
	if err := tx.QueryRow(ctx,
		`SELECT owner_id FROM centry.project WHERE id = $1`, binding.ProjectID,
	).Scan(&ownerID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	for _, member := range members {
		var (
			roleID  int
			granted bool
		)
		err := tx.QueryRow(ctx,
			`DELETE FROM elitea_auth.scim_group_members
			  WHERE binding_id = $1 AND user_id = $2
			  RETURNING role_id, granted`,
			binding.ID, member).Scan(&roleID, &granted)
		if errors.Is(err, pgx.ErrNoRows) {
			// The group never granted this person. A SCIM remove of somebody
			// who is not a member is not an error — the requested end state is
			// the state that already holds.
			continue
		}
		if err != nil {
			return err
		}
		if !granted {
			// They held the role before this group claimed them. The claim is
			// gone; the membership is theirs.
			continue
		}
		if member == ownerID {
			slog.Info("SCIM: kept the project owner's role on a group revoke",
				"project_id", binding.ProjectID, "user_id", member, "group_id", binding.ID)
			continue
		}

		// Another binding may still claim the same membership. The ledger row
		// of THIS binding is already deleted, so what remains is every other
		// claim.
		//
		// `AND granted` is load-bearing, and it is the SAME question the grant
		// side asks above. A ledger row with `granted = false` records that its
		// binding FOUND the membership, not that it holds it — such a row must
		// not keep a membership alive.
		//
		// Without the clause: a manual member is claimed by group A
		// (granted = false), an administrator then removes the membership by
		// hand, group B pushes and creates it (granted = true), and B letting go
		// finds A's row and skips the delete. A letting go skips it too. The
		// person keeps a role that no binding grants and no push can withdraw.
		var claimed bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (
			     SELECT 1 FROM elitea_auth.scim_group_members
			      WHERE user_id = $1 AND role_id = $2 AND granted
			 )`, member, roleID).Scan(&claimed); err != nil {
			return err
		}
		if claimed {
			continue
		}

		if _, err := tx.Exec(ctx,
			`DELETE FROM auth_core__project_user_role
			  WHERE project_id = $1 AND user_id = $2 AND role_id = $3`,
			binding.ProjectID, member, roleID); err != nil {
			return err
		}
	}
	return nil
}

/* ── shared ────────────────────────────────────────────────────────────── */

// queryRower is the half of a pool and a transaction this file reads through,
// so the project and role checks run identically inside and outside one.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, arguments ...any) pgx.Row
}

func (s *Store) verifyProjectRole(ctx context.Context, db queryRower, projectID int, roleName string) error {
	var exists bool
	if err := db.QueryRow(ctx,
		`SELECT true FROM centry.project WHERE id = $1`, projectID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return UnknownProjectError{ProjectID: projectID}
		}
		return err
	}
	_, err := resolveRoleID(ctx, db, projectID, roleName)
	return err
}

// resolveRoleID reads the project role by NAME.
//
// The name is what a binding stores and what an operator authors; the id is
// per-project and is re-created whenever a project is re-provisioned. A binding
// that stored the id would silently point at another project's role after such
// a rebuild.
func resolveRoleID(ctx context.Context, db queryRower, projectID int, roleName string) (int, error) {
	var roleID int
	err := db.QueryRow(ctx,
		`SELECT id FROM public.auth_core__project_role
		  WHERE project_id = $1 AND name = $2`, projectID, roleName).Scan(&roleID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, RoleMissingError{ProjectID: projectID, RoleName: roleName}
	}
	return roleID, err
}

// lockBinding reads a binding FOR UPDATE. See applyMembers for why.
func lockBinding(ctx context.Context, tx pgx.Tx, id int64) (Group, error) {
	var binding Group
	err := tx.QueryRow(ctx,
		`SELECT id, display_name, external_id, project_id, role_name, created_at, updated_at
		   FROM elitea_auth.scim_group_bindings WHERE id = $1 FOR UPDATE`, id).
		Scan(&binding.ID, &binding.DisplayName, &binding.ExternalID,
			&binding.ProjectID, &binding.RoleName, &binding.CreatedAt, &binding.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Group{}, ErrNotFound
	}
	return binding, err
}

// ledgerMembers reads the accounts this binding claims.
func ledgerMembers(ctx context.Context, tx pgx.Tx, id int64) ([]int, error) {
	rows, err := tx.Query(ctx,
		`SELECT user_id FROM elitea_auth.scim_group_members
		  WHERE binding_id = $1 ORDER BY user_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []int
	for rows.Next() {
		var member int
		if err := rows.Scan(&member); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

func scanGroups(rows pgx.Rows) ([]Group, error) {
	defer rows.Close()

	groups := make([]Group, 0, 8)
	for rows.Next() {
		var group Group
		if err := rows.Scan(&group.ID, &group.DisplayName, &group.ExternalID,
			&group.ProjectID, &group.RoleName, &group.ProjectName,
			&group.CreatedAt, &group.UpdatedAt); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

// attachMembers fills every group's member list in ONE query.
//
// The join is inner, not outer: a ledger row whose account was hard-deleted from
// the admin Users page names nobody, and listing it would put a member on the
// resource that the client cannot resolve, remove or match.
func (s *Store) attachMembers(ctx context.Context, groups []Group) error {
	if len(groups) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(groups))
	index := make(map[int64]int, len(groups))
	for position, group := range groups {
		ids = append(ids, group.ID)
		index[group.ID] = position
	}

	rows, err := s.pool.Query(ctx,
		`SELECT membership.binding_id, membership.user_id,
		        COALESCE(account.email, ''), COALESCE(account.name, ''), membership.granted
		   FROM elitea_auth.scim_group_members AS membership
		   JOIN auth_core__user AS account ON account.id = membership.user_id
		  WHERE membership.binding_id = ANY($1)
		  ORDER BY membership.user_id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			bindingID int64
			member    GroupMember
		)
		if err := rows.Scan(&bindingID, &member.UserID,
			&member.UserName, &member.DisplayName, &member.Granted); err != nil {
			return err
		}
		if position, ok := index[bindingID]; ok {
			groups[position].Members = append(groups[position].Members, member)
		}
	}
	return rows.Err()
}

// AmbiguousMemberError reports a member value that resolves to more than one
// account.
//
// It is possible because a member value is matched against three identifiers —
// see ResolveMember — and an identity provider whose group member values are
// numeric could send one that is another account's platform id. Picking either
// account would put a person in a project they are not in, so the push is
// refused and the value is named.
type AmbiguousMemberError struct {
	Value string
}

func (e AmbiguousMemberError) Error() string {
	return fmt.Sprintf("scimdirectory: the member value %q names more than one account", e.Value)
}

// ResolveMember maps one SCIM `members[].value` onto an account id.
//
// RFC 7643 defines the value as the id of the User resource, which is what a
// provider that created its users through `/Users` sends back. Two other
// identifiers are accepted because providers do send them: the `externalId`
// this service stored, and the address. All three are EXACT matches — nothing
// here is a prefix, a fold beyond the case-insensitivity SCIM defines for an
// address, or a nearest guess.
//
// A value that matches nothing is refused, and so is a value that matches more
// than one account. Both refusals name the value. A platform principal is
// returned with its id AND a *ProtectedError: a removal may proceed, a grant
// must refuse.
func (s *Store) ResolveMember(ctx context.Context, value string) (int, error) {
	if s == nil || s.pool == nil {
		return 0, ErrNoPool
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, UnknownMemberError{Value: value}
	}

	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT account.id, COALESCE(account.email, '')
		   FROM auth_core__user AS account
		   LEFT JOIN elitea_auth.scim_users AS scim ON scim.user_id = account.id
		  WHERE account.id::text = $1
		     OR (COALESCE(scim.external_id, '') <> '' AND scim.external_id = $1)
		     OR lower(COALESCE(account.email, '')) = lower($1)`, value)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var (
		matches   []int
		addresses []string
	)
	for rows.Next() {
		var (
			id      int
			address string
		)
		if err := rows.Scan(&id, &address); err != nil {
			return 0, err
		}
		matches = append(matches, id)
		addresses = append(addresses, address)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	switch len(matches) {
	case 0:
		return 0, UnknownMemberError{Value: value}
	case 1:
		// A PLATFORM PRINCIPAL is never a member a group may grant to: a
		// bound project role on `system_user_<n>@centry.user` or
		// `system@centry.user` is a grant to the platform's own identity.
		// The id is returned WITH the refusal, so a caller removing members
		// can still remove it; a caller granting must refuse.
		if IsReservedAddress(addresses[0]) {
			return matches[0], &ProtectedError{Reason: "the member " + strconv.Quote(value) +
				" is a platform principal (an address in the " + reservedAddressDomain +
				" domain); a group cannot grant it a project role"}
		}
		return matches[0], nil
	default:
		return 0, AmbiguousMemberError{Value: value}
	}
}
