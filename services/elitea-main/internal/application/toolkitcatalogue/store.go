package toolkitcatalogue

// The PostgreSQL store behind centry.toolkit_type_policy and
// centry.toolkit_type_project_grant (shared migration 0114).

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoPool reports a store built with no database.
//
// It is returned rather than papered over. The catalogue path degrades to
// "allow everything" on a read failure, and it does so at ITS call site with a
// log line; a store that silently answered "no rows" would take that decision
// away from the caller and hide the fault from the admin page too, where a
// missing store must render as an explanation and not as an empty catalogue.
var ErrNoPool = errors.New("toolkitcatalogue: no database pool")

// ErrPolicyNotFound reports that no deployment decision exists for a type.
var ErrPolicyNotFound = errors.New("toolkitcatalogue: no policy for that toolkit type")

// Store reads and writes the two tables.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore builds the store. A nil pool is accepted and every method then
// answers ErrNoPool, which is what a deployment without the shared migrations
// gets — see the composition adapter.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const policyColumns = `toolkit_type, availability, reason, decided_by, decided_at`

const grantColumns = `toolkit_type, project_id, availability, reason, granted_by, granted_at`

// ListPolicies returns every deployment decision, ordered by type.
func (s *Store) ListPolicies(ctx context.Context) ([]Policy, error) {
	if s == nil || s.pool == nil {
		return nil, ErrNoPool
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+policyColumns+`
		   FROM centry.toolkit_type_policy
		  ORDER BY toolkit_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	policies := make([]Policy, 0)
	for rows.Next() {
		var policy Policy
		if err := rows.Scan(&policy.ToolkitType, &policy.Availability, &policy.Reason,
			&policy.DecidedBy, &policy.DecidedAt); err != nil {
			return nil, err
		}
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}

// ListGrants returns every per-project exception, ordered by type then project.
//
// EVERY project's, not one project's. The admin listing renders the exceptions
// per type, and the catalogue filter is built for one project from the same
// rows — one read serving both is what keeps the page and the served catalogue
// from disagreeing about what is granted.
func (s *Store) ListGrants(ctx context.Context) ([]Grant, error) {
	if s == nil || s.pool == nil {
		return nil, ErrNoPool
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+grantColumns+`
		   FROM centry.toolkit_type_project_grant
		  ORDER BY toolkit_type, project_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGrants(rows)
}

// ListProjectGrants returns one project's exceptions.
//
// The catalogue path uses this rather than ListGrants: a deployment with many
// per-project exceptions would otherwise carry every project's rows through
// every project's catalogue request.
func (s *Store) ListProjectGrants(ctx context.Context, projectID int64) ([]Grant, error) {
	if s == nil || s.pool == nil {
		return nil, ErrNoPool
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+grantColumns+`
		   FROM centry.toolkit_type_project_grant
		  WHERE project_id = $1
		  ORDER BY toolkit_type`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGrants(rows)
}

func scanGrants(rows pgx.Rows) ([]Grant, error) {
	grants := make([]Grant, 0)
	for rows.Next() {
		var grant Grant
		if err := rows.Scan(&grant.ToolkitType, &grant.ProjectID, &grant.Availability,
			&grant.Reason, &grant.GrantedBy, &grant.GrantedAt); err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// SavePolicy records or replaces one deployment decision.
//
// A write REPLACES the row, including its reason and decider. The page's
// control is "this is the decision now", not "merge this into the last one", and
// a decision that kept a previous operator's reason would attribute a sentence
// to someone who did not write it.
func (s *Store) SavePolicy(ctx context.Context, policy Policy) (Policy, error) {
	if s == nil || s.pool == nil {
		return Policy{}, ErrNoPool
	}
	toolkitType, err := ValidateToolkitType(policy.ToolkitType)
	if err != nil {
		return Policy{}, err
	}
	availability, err := ParseAvailability(string(policy.Availability))
	if err != nil {
		return Policy{}, PolicyValidationError{Message: err.Error()}
	}
	reason, err := ValidateReason(policy.Reason)
	if err != nil {
		return Policy{}, err
	}
	if policy.DecidedBy == "" {
		return Policy{}, PolicyValidationError{Message: "the decider is required"}
	}

	row := s.pool.QueryRow(ctx,
		`INSERT INTO centry.toolkit_type_policy (toolkit_type, availability, reason, decided_by, decided_at)
		 VALUES ($1, $2, $3, $4, clock_timestamp())
		 ON CONFLICT (toolkit_type) DO UPDATE
		    SET availability = EXCLUDED.availability,
		        reason       = EXCLUDED.reason,
		        decided_by   = EXCLUDED.decided_by,
		        decided_at   = EXCLUDED.decided_at
		 RETURNING `+policyColumns,
		toolkitType, string(availability), reason, policy.DecidedBy)

	var saved Policy
	if err := row.Scan(&saved.ToolkitType, &saved.Availability, &saved.Reason,
		&saved.DecidedBy, &saved.DecidedAt); err != nil {
		return Policy{}, err
	}
	return saved, nil
}

// DeletePolicy reverts one type to the shipping default.
//
// The grants cascade with it, and that is the only correct reading of "revert to
// default": leaving per-project exceptions behind would leave a type whose
// deployment decision is "none" carrying refusals nobody can see on the page,
// because the page renders exceptions under their policy row.
//
// A type with no row is NOT an error. Reverting something already at its default
// is the operator asking for a state they are already in, and a 404 there would
// send them looking for a fault that does not exist.
func (s *Store) DeletePolicy(ctx context.Context, toolkitType string) error {
	if s == nil || s.pool == nil {
		return ErrNoPool
	}
	key, err := ValidateToolkitType(toolkitType)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM centry.toolkit_type_policy WHERE toolkit_type = $1`, key)
	return err
}

// SaveGrant records or replaces one project's exception.
//
// It REQUIRES a policy row, and reports ErrPolicyNotFound when there is none.
// The foreign key would refuse the write anyway; catching it here is what turns
// a constraint name into a sentence the operator can act on ("decide about this
// type first"). The check and the constraint are both kept: the constraint is
// what makes the rule true for every writer.
func (s *Store) SaveGrant(ctx context.Context, grant Grant) (Grant, error) {
	if s == nil || s.pool == nil {
		return Grant{}, ErrNoPool
	}
	toolkitType, err := ValidateToolkitType(grant.ToolkitType)
	if err != nil {
		return Grant{}, err
	}
	availability, err := ParseGrantAvailability(string(grant.Availability))
	if err != nil {
		return Grant{}, PolicyValidationError{Message: err.Error()}
	}
	reason, err := ValidateReason(grant.Reason)
	if err != nil {
		return Grant{}, err
	}
	if grant.ProjectID <= 0 {
		return Grant{}, PolicyValidationError{Message: "a project id is required"}
	}
	if grant.GrantedBy == "" {
		return Grant{}, PolicyValidationError{Message: "the granter is required"}
	}

	row := s.pool.QueryRow(ctx,
		`INSERT INTO centry.toolkit_type_project_grant
		     (toolkit_type, project_id, availability, reason, granted_by, granted_at)
		 VALUES ($1, $2, $3, $4, $5, clock_timestamp())
		 ON CONFLICT (toolkit_type, project_id) DO UPDATE
		    SET availability = EXCLUDED.availability,
		        reason       = EXCLUDED.reason,
		        granted_by   = EXCLUDED.granted_by,
		        granted_at   = EXCLUDED.granted_at
		 RETURNING `+grantColumns,
		toolkitType, grant.ProjectID, string(availability), reason, grant.GrantedBy)

	var saved Grant
	if err := row.Scan(&saved.ToolkitType, &saved.ProjectID, &saved.Availability,
		&saved.Reason, &saved.GrantedBy, &saved.GrantedAt); err != nil {
		if isForeignKeyViolation(err) {
			return Grant{}, ErrPolicyNotFound
		}
		return Grant{}, err
	}
	return saved, nil
}

// DeleteGrant removes one project's exception. Reports ErrPolicyNotFound when
// there was none, so a revoke of something already absent is visible to the
// caller rather than reported as a change that did not happen.
func (s *Store) DeleteGrant(ctx context.Context, toolkitType string, projectID int64) error {
	if s == nil || s.pool == nil {
		return ErrNoPool
	}
	key, err := ValidateToolkitType(toolkitType)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM centry.toolkit_type_project_grant
		  WHERE toolkit_type = $1 AND project_id = $2`, key, projectID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrPolicyNotFound
	}
	return nil
}

// ProjectFilter builds one project's view in two reads.
//
// It returns an error rather than an empty filter when it cannot read. The
// caller decides what an unreadable policy table means for the surface it
// serves, and for the catalogue that decision is "serve everything and log" —
// which must be made where the log line names the surface, not here.
func (s *Store) ProjectFilter(ctx context.Context, projectID int64) (Filter, error) {
	if s == nil || s.pool == nil {
		return EmptyFilter(), ErrNoPool
	}
	policies, err := s.ListPolicies(ctx)
	if err != nil {
		return EmptyFilter(), fmt.Errorf("toolkitcatalogue: read policies: %w", err)
	}
	if len(policies) == 0 {
		// The common path on most deployments, and it skips the second read.
		return EmptyFilter(), nil
	}
	grants, err := s.ListProjectGrants(ctx, projectID)
	if err != nil {
		return EmptyFilter(), fmt.Errorf("toolkitcatalogue: read project grants: %w", err)
	}
	return BuildFilter(projectID, policies, grants), nil
}

// isForeignKeyViolation recognises PostgreSQL's 23503 without importing the
// driver's error struct into every caller.
func isForeignKeyViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23503"
	}
	return false
}
