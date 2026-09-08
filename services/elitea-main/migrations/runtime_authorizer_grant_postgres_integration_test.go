package migrations_test

// The permissions the chat path requires, measured against a CLEAN database
// (#354).
//
// # Why this file exists, and why no other test could catch this
//
// `models.chat.messages.create` gates TWO things, not one:
//
//	internal/api/v2/agentexecution/route.go:32, CurrentApplicationStartPermission
//	    → POST .../messages/prompt_lib/...      (applied at :81)
//	    → POST .../continue_predict/prompt_lib/... (applied at :105)
//
//	internal/runtimecomposition/public_authorizer.go:124, AuthorizeExecutionEvents
//	    → the agent.execute.application.v1 and agent.execute.adhoc.v1 branch
//	      of the execution-event stream
//
// No migration granted it, and 001_initial.sql does not seed it. So on a clean
// database a user can neither START an agent nor READ its stream: both answer
// 403 to every caller, and the chat never produces a reply. Nothing in the logs
// names the missing grant.
//
// The same authorizer branches on two more permissions, which this file
// measures at the same time so that the whole runtime gate set is pinned:
//
//	index.ingest.v1          → models.applications.tool.patch  (0068, admin+editor)
//	configuration.validate.v1 → len(resolution.Permissions) != 0
//
// # Why the end-to-end suite could not see it
//
// apps/elitea-web/scripts/e2e-stack.sh:461 and :674 insert the permission as a
// PER-PROJECT auth_core__project_role_permission row. Those rows suppress
// legacyrbac's central default-mode fallback entirely and supply the permission
// directly. So the whole end-to-end suite passed against a database that
// already granted what a real deployment lacked. The seed hid the defect. A
// pylon-backed database and a legacy dump have that same per-project shape,
// which is why only a clean Go bootstrap breaks.
//
// So this file asserts against the corpus a real deployment applies, and
// against nothing else. newMigratedPool CREATEs an empty database, applies
// 001_initial.sql, then applies every shared and tenant migration through the
// real runner. No seed script runs here. Delete
// shared/0070_chat_messages_create_permission.sql and the first two tests fail;
// no other test does.
//
// Both directions are covered, because only the pair discriminates. A gate that
// refuses EVERY caller also passes a positive-only test suite that never checks
// a caller is refused. So the entitled caller must get through
// (TestAProjectMemberPassesBothChatGatesOnACleanDatabase) AND the caller
// without the grant must be refused (TestACallerWithoutTheGrantIsRefusedAtBothChatGates).
//
// Every assertion reads the grant ROWS or the resolver, never a status code, so
// a change to the HTTP surface cannot mask a lost grant.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

// The default-mode roles 001_initial.sql seeds. The legacy matrix
// (testdata/postgres/legacy-rbac-matrix.json) grants
// `models.chat.messages.create` to all three.
//
// These are the three HUMAN roles. shared/0088 seeds a fourth default-mode
// role, `system`, for the per-project machine identity; see machineModeRole.
var defaultModeRoles = []string{"admin", "editor", "viewer"}

// machineModeRole is the default-mode role the per-project system identity
// holds. shared/0089_central_system_role.sql seeds it. projectprovisioning
// assigns the system user a project role of this name. legacyrbac resolves
// such a role through the CENTRAL default-mode role of the same name. With no central row the scheduled-execution PAT resolved nothing
// and every worker callback answered 403.
//
// It is NOT a human role. It holds only the callback surface 0088 lists, so it
// appears in the expected holder set of those permissions and of no other.
const machineModeRole = "system"

// withMachineRole returns `holders` plus the machine role, sorted the way
// defaultModeGrantHolders sorts what it reads.
//
// Use it only for a permission shared/0088 grants. Naming the role at each such
// call site keeps the assertion discriminating: a grant that reaches `system`
// anywhere else still fails.
func withMachineRole(holders ...string) []string {
	combined := append(append([]string{}, holders...), machineModeRole)
	slices.Sort(combined)
	return combined
}

// The permission both chat gates require. It is read from the production
// constant, not copied, so a rename cannot leave this test measuring a string
// nothing uses. public_authorizer.go:124 spells the same value as a literal.
const chatPathPermission = agentexecution.CurrentApplicationStartPermission

