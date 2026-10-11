package pgvector

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestParseProjectNamesAcceptOnlyCanonicalProjectNames(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]int64{"project_1": 1, "project_42": 42, "project_2147483647": 2147483647} {
		if id, ok := ParseProjectDatabaseName(name); !ok || id != want {
			t.Errorf("ParseProjectDatabaseName(%q) = %d, %v", name, id, ok)
		}
		if id, ok := ParseProjectRoleName(name + "_user"); !ok || id != want {
			t.Errorf("ParseProjectRoleName(%q) = %d, %v", name+"_user", id, ok)
		}
	}
	for _, name := range []string{
		"", "project_", "project_0", "project_007", "project_-1", "project_+1", "project_1_user",
		"project_1x", "Project_1", "project_2147483648", "project_99999999999999999999",
		"postgres", "template1", "project_1; DROP", `project_1"`, "project_ 1", "xproject_1",
	} {
		if id, ok := ParseProjectDatabaseName(name); ok {
			t.Errorf("ParseProjectDatabaseName(%q) accepted as %d", name, id)
		}
	}
	for _, name := range []string{"project_1", "project_1_users", "project__user", "project_01_user", "postgres_user", "project_1_user_user"} {
		if id, ok := ParseProjectRoleName(name); ok {
			t.Errorf("ParseProjectRoleName(%q) accepted as %d", name, id)
		}
	}
}

func dropAdmin() AdminConnection {
	return AdminConnection{User: "postgres", Password: "pw", Host: "pgvector", Port: 5432, Database: "vectors"}
}

func TestDropDatabaseRoleDropsDatabaseThenRole(t *testing.T) {
	t.Parallel()

	// database exists, role exists, admin is NOT a member of the role.
	admin := &scriptedConnection{queryResults: []queryResult{{value: true}, {value: true}, {value: false}}}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, _ := NewProvisioner(connector)

	result, err := provisioner.Drop(context.Background(), DropRequest{ProjectID: 42, Admin: dropAdmin()})
	if err != nil {
		t.Fatalf("Drop() = %v", err)
	}
	if !result.DatabaseDropped || !result.RoleDropped {
		t.Fatalf("result = %+v", result)
	}
	if args := admin.execArgs[1]; len(args) != 2 || args[0] != projectLockNamespace || args[1] != int32(42) {
		t.Fatalf("lock args = %#v", args)
	}
	assertQuery(t, admin, 0, databaseExistsSQL, "project_42")
	assertQuery(t, admin, 1, roleExistsSQL, "project_42_user")
	assertQuery(t, admin, 2, roleMemberSQL, "project_42_user")
	// The managed-Postgres order (#1211): lock the role out, become a member so
	// FORCE may terminate its sessions, close the door, then drop.
	assertStatements(t, admin.execStatements, []string{
		"SET lock_timeout = 30000",
		acquireProjectLockSQL,
		"RESET lock_timeout",
		`ALTER ROLE "project_42_user" NOLOGIN`,
		`GRANT "project_42_user" TO CURRENT_USER`,
		`REVOKE CONNECT ON DATABASE "project_42" FROM PUBLIC, "project_42_user"`,
		`DROP DATABASE "project_42" WITH (FORCE)`,
		`DROP ROLE "project_42_user"`,
	})
	if admin.closeCalls != 1 {
		t.Fatalf("close calls = %d", admin.closeCalls)
	}
}

func TestDropSkipsTheGrantWhenTheAdminIsAlreadyAMember(t *testing.T) {
	t.Parallel()

	admin := &scriptedConnection{queryResults: []queryResult{{value: true}, {value: true}, {value: true}}}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, _ := NewProvisioner(connector)

	if _, err := provisioner.Drop(context.Background(), DropRequest{ProjectID: 42, Admin: dropAdmin()}); err != nil {
		t.Fatalf("Drop() = %v", err)
	}
	assertStatements(t, admin.execStatements[3:], []string{
		`ALTER ROLE "project_42_user" NOLOGIN`,
		`REVOKE CONNECT ON DATABASE "project_42" FROM PUBLIC, "project_42_user"`,
		`DROP DATABASE "project_42" WITH (FORCE)`,
		`DROP ROLE "project_42_user"`,
	})
}

