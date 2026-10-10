package pgvector

import (
	"context"
	"errors"
	"testing"
	"time"
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

	// lock, database exists, role exists, admin is NOT a member of the role.
	admin := &scriptedConnection{queryResults: []queryResult{{value: true}, {value: true}, {value: true}, {value: false}}}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, _ := NewProvisioner(connector)

	result, err := provisioner.Drop(context.Background(), DropRequest{ProjectID: 42, Admin: dropAdmin()})
	if err != nil {
		t.Fatalf("Drop() = %v", err)
	}
	if !result.DatabaseDropped || !result.RoleDropped {
		t.Fatalf("result = %+v", result)
	}
	if call := admin.queryCalls[0]; call.statement != tryProjectLockSQL ||
		len(call.args) != 2 || call.args[0] != projectLockNamespace || call.args[1] != int32(42) {
		t.Fatalf("lock query = %#v", call)
	}
	assertQuery(t, admin, 1, databaseExistsSQL, "project_42")
	assertQuery(t, admin, 2, roleExistsSQL, "project_42_user")
	assertQuery(t, admin, 3, roleMemberSQL, "project_42_user")
	// The managed-Postgres order (#1211): lock the role out, become a member so
	// FORCE may terminate its sessions, close the door, then drop.
	assertStatements(t, admin.execStatements, []string{
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

	admin := &scriptedConnection{queryResults: []queryResult{{value: true}, {value: true}, {value: true}, {value: true}}}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, _ := NewProvisioner(connector)

	if _, err := provisioner.Drop(context.Background(), DropRequest{ProjectID: 42, Admin: dropAdmin()}); err != nil {
		t.Fatalf("Drop() = %v", err)
	}
	assertStatements(t, admin.execStatements, []string{
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
		queryResults: []queryResult{{value: true}, {value: true}, {value: true}, {value: false}, {value: true}},
		execErrors:   map[int]error{1: errors.New("role is already a member")},
	}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, _ := NewProvisioner(connector)
	if _, err := provisioner.Drop(context.Background(), DropRequest{ProjectID: 42, Admin: dropAdmin()}); err != nil {
		t.Fatalf("Drop() with a raced grant = %v", err)
	}

	// The grant errors and the admin is still not a member: the drop must not run.
	admin = &scriptedConnection{
		queryResults: []queryResult{{value: true}, {value: true}, {value: true}, {value: false}, {value: false}},
		execErrors:   map[int]error{1: errors.New("permission denied")},
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

	admin := &scriptedConnection{queryResults: []queryResult{{value: true}, {value: false}, {value: false}}}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, _ := NewProvisioner(connector)

	result, err := provisioner.Drop(context.Background(), DropRequest{ProjectID: 42, Admin: dropAdmin()})
	if err != nil {
		t.Fatalf("Drop() = %v", err)
	}
	if result != (DropResult{}) {
		t.Fatalf("result = %+v, want nothing dropped", result)
	}
	assertStatements(t, admin.execStatements, nil)
}

func TestDropSchemaModeDropsOnlyTheProjectSchemaAndNoRole(t *testing.T) {
	t.Parallel()

	admin := &scriptedConnection{queryResults: []queryResult{{value: true}, {value: true}}}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, _ := NewProvisioner(connector)

	result, err := provisioner.Drop(context.Background(), DropRequest{ProjectID: 9, Admin: dropAdmin(), Mode: ModeSchema})
	if err != nil {
		t.Fatalf("Drop() = %v", err)
	}
	if !result.SchemaDropped || result.DatabaseDropped || result.RoleDropped {
		t.Fatalf("result = %+v", result)
	}
	assertQuery(t, admin, 1, schemaExistsSQL, "project_9")
	assertStatements(t, admin.execStatements, []string{`DROP SCHEMA "project_9" CASCADE`})
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

func TestDropWaitsBrieflyForTheProjectLockAndThenSucceeds(t *testing.T) {
	shrinkLockWait(t, 2*time.Second)

	admin := &scriptedConnection{queryResults: []queryResult{
		{value: false}, {value: false}, {value: true}, {value: false}, {value: false}}}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, _ := NewProvisioner(connector)

	if _, err := provisioner.Drop(context.Background(), DropRequest{ProjectID: 42, Admin: dropAdmin()}); err != nil {
		t.Fatalf("Drop() = %v", err)
	}
	if got := len(admin.queryCalls); got != 5 {
		t.Fatalf("queries = %d, want 3 lock attempts then the 2 existence checks", got)
	}
}

func TestDropGivesUpWhenTheProjectLockIsNeverFreed(t *testing.T) {
	shrinkLockWait(t, 50*time.Millisecond)

	results := make([]queryResult, 200)
	admin := &scriptedConnection{queryResults: results}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, _ := NewProvisioner(connector)

	// A background context with no deadline: the wait must be bounded by the
	// drop's own timeout, the way Deprovision's context.WithoutCancel is.
	started := time.Now()
	_, err := provisioner.Drop(context.Background(), DropRequest{ProjectID: 42, Admin: dropAdmin()})
	if !errors.Is(err, ErrDropLockTimeout) {
		t.Fatalf("Drop() = %v, want ErrDropLockTimeout", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Drop() waited %s", elapsed)
	}
	if len(admin.execStatements) != 0 {
		t.Fatalf("a timed-out drop executed %v", admin.execStatements)
	}
	if admin.closeCalls != 1 {
		t.Fatalf("close calls = %d, want the session closed", admin.closeCalls)
	}
}

func shrinkLockWait(t *testing.T, timeout time.Duration) {
	t.Helper()
	oldTimeout, oldInitial, oldMax := dropLockTimeout, dropLockPollInitial, dropLockPollMax
	dropLockTimeout, dropLockPollInitial, dropLockPollMax = timeout, 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { dropLockTimeout, dropLockPollInitial, dropLockPollMax = oldTimeout, oldInitial, oldMax })
}
