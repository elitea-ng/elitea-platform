package adminaccess

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// PostgresRoles reads the roles from auth_core__role and the seed record
// (elitea_auth.admin_role_seeds, shared 0263). A database without the seed
// table records no built-in role, as the Roles matrix reads it.
type PostgresRoles struct {
	pool      registry.Querier
	seedTable atomic.Bool
	// absentUntil caches a "no seed table" answer (unix nanoseconds), so a
	// database without it costs one look per absentFor, not one per request;
	// a migration that adds the table is seen within that time.
	absentUntil atomic.Int64
	now         func() time.Time
}

// absentFor is how long an absent seed table is believed absent.
const absentFor = time.Minute

// NewPostgresRoles returns the reader.
func NewPostgresRoles(pool registry.Querier) *PostgresRoles {
	return &PostgresRoles{pool: pool, now: time.Now}
}

func (p *PostgresRoles) seedsPresent(ctx context.Context) (bool, error) {
	if p.seedTable.Load() {
		return true, nil
	}
	if p.now().UnixNano() < p.absentUntil.Load() {
		return false, nil
	}
	var present bool
	if err := p.pool.QueryRow(ctx,
		`SELECT to_regclass('elitea_auth.admin_role_seeds') IS NOT NULL`).Scan(&present); err != nil {
		return false, fmt.Errorf("look for the built-in role record: %w", err)
	}
	if present {
		p.seedTable.Store(true)
	} else {
		p.absentUntil.Store(p.now().Add(absentFor).UnixNano())
	}
	return present, nil
}

// markerColumn is the marker of the seed row, or NULL without the table.
func (p *PostgresRoles) markerColumn(ctx context.Context) (string, string, error) {
	present, err := p.seedsPresent(ctx)
	if err != nil {
		return "", "", err
	}
	if !present {
		return "NULL::text", "", nil
	}
	return "seed.marker", "LEFT JOIN elitea_auth.admin_role_seeds AS seed ON seed.role_id = role.id", nil
}

// HeldRoles implements RoleReader.
func (p *PostgresRoles) HeldRoles(ctx context.Context, userID int64) ([]Role, error) {
	marker, join, err := p.markerColumn(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, `
SELECT DISTINCT role.name, `+marker+`
FROM public.auth_core__user_role AS assignment
JOIN public.auth_core__role AS role ON role.id = assignment.role_id
`+join+`
WHERE assignment.user_id = $1 AND role.mode = $2
ORDER BY 1`, userID, registry.ModeAdministration)
	if err != nil {
		return nil, fmt.Errorf("read held roles: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Role, error) {
		var name string
		var seed *string
		if err := row.Scan(&name, &seed); err != nil {
			return Role{}, err
		}
		return newRole(name, seed), nil
	})
}

// GrantingRoles implements RoleReader.
func (p *PostgresRoles) GrantingRoles(ctx context.Context, names []string) ([]GrantingRole, error) {
	marker, join, err := p.markerColumn(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, `
SELECT role.name, `+marker+`, array_agg(DISTINCT grant_row.permission ORDER BY grant_row.permission)
FROM public.auth_core__role AS role
JOIN public.auth_core__role_permission AS grant_row ON grant_row.role_id = role.id
`+join+`
WHERE role.mode = $1 AND grant_row.permission = ANY($2::text[])
GROUP BY role.name, 2
ORDER BY 1`, registry.ModeAdministration, names)
	if err != nil {
		return nil, fmt.Errorf("read granting roles: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (GrantingRole, error) {
		var name string
		var seed *string
		var permissions []string
		if err := row.Scan(&name, &seed, &permissions); err != nil {
			return GrantingRole{}, err
		}
		return GrantingRole{Role: newRole(name, seed), Permissions: permissions}, nil
	})
}

func newRole(name string, seed *string) Role {
	if seed == nil {
		return Role{Name: name}
	}
	return Role{Name: name, BuiltIn: true, Marker: *seed}
}

/* ── the admin_console_areas setting ───────────────────────────────────── */

// PlatformFlags reads the console's switches from centry.platform_config.
type PlatformFlags struct {
	pool registry.Querier
}

// NewPlatformFlags returns the reader.
func NewPlatformFlags(pool registry.Querier) *PlatformFlags {
	return &PlatformFlags{pool: pool}
}

// ConsoleAreas answers the stored `admin_console_areas`, or its code default
// (on) when no row was written. A database without the table (a test
// database with part of the corpus) has written none.
func (p *PlatformFlags) ConsoleAreas(ctx context.Context) (bool, error) {
	values, err := platformconfig.LoadVia(ctx, func(ctx context.Context, sql string, args ...any) (platformconfig.Rows, error) {
		return p.pool.Query(ctx, sql, args...)
	}, platformconfig.SectionAdminConsole)
	if platformconfig.UndefinedTable(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the admin_console settings: %w", err)
	}
	return values.Bool(platformconfig.KeyAdminConsoleAreas, true), nil
}
