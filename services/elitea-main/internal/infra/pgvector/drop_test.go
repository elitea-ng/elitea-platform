package pgvector

import (
	"context"
	"errors"
	"testing"
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

	admin := &scriptedConnection{queryResults: []queryResult{{value: true}, {value: true}}}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, _ := NewProvisioner(connector)

	result, err := provisioner.Drop(context.Background(), DropRequest{ProjectID: 42, Admin: dropAdmin()})
	if err != nil {
		t.Fatalf("Drop() = %v", err)
	}
	if !result.DatabaseDropped || !result.RoleDropped {
		t.Fatalf("result = %+v", result)
	}
	assertQuery(t, admin, 0, databaseExistsSQL, "project_42")
	assertQuery(t, admin, 1, roleExistsSQL, "project_42_user")
	assertStatements(t, admin.execStatements, []string{
		acquireProjectLockSQL,
		`DROP DATABASE "project_42" WITH (FORCE)`,
		`DROP ROLE "project_42_user"`,
	})
	if admin.closeCalls != 1 {
		t.Fatalf("close calls = %d", admin.closeCalls)
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
	assertStatements(t, admin.execStatements, []string{acquireProjectLockSQL})
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
	assertStatements(t, admin.execStatements, []string{acquireProjectLockSQL, `DROP SCHEMA "project_9" CASCADE`})
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