// The permission the index.ingest.v1 branch of AuthorizeExecutionEvents
// requires. 0068 grants it to admin and editor, and withholds it from viewer,
// which is what the legacy matrix records.
const indexIngestPermission = "models.applications.tool.patch"

// A clean database grants the chat-path permission to every default-mode role.
// This is the assertion #354 exists for.
func TestCleanDatabaseGrantsTheChatPathPermission(t *testing.T) {
	pool := newMigratedPool(t)

	holders := defaultModeGrantHolders(t, pool, chatPathPermission)
	if !slices.Equal(holders, defaultModeRoles) {
		t.Fatalf("default-mode holders of %s = %v, want %v: without the grant the agent START "+
			"route and the execution-event stream both answer 403 to every caller, and the chat "+
			"never produces a reply", chatPathPermission, holders, defaultModeRoles)
	}
}

// A project member of a clean database passes BOTH chat gates. This runs the
// same resolver call both gates make, then the same membership test each gate
// applies to the result.
func TestAProjectMemberPassesBothChatGatesOnACleanDatabase(t *testing.T) {
	pool := newMigratedPool(t)
	seedProjectMembership(t, pool)

	resolution := resolveDefaultMode(t, pool, "1")

	// Gate 1 — agentexecution/route.go:81 and :105, the agent START and
	// CONTINUE routes.
	if !slices.Contains(resolution.Permissions, chatPathPermission) {
		t.Fatalf("a project member does not resolve %s on a clean database (resolved %v); "+
			"the agent START route refuses the caller and no execution begins",
			chatPathPermission, resolution.Permissions)
	}

	// Gate 2 — public_authorizer.go:123-126, the agent.execute.application.v1
	// and agent.execute.adhoc.v1 branch of AuthorizeExecutionEvents. Same
	// permission, same resolution, separate refusal.
	if !slices.Contains(resolution.Permissions, chatPathPermission) {
		t.Fatal("the execution-event stream refuses the caller; the chat never streams a reply")
	}

	// public_authorizer.go:114-122, the configuration.validate.v1 branch. It
	// reads the SIZE of the set as a membership stand-in (#276), so it is
	// recorded here as well.
	if len(resolution.Permissions) == 0 {
		t.Fatal("a project member resolves the empty set; the configuration.validate.v1 branch " +
			"refuses every caller")
	}

	// public_authorizer.go:110-113, the index.ingest.v1 branch. The member
	// holds an `editor` project role, and 0068 grants this permission to
	// admin and editor.
	if !slices.Contains(resolution.Permissions, indexIngestPermission) {
		t.Fatalf("an editor does not resolve %s on a clean database (resolved %v); "+
			"the index.ingest.v1 branch refuses the caller", indexIngestPermission, resolution.Permissions)
	}
}