func TestDropToleratesAGrantThatRacedAndStopsOnAGrantThatFailed(t *testing.T) {
	t.Parallel()

	// The grant errors but a recheck finds the admin a member: tolerated.
	admin := &scriptedConnection{
		queryResults: []queryResult{{value: true}, {value: true}, {value: false}, {value: true}},
		execErrors:   map[int]error{4: errors.New("role is already a member")},
	}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, _ := NewProvisioner(connector)
	if _, err := provisioner.Drop(context.Background(), DropRequest{ProjectID: 42, Admin: dropAdmin()}); err != nil {
		t.Fatalf("Drop() with a raced grant = %v", err)
	}

	// The grant errors and the admin is still not a member: the drop must not run.
	admin = &scriptedConnection{
		queryResults: []queryResult{{value: true}, {value: true}, {value: false}, {value: false}},
		execErrors:   map[int]error{4: errors.New("permission denied")},
	}
	connector = &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, _ = NewProvisioner(connector)
	if _, err := provisioner.Drop(context.Background(), DropRequest{ProjectID: 42, Admin: dropAdmin()}); err == nil {
		t.Fatal("Drop() succeeded although the grant failed")
	}
	for _, statement := range admin.execStatements {
		if statement == `DROP DATABASE "project_42" WITH (FORCE)` {
			t.Fatalf("dropped the database after a failed grant: %v", admin.execStatements)
		}
	}
}

func TestDropIsANoOpWhenDatabaseAndRoleAreMissing(t *testing.T) {
	t.Parallel()

	admin := &scriptedConnection{queryResults: []queryResult{{value: false}, {value: false}}}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, _ := NewProvisioner(connector)

	result, err := provisioner.Drop(context.Background(), DropRequest{ProjectID: 42, Admin: dropAdmin()})
	if err != nil {
		t.Fatalf("Drop() = %v", err)
	}
	if result != (DropResult{}) {
		t.Fatalf("result = %+v, want nothing dropped", result)
	}
	assertStatements(t, admin.execStatements[3:], nil)
}

func TestDropSchemaModeDropsOnlyTheProjectSchemaAndNoRole(t *testing.T) {
	t.Parallel()

	admin := &scriptedConnection{queryResults: []queryResult{{value: true}}}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, _ := NewProvisioner(connector)

	result, err := provisioner.Drop(context.Background(), DropRequest{ProjectID: 9, Admin: dropAdmin(), Mode: ModeSchema})
	if err != nil {
		t.Fatalf("Drop() = %v", err)
	}
	if !result.SchemaDropped || result.DatabaseDropped || result.RoleDropped {
		t.Fatalf("result = %+v", result)
	}
	assertQuery(t, admin, 0, schemaExistsSQL, "project_9")
	assertStatements(t, admin.execStatements[3:], []string{`DROP SCHEMA "project_9" CASCADE`})
}