// The negative control, and the reason the positive test above means anything.
//
// A member of a project that carries its OWN per-project grant rows resolves
// only those rows: the central fallback is suppressed for that project, so the
// corpus grant does not reach it. That caller therefore does NOT hold
// `models.chat.messages.create`, and BOTH chat gates must refuse them.
//
// Without this case a gate that admitted every caller would pass the positive
// test unchanged. This case also pins the migration's blast radius: it is the
// shape of every pylon-backed database, every legacy dump and the end-to-end
// stack, so the migration cannot change what their members can do.
func TestACallerWithoutTheGrantIsRefusedAtBothChatGates(t *testing.T) {
	pool := newMigratedPool(t)
	roleID := seedProjectMembership(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
VALUES (1, $1, 'models.something.else')`, roleID); err != nil {
		t.Fatalf("seed the per-project grant: %v", err)
	}

	resolution := resolveDefaultMode(t, pool, "1")
	if !slices.Equal(resolution.Permissions, []string{"models.something.else"}) {
		t.Fatalf("permissions = %v, want only the project's own grant; the central fallback must "+
			"not apply to a project that carries per-project rows", resolution.Permissions)
	}
	// Both gates read the resolution the same way, so one absence refuses both.
	if slices.Contains(resolution.Permissions, chatPathPermission) {
		t.Fatalf("%s resolves for a caller nothing granted it; both chat gates would admit an "+
			"unentitled caller", chatPathPermission)
	}
}

// A user with no role in the project gains nothing. The central fallback is
// joined through the caller's assigned project roles, so a non-member has no
// row to fall back from and the grant cannot reach outside the project.
func TestTheChatPathGrantGivesANonMemberNothing(t *testing.T) {
	pool := newMigratedPool(t)
	seedProjectMembership(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`INSERT INTO public.auth_core__user (id, email, name) VALUES (354, 'out354@example.com', 'Out')`,
	); err != nil {
		t.Fatalf("seed the non-member: %v", err)
	}

	resolution, err := legacyrbac.NewPostgresResolver(pool).ResolvePermissions(
		ctx, auth.User{ID: "354", UserID: "354"}, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatalf("resolve the non-member's permissions: %v", err)
	}
	if len(resolution.Permissions) != 0 {
		t.Fatalf("non-member permissions = %v, want empty", resolution.Permissions)
	}
}

/* ── harness ───────────────────────────────────────────────────────────── */

// resolveDefaultMode makes the exact call both gates make: agentexecution's
// route resolves `CurrentApplicationStartMode` for the path's project, and
// public_authorizer.go:100-108 resolves auth.PermissionModeDefault for the
// execution's project. The two modes are the same value.
func resolveDefaultMode(t *testing.T, pool *pgxpool.Pool, projectID string) auth.PermissionResolution {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	resolution, err := legacyrbac.NewPostgresResolver(pool).ResolvePermissions(
		ctx,
		auth.User{ID: "1", UserID: "1"},
		agentexecution.CurrentApplicationStartMode,
		projectID,
	)
	if err != nil {
		t.Fatalf("resolve the caller's default-mode permissions: %v", err)
	}
	return resolution
}

// defaultModeGrantHolders reads the grant ROWS the corpus leaves behind. It
// asserts the database state, not an HTTP status, so a change to the route
// cannot hide a lost grant.
func defaultModeGrantHolders(t *testing.T, pool *pgxpool.Pool, permission string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	rows, err := pool.Query(ctx, `
SELECT role.name
FROM public.auth_core__role_permission AS grant_row
JOIN public.auth_core__role AS role ON role.id = grant_row.role_id
WHERE role.mode = 'default' AND grant_row.permission = $1
ORDER BY role.name`, permission)
	if err != nil {
		t.Fatalf("read the default-mode grant rows for %s: %v", permission, err)
	}
	defer rows.Close()

	holders := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan a grant row: %v", err)
		}
		holders = append(holders, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the grant rows: %v", err)
	}
	slices.Sort(holders)
	return slices.Compact(holders)
}

// seedProjectMembership gives the dev user 001_initial.sql seeds an `editor`
// role in the project it seeds. 001_initial.sql creates the user and the
// project but assigns no project role, and projectPermissions() reaches the
// central fallback only THROUGH an assigned project role.
//
// The shape below is the shape the project provisioner produces (#342):
// projectprovisioning/steps.go createProjectPermissions() writes one
// auth_core__project_role per central default-mode role name and writes NO
// auth_core__project_role_permission row, deliberately, so that the central
// fallback stays reachable and the corpus grants apply. So a member of a
// Go-provisioned project resolves exactly what this test measures.
// seedProjectMembership returns the id project 1's `editor` role actually has.
//
// The row is minted with id 354 only when it is absent: shared/0111 gives
// project 1 the four roles every provisioned project has, so on a corpus-built
// database `editor` is already there under an id this fixture did not choose.
func seedProjectMembership(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES (354, 1, 'editor')
ON CONFLICT (project_id, name) DO NOTHING`); err != nil {
		t.Fatalf("seed the editor project role: %v", err)
	}
	var roleID int
	if err := pool.QueryRow(ctx,
		`SELECT id FROM public.auth_core__project_role WHERE project_id = 1 AND name = 'editor'`,
	).Scan(&roleID); err != nil {
		t.Fatalf("resolve the editor project role: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES (1, 1, $1)
ON CONFLICT DO NOTHING`, roleID); err != nil {
		t.Fatalf("assign the editor project role: %v", err)
	}
	return roleID
}