func TestDropRefusesNamesThatAreNotTheProjectsOwn(t *testing.T) {
	t.Parallel()

	connector := &scriptedConnector{connections: map[string][]Connection{}}
	provisioner, _ := NewProvisioner(connector)

	cases := map[string]DropRequest{
		"other database":     {ProjectID: 42, Admin: dropAdmin(), Database: "project_43"},
		"system database":    {ProjectID: 42, Admin: dropAdmin(), Database: "postgres"},
		"other role":         {ProjectID: 42, Admin: dropAdmin(), Role: "project_43_user"},
		"role as database":   {ProjectID: 42, Admin: dropAdmin(), Database: "project_42_user"},
		"injection database": {ProjectID: 42, Admin: dropAdmin(), Database: `project_42"; DROP DATABASE x; --`},
	}
	for name, request := range cases {
		if _, err := provisioner.Drop(context.Background(), request); !errors.Is(err, ErrInvalidDropTarget) {
			t.Errorf("%s: err = %v, want ErrInvalidDropTarget", name, err)
		}
	}
	for name, request := range map[string]DropRequest{
		"zero id":       {ProjectID: 0, Admin: dropAdmin()},
		"negative id":   {ProjectID: -1, Admin: dropAdmin()},
		"int4 overflow": {ProjectID: 1 << 40, Admin: dropAdmin()},
		"bad mode":      {ProjectID: 1, Admin: dropAdmin(), Mode: 9},
		"no admin db":   {ProjectID: 1},
	} {
		if _, err := provisioner.Drop(context.Background(), request); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	// A bootstrap database that IS the project database could not be dropped
	// from its own session, and must never be targeted.
	request := DropRequest{ProjectID: 42, Admin: dropAdmin()}
	request.Admin.Database = "project_42"
	if _, err := provisioner.Drop(context.Background(), request); !errors.Is(err, ErrInvalidDropTarget) {
		t.Errorf("bootstrap==target: err = %v", err)
	}
	if len(connector.connectCalls) != 0 {
		t.Fatalf("a refused drop opened connections: %v", connector.connectCalls)
	}
}

func TestDropSQLQuotesIdentifiers(t *testing.T) {
	t.Parallel()

	if got := dropDatabaseSQL(`a"b`); got != `DROP DATABASE "a""b" WITH (FORCE)` {
		t.Fatalf("dropDatabaseSQL = %s", got)
	}
}

// The lock wait is the blocking pg_advisory_lock under a session lock_timeout:
// SQLSTATE 55P03 (lock_not_available) is ErrDropLockTimeout, nothing is
// dropped, and the session is closed.
func TestDropMapsALockTimeoutToErrDropLockTimeout(t *testing.T) {
	t.Parallel()

	admin := &scriptedConnection{execErrors: map[int]error{
		1: &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}}}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, _ := NewProvisioner(connector)

	_, err := provisioner.Drop(context.Background(), DropRequest{ProjectID: 42, Admin: dropAdmin()})
	if !errors.Is(err, ErrDropLockTimeout) {
		t.Fatalf("Drop() = %v, want ErrDropLockTimeout", err)
	}
	assertStatements(t, admin.execStatements, []string{"SET lock_timeout = 30000", acquireProjectLockSQL})
	if len(admin.queryCalls) != 0 {
		t.Fatalf("a timed-out drop ran queries: %v", admin.queryCalls)
	}
	if admin.closeCalls != 1 {
		t.Fatalf("close calls = %d, want the session closed", admin.closeCalls)
	}

	// Any other lock failure is an ordinary provisioning error, not a timeout.
	admin = &scriptedConnection{execErrors: map[int]error{1: &pgconn.PgError{Code: "57014"}}}
	connector = &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, _ = NewProvisioner(connector)
	if _, err := provisioner.Drop(context.Background(), DropRequest{ProjectID: 42, Admin: dropAdmin()}); errors.Is(err, ErrDropLockTimeout) || !errors.Is(err, ErrProvisioning) {
		t.Fatalf("Drop() with a cancelled lock = %v, want ErrProvisioning", err)
	}
}

func TestSetLockTimeoutSQLRendersMilliseconds(t *testing.T) {
	t.Parallel()

	for timeout, want := range map[time.Duration]string{
		30 * time.Second:        "SET lock_timeout = 30000",
		1500 * time.Millisecond: "SET lock_timeout = 1500",
		time.Microsecond:        "SET lock_timeout = 1",
	} {
		if got := setLockTimeoutSQL(timeout); got != want {
			t.Errorf("setLockTimeoutSQL(%v) = %q, want %q", timeout, got, want)
		}
	}
}

// Both drops succeeded and only the close failed: the result stands, and the
// close error is logged, not returned.
func TestDropReturnsTheResultWhenOnlyTheCloseFails(t *testing.T) {
	admin := &scriptedConnection{
		queryResults: []queryResult{{value: true}, {value: true}, {value: true}},
		closeErr:     errors.New("connection reset"),
	}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, _ := NewProvisioner(connector)

	var logged bytes.Buffer
	previous := dropLogger
	dropLogger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&logged, nil)) }
	t.Cleanup(func() { dropLogger = previous })

	result, err := provisioner.Drop(context.Background(), DropRequest{ProjectID: 42, Admin: dropAdmin()})
	if err != nil {
		t.Fatalf("Drop() = %v, want the close failure tolerated", err)
	}
	if !result.DatabaseDropped || !result.RoleDropped {
		t.Fatalf("result = %+v, want both dropped", result)
	}
	if !strings.Contains(logged.String(), "did not close cleanly") {
		t.Fatalf("the close failure was not logged: %q", logged.String())
	}
}
