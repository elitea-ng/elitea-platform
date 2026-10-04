package pgvector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestProvisionDatabaseRoleCreatesCurrentProjectResources(t *testing.T) {
	t.Parallel()

	events := make([]string, 0, 12)
	admin := &scriptedConnection{
		queryResults: []queryResult{{value: false}, {value: false}},
		label:        "admin",
		events:       &events,
	}
	project := &scriptedConnection{label: "project", events: &events}
	connector := &scriptedConnector{connections: map[string][]Connection{
		"vectors":    {admin},
		"project_42": {project},
	}}
	provisioner, err := NewProvisioner(connector)
	if err != nil {
		t.Fatalf("NewProvisioner() error = %v", err)
	}

	result, err := provisioner.Provision(context.Background(), Request{
		ProjectID: 42,
		Admin: AdminConnection{
			User: "postgres", Password: "admin-password", Host: "pgvector", Port: 5432, Database: "vectors",
		},
	})
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}

	if result.Status != "created with new password" {
		t.Fatalf("Status = %q", result.Status)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9]{20}$`).MatchString(result.Password) {
		t.Fatalf("generated password does not match current 20-character contract")
	}
	if result.ProjectDatabase != "project_42" || result.ProjectRole != "project_42_user" || result.User != "project_42_user" {
		t.Fatalf("unexpected project identities: %+v", result)
	}
	wantURL := "postgresql+psycopg://project_42_user:" + result.Password + "@pgvector:5432/project_42"
	if result.ConnectionString != wantURL {
		t.Fatalf("ConnectionString = %q, want %q", result.ConnectionString, wantURL)
	}

	assertQuery(t, admin, 0, roleExistsSQL, "project_42_user")
	assertQuery(t, admin, 1, databaseExistsSQL, "project_42")
	assertStatements(t, admin.execStatements, []string{
		acquireProjectLockSQL,
		createRoleSQL("project_42_user", result.Password),
		`CREATE DATABASE "project_42"`,
		`GRANT ALL PRIVILEGES ON DATABASE "project_42" TO "project_42_user"`,
	})
	assertStatements(t, project.execStatements, []string{
		`GRANT ALL ON SCHEMA public TO "project_42_user"`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO "project_42_user"`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO "project_42_user"`,
		`GRANT ALL ON ALL TABLES IN SCHEMA public TO "project_42_user"`,
		`GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO "project_42_user"`,
		createVectorExtensionSQL,
	})
	if admin.closeCalls != 1 || project.closeCalls != 1 {
		t.Fatalf("close calls = admin %d, project %d", admin.closeCalls, project.closeCalls)
	}
	assertExecArgs(t, admin, 0, projectLockNamespace, int32(42))
	if len(events) == 0 || events[0] != "admin:exec:"+acquireProjectLockSQL {
		t.Fatalf("lock was not acquired before mutation: %#v", events)
	}
	if len(events) < 2 || events[len(events)-2] != "project:close" || events[len(events)-1] != "admin:close" {
		t.Fatalf("administrator lock was not released last by close: %#v", events)
	}
	if got := connector.connectCalls; !equalStrings(got, []string{"vectors", "project_42"}) {
		t.Fatalf("connect calls = %#v", got)
	}
}

func TestProvisionDatabaseRolePreservesExistingPasswordAndIsIdempotent(t *testing.T) {
	t.Parallel()

	const password = "current-password"
	firstAdmin := &scriptedConnection{
		queryResults: []queryResult{{value: true}, {value: true}},
	}
	firstProject := &scriptedConnection{}
	secondAdmin := &scriptedConnection{
		queryResults: []queryResult{{value: true}, {value: true}},
	}
	secondProject := &scriptedConnection{}
	connector := &scriptedConnector{connections: map[string][]Connection{
		"vectors":   {firstAdmin, secondAdmin},
		"project_7": {firstProject, secondProject},
	}}
	provisioner, err := NewProvisioner(connector)
	if err != nil {
		t.Fatalf("NewProvisioner() error = %v", err)
	}
	request := validRequest(7)
	request.Password = password

	for attempt := 0; attempt < 2; attempt++ {
		result, provisionErr := provisioner.Provision(context.Background(), request)
		if provisionErr != nil {
			t.Fatalf("Provision() attempt %d error = %v", attempt+1, provisionErr)
		}
		if result.Status != "password reset" || result.Password != password {
			t.Fatalf("attempt %d result = %+v", attempt+1, result)
		}
	}

	for _, connection := range []*scriptedConnection{firstAdmin, secondAdmin} {
		assertStatements(t, connection.execStatements, []string{
			acquireProjectLockSQL,
			alterRoleSQL("project_7_user", password),
			`GRANT ALL PRIVILEGES ON DATABASE "project_7" TO "project_7_user"`,
		})
	}
}

func TestProvisionEmptyPasswordNeverRotatesExistingRole(t *testing.T) {
	t.Parallel()

	admin := &scriptedConnection{queryResults: []queryResult{{value: true}}}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, err := NewProvisioner(connector)
	if err != nil {
		t.Fatalf("NewProvisioner() error = %v", err)
	}

	result, err := provisioner.Provision(context.Background(), validRequest(73))
	var passwordRequired *PasswordRequiredError
	if !errors.As(err, &passwordRequired) {
		t.Fatalf("Provision() error = %v, want PasswordRequiredError", err)
	}
	if result != (Result{}) {
		t.Fatalf("Provision() result = %+v, want zero result", result)
	}
	if strings.Contains(err.Error(), "73") || strings.Contains(err.Error(), "project_") {
		t.Fatalf("PasswordRequiredError leaked project identity: %v", err)
	}
	assertQuery(t, admin, 0, roleExistsSQL, "project_73_user")
	assertStatements(t, admin.execStatements, []string{acquireProjectLockSQL})
	assertExecArgs(t, admin, 0, projectLockNamespace, int32(73))
	if admin.closeCalls != 1 {
		t.Fatalf("administrator lock was not released by close")
	}
	if len(connector.connectCalls) != 1 || connector.connectCalls[0] != "vectors" {
		t.Fatalf("unexpected target-database connection: %#v", connector.connectCalls)
	}
}

func TestProvisionRecoversConcurrentRoleAndDatabaseCreates(t *testing.T) {
	t.Parallel()

	admin := &scriptedConnection{
		queryResults: []queryResult{
			{value: false},
			{value: true},
			{value: false},
			{value: true},
		},
		execErrors: map[int]error{
			1: errors.New("duplicate role"),
			3: errors.New("duplicate database"),
		},
	}
	project := &scriptedConnection{}
	connector := &scriptedConnector{connections: map[string][]Connection{
		"vectors":   {admin},
		"project_9": {project},
	}}
	provisioner, err := NewProvisioner(connector)
	if err != nil {
		t.Fatalf("NewProvisioner() error = %v", err)
	}
	request := validRequest(9)
	request.Password = "stable-password"

	result, err := provisioner.Provision(context.Background(), request)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if result.Status != "password reset" {
		t.Fatalf("Status = %q", result.Status)
	}
	assertStatements(t, admin.execStatements, []string{
		acquireProjectLockSQL,
		createRoleSQL("project_9_user", "stable-password"),
		alterRoleSQL("project_9_user", "stable-password"),
		`CREATE DATABASE "project_9"`,
		`GRANT ALL PRIVILEGES ON DATABASE "project_9" TO "project_9_user"`,
	})
}

func TestProvisionExistingAdminUserGrantsBothUsersAndEscapesURL(t *testing.T) {
	t.Parallel()

	admin := &scriptedConnection{queryResults: []queryResult{{value: false}, {value: false}}}
	project := &scriptedConnection{}
	connector := &scriptedConnector{connections: map[string][]Connection{
		"vectors":     {admin},
		"project_101": {project},
	}}
	provisioner, err := NewProvisioner(connector)
	if err != nil {
		t.Fatalf("NewProvisioner() error = %v", err)
	}
	request := validRequest(101)
	request.Admin.User = `admin"; DROP ROLE victim; --`
	request.Admin.Password = "admin:p@ss/word"
	request.Password = "project'pass\\word"
	request.UseExistingAdminUser = true

	result, err := provisioner.Provision(context.Background(), request)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	quotedAdmin := `"admin""; DROP ROLE victim; --"`
	assertStatements(t, admin.execStatements, []string{
		acquireProjectLockSQL,
		`CREATE USER "project_101_user" WITH PASSWORD E'project''pass\\word'`,
		`CREATE DATABASE "project_101"`,
		`GRANT ALL PRIVILEGES ON DATABASE "project_101" TO "project_101_user"`,
		`GRANT ALL PRIVILEGES ON DATABASE "project_101" TO ` + quotedAdmin,
	})
	assertStatements(t, project.execStatements, []string{
		`GRANT ALL ON SCHEMA public TO "project_101_user"`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO "project_101_user"`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO "project_101_user"`,
		`GRANT ALL ON ALL TABLES IN SCHEMA public TO "project_101_user"`,
		`GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO "project_101_user"`,
		`GRANT ALL ON SCHEMA public TO ` + quotedAdmin,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO ` + quotedAdmin,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO ` + quotedAdmin,
		`GRANT ALL ON ALL TABLES IN SCHEMA public TO ` + quotedAdmin,
		`GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO ` + quotedAdmin,
		createVectorExtensionSQL,
	})
	if result.User != request.Admin.User {
		t.Fatalf("User = %q", result.User)
	}
	if strings.Contains(result.ConnectionString, request.Admin.Password) {
		t.Fatalf("connection string password was not URL encoded")
	}
	wantURL := "postgresql+psycopg://admin%22;%20DROP%20ROLE%20victim;%20--:admin%3Ap%40ss%2Fword@localhost:5432/project_101"
	if result.ConnectionString != wantURL {
		t.Fatalf("ConnectionString = %q, want %q", result.ConnectionString, wantURL)
	}
}

func TestProvisionSchemaModePreservesCurrentCompatibilityContract(t *testing.T) {
	t.Parallel()

	admin := &scriptedConnection{queryResults: []queryResult{{value: false}}}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, err := NewProvisioner(connector)
	if err != nil {
		t.Fatalf("NewProvisioner() error = %v", err)
	}
	request := validRequest(33)
	request.Mode = ModeSchema
	request.Password = "ignored-project-password"

	result, err := provisioner.Provision(context.Background(), request)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	assertStatements(t, admin.execStatements, []string{acquireProjectLockSQL, `CREATE SCHEMA "project_33"`})
	if result.Status != "created" || result.Password != request.Admin.Password || result.User != request.Admin.User {
		t.Fatalf("result = %+v", result)
	}
	if result.Schema != "project_33" || result.ProjectDatabase != "project_33" || result.ProjectRole != "project_33_user" {
		t.Fatalf("result identities = %+v", result)
	}
	wantURL := "postgresql+psycopg://postgres:admin-password@localhost:5432/vectors?options=-csearch_path%3Dproject_33,public"
	if result.ConnectionString != wantURL {
		t.Fatalf("ConnectionString = %q, want %q", result.ConnectionString, wantURL)
	}
	if len(connector.connectCalls) != 1 || connector.connectCalls[0] != "vectors" {
		t.Fatalf("connect calls = %#v", connector.connectCalls)
	}
}

func TestProjectConnectionParametersFromURLKeepsOnlyBoundedTransportOptions(t *testing.T) {
	t.Parallel()

	connectionString := "postgresql://admin:password@primary:5432/vectors?" +
		"application_name=must-not-copy&channel_binding=require&connect_timeout=15&" +
		"load_balance_hosts=random&sslcert=%2Fetc%2Felitea%2Fclient.crt&" +
		"sslkey=%2Fetc%2Felitea%2Fclient.key&sslmode=verify-full&" +
		"sslnegotiation=direct&sslrootcert=%2Fetc%2Felitea%2Froot.crt&" +
		"target_session_attrs=read-write"

	got, err := ProjectConnectionParametersFromURL(connectionString)
	if err != nil {
		t.Fatal(err)
	}
	want := "channel_binding=require&connect_timeout=15&load_balance_hosts=random&" +
		"sslcert=%2Fetc%2Felitea%2Fclient.crt&sslkey=%2Fetc%2Felitea%2Fclient.key&" +
		"sslmode=verify-full&sslnegotiation=direct&" +
		"sslrootcert=%2Fetc%2Felitea%2Froot.crt&target_session_attrs=read-write"
	if got != want {
		t.Fatalf("ProjectConnectionParametersFromURL() = %q, want %q", got, want)
	}

	unknownOnly, err := ProjectConnectionParametersFromURL(
		"postgresql://admin:password@primary:5432/vectors?application_name=ignored",
	)
	if err != nil || unknownOnly != "" {
		t.Fatalf("unknown parameters = %q, error = %v", unknownOnly, err)
	}
	keywordDSN, err := ProjectConnectionParametersFromURL(
		"host=primary dbname=vectors sslmode=verify-full",
	)
	if err != nil || keywordDSN != "" {
		t.Fatalf("keyword DSN parameters = %q, error = %v", keywordDSN, err)
	}
}

func TestProjectConnectionParametersFromURLRejectsUnsafeRecognizedOptions(t *testing.T) {
	t.Parallel()

	invalid := []string{
		"https://admin:password@primary/vectors?sslmode=require",
		"postgresql://admin:password@primary/vectors?sslmode=unsafe",
		"postgresql://admin:password@primary/vectors?sslmode=require&sslmode=verify-full",
		"postgresql://admin:password@primary/vectors?connect_timeout=0",
		"postgresql://admin:password@primary/vectors?sslrootcert=%00secret",
		"postgresql://admin:password@primary/vectors?sslrootcert=%zz",
	}
	for _, connectionString := range invalid {
		if _, err := ProjectConnectionParametersFromURL(connectionString); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("connection string %q error = %v", connectionString, err)
		}
	}
}

func TestSQLAlchemyURLPreservesProjectParametersAndSchemaOption(t *testing.T) {
	t.Parallel()

	admin := validRequest(1).Admin
	admin.ProjectConnectionParams = "connect_timeout=15&sslmode=verify-full&target_session_attrs=read-write"
	got := sqlalchemyURL(admin, "project_user", "project-password", "project_database", "")
	want := "postgresql+psycopg://project_user:project-password@localhost:5432/project_database?" +
		"connect_timeout=15&sslmode=verify-full&target_session_attrs=read-write"
	if got != want {
		t.Fatalf("database role URL = %q, want %q", got, want)
	}

	got = sqlalchemyURL(admin, "postgres", "admin-password", "vectors", "project_1")
	want = "postgresql+psycopg://postgres:admin-password@localhost:5432/vectors?" +
		"connect_timeout=15&options=-csearch_path%3Dproject_1,public&" +
		"sslmode=verify-full&target_session_attrs=read-write"
	if got != want {
		t.Fatalf("schema URL = %q, want %q", got, want)
	}
}

func TestProvisionPreservesCancellationAndClosesOwnedConnection(t *testing.T) {
	t.Parallel()

	admin := &scriptedConnection{
		queryResults: []queryResult{{value: false}},
		execErrors:   map[int]error{1: fmt.Errorf("secret in lower error: %w", context.Canceled)},
	}
	connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
	provisioner, err := NewProvisioner(connector)
	if err != nil {
		t.Fatalf("NewProvisioner() error = %v", err)
	}
	request := validRequest(1)
	request.Password = "must-not-leak"

	_, err = provisioner.Provision(context.Background(), request)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Provision() error = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), request.Password) || strings.Contains(err.Error(), "secret in lower error") {
		t.Fatalf("Provision() leaked dependency details: %v", err)
	}
	if admin.closeCalls != 1 {
		t.Fatalf("admin close calls = %d", admin.closeCalls)
	}
	if len(admin.queryCalls) != 1 || len(admin.execStatements) != 2 {
		t.Fatalf("work continued after cancellation: queries=%d execs=%d", len(admin.queryCalls), len(admin.execStatements))
	}
}

func TestProvisionRejectsCanceledContextBeforeConnecting(t *testing.T) {
	t.Parallel()

	connector := &scriptedConnector{}
	provisioner, err := NewProvisioner(connector)
	if err != nil {
		t.Fatalf("NewProvisioner() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = provisioner.Provision(ctx, validRequest(1))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Provision() error = %v, want context.Canceled", err)
	}
	if len(connector.connectCalls) != 0 {
		t.Fatalf("Connect() called after cancellation")
	}
}

func TestProvisionDoesNotLeakConnectorOrSQLSecretsInErrors(t *testing.T) {
	t.Parallel()

	t.Run("connect", func(t *testing.T) {
		connector := &scriptedConnector{connectErr: errors.New("postgresql://postgres:admin-secret@db/vectors")}
		provisioner, newErr := NewProvisioner(connector)
		if newErr != nil {
			t.Fatalf("NewProvisioner() error = %v", newErr)
		}
		_, provisionErr := provisioner.Provision(context.Background(), validRequest(2))
		if !errors.Is(provisionErr, ErrProvisioning) || strings.Contains(provisionErr.Error(), "admin-secret") {
			t.Fatalf("Provision() error = %v", provisionErr)
		}
	})

	t.Run("statement", func(t *testing.T) {
		const password = "statement-secret"
		admin := &scriptedConnection{
			queryResults: []queryResult{{value: false}, {value: false}},
			execErrors: map[int]error{
				1: errors.New(`CREATE USER "project_2_user" WITH PASSWORD E'statement-secret' failed`),
			},
		}
		connector := &scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}}
		provisioner, newErr := NewProvisioner(connector)
		if newErr != nil {
			t.Fatalf("NewProvisioner() error = %v", newErr)
		}
		request := validRequest(2)
		request.Password = password
		_, provisionErr := provisioner.Provision(context.Background(), request)
		if !errors.Is(provisionErr, ErrProvisioning) || strings.Contains(provisionErr.Error(), password) {
			t.Fatalf("Provision() error = %v", provisionErr)
		}
	})
}

func TestProvisionValidationIsBounded(t *testing.T) {
	t.Parallel()

	connector := &scriptedConnector{}
	provisioner, err := NewProvisioner(connector)
	if err != nil {
		t.Fatalf("NewProvisioner() error = %v", err)
	}
	tooLongName := strings.Repeat("x", maxPostgresNameBytes+1)
	tooLongPassword := strings.Repeat("x", maxPasswordBytes+1)
	tests := []struct {
		name    string
		request Request
	}{
		{name: "non-positive project", request: validRequest(0)},
		{name: "project exceeds current int4", request: validRequest(1 << 31)},
		{name: "unknown mode", request: func() Request { r := validRequest(1); r.Mode = Mode(99); return r }()},
		{name: "empty admin user", request: func() Request { r := validRequest(1); r.Admin.User = ""; return r }()},
		{name: "long database", request: func() Request { r := validRequest(1); r.Admin.Database = tooLongName; return r }()},
		{name: "newline host", request: func() Request { r := validRequest(1); r.Admin.Host = "db\nattacker"; return r }()},
		{name: "zero port", request: func() Request { r := validRequest(1); r.Admin.Port = 0; return r }()},
		{name: "NUL password", request: func() Request { r := validRequest(1); r.Password = "bad\x00password"; return r }()},
		{name: "long password", request: func() Request { r := validRequest(1); r.Password = tooLongPassword; return r }()},
		{name: "noncanonical project parameters", request: func() Request {
			r := validRequest(1)
			r.Admin.ProjectConnectionParams = "sslmode=require&connect_timeout=15"
			return r
		}()},
		{name: "unknown project parameter", request: func() Request {
			r := validRequest(1)
			r.Admin.ProjectConnectionParams = "application_name=not-allowed"
			return r
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, provisionErr := provisioner.Provision(context.Background(), test.request)
			if !errors.Is(provisionErr, ErrInvalidRequest) {
				t.Fatalf("Provision() error = %v", provisionErr)
			}
		})
	}
	if len(connector.connectCalls) != 0 {
		t.Fatalf("Connect() called for invalid request")
	}
}

func TestPasswordGenerationUsesBoundedUnbiasedAlphabet(t *testing.T) {
	t.Parallel()

	password, err := generatePassword(bytes.NewReader(make([]byte, passwordLength)))
	if err != nil {
		t.Fatalf("generatePassword() error = %v", err)
	}
	if password != strings.Repeat("a", passwordLength) {
		t.Fatalf("password = %q", password)
	}
	_, err = generatePassword(bytes.NewReader(bytes.Repeat([]byte{255}, passwordLength*8)))
	if !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("generatePassword() error = %v, want io.ErrNoProgress", err)
	}
}

func TestSQLQuotingKeepsIdentifiersAndPasswordsInsideTokens(t *testing.T) {
	t.Parallel()

	if got, want := quoteIdentifier(`admin"; DROP ROLE victim; --`), `"admin""; DROP ROLE victim; --"`; got != want {
		t.Fatalf("quoteIdentifier() = %q, want %q", got, want)
	}
	if got, want := quoteLiteral("pa'ss\\word; DROP ROLE victim; --"), `E'pa''ss\\word; DROP ROLE victim; --'`; got != want {
		t.Fatalf("quoteLiteral() = %q, want %q", got, want)
	}
}

func TestPGXConnectorCopiesConfiguration(t *testing.T) {
	t.Parallel()

	if _, err := NewPGXConnector(nil); !errors.Is(err, ErrInvalidConnector) {
		t.Fatalf("NewPGXConnector(nil) error = %v", err)
	}
	config, err := pgx.ParseConfig("postgresql://admin:password@localhost:5432/vectors")
	if err != nil {
		t.Fatalf("pgx.ParseConfig() error = %v", err)
	}
	connector, err := NewPGXConnector(config)
	if err != nil {
		t.Fatalf("NewPGXConnector() error = %v", err)
	}
	config.Database = "mutated"
	if connector.config.Database != "vectors" {
		t.Fatalf("connector config aliases caller config")
	}
}

func TestProvisionWithHandoffKeepsProjectLockThroughPersistence(t *testing.T) {
	t.Parallel()

	events := []string{}
	admin := &scriptedConnection{
		queryResults: []queryResult{{value: true}, {value: true}},
		label:        "admin",
		events:       &events,
	}
	project := &scriptedConnection{label: "project", events: &events}
	provisioner, err := NewProvisioner(&scriptedConnector{connections: map[string][]Connection{
		"vectors":   {admin},
		"project_7": {project},
	}})
	if err != nil {
		t.Fatal(err)
	}
	request := validRequest(7)
	request.Password = "stable-password"

	result, err := provisioner.ProvisionWithHandoff(context.Background(), request, func(_ context.Context, value Result) error {
		if admin.closeCalls != 0 || project.closeCalls != 1 {
			t.Fatalf("handoff ran outside lock ownership: admin close=%d project close=%d", admin.closeCalls, project.closeCalls)
		}
		if value.Password != "stable-password" || value.ConnectionString == "" {
			t.Fatal("handoff did not receive the completed project material")
		}
		events = append(events, "handoff")
		return nil
	})
	if err != nil || result.Password != "stable-password" {
		t.Fatalf("ProvisionWithHandoff() result status=%q error=%v", result.Status, err)
	}
	if len(events) < 3 || events[len(events)-2] != "handoff" || events[len(events)-1] != "admin:close" {
		t.Fatalf("handoff/lock release order = %#v", events)
	}
}

func TestProvisionWithHandoffFailureReleasesLockAndReturnsNoMaterial(t *testing.T) {
	t.Parallel()

	admin := &scriptedConnection{queryResults: []queryResult{{value: true}, {value: true}}}
	project := &scriptedConnection{}
	provisioner, err := NewProvisioner(&scriptedConnector{connections: map[string][]Connection{
		"vectors":   {admin},
		"project_9": {project},
	}})
	if err != nil {
		t.Fatal(err)
	}
	request := validRequest(9)
	request.Password = "stable-password"
	handoffErr := errors.New("safe handoff failure")

	result, err := provisioner.ProvisionWithHandoff(context.Background(), request, func(context.Context, Result) error {
		return handoffErr
	})
	if !errors.Is(err, handoffErr) || result != (Result{}) {
		t.Fatalf("ProvisionWithHandoff() did not return a zero result with the safe handoff error: %v", err)
	}
	if admin.closeCalls != 1 || project.closeCalls != 1 {
		t.Fatalf("handoff failure close calls = admin %d project %d", admin.closeCalls, project.closeCalls)
	}
	if _, err := provisioner.ProvisionWithHandoff(context.Background(), request, nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil handoff error = %v", err)
	}
}

func TestProvisionWithHandoffCoversSchemaMode(t *testing.T) {
	t.Parallel()

	events := []string{}
	admin := &scriptedConnection{
		queryResults: []queryResult{{value: false}},
		label:        "admin",
		events:       &events,
	}
	provisioner, err := NewProvisioner(&scriptedConnector{connections: map[string][]Connection{"vectors": {admin}}})
	if err != nil {
		t.Fatal(err)
	}
	request := validRequest(11)
	request.Mode = ModeSchema

	_, err = provisioner.ProvisionWithHandoff(context.Background(), request, func(_ context.Context, value Result) error {
		if value.Schema != "project_11" || admin.closeCalls != 0 {
			t.Fatalf("schema handoff result is outside lock: schema=%q closes=%d", value.Schema, admin.closeCalls)
		}
		events = append(events, "handoff")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[len(events)-2] != "handoff" || events[len(events)-1] != "admin:close" {
		t.Fatalf("schema handoff/lock release order = %#v", events)
	}
}

func validRequest(projectID int64) Request {
	return Request{
		ProjectID: projectID,
		Admin: AdminConnection{
			User: "postgres", Password: "admin-password", Host: "localhost", Port: 5432, Database: "vectors",
		},
	}
}

type queryResult struct {
	value bool
	err   error
}

type queryCall struct {
	statement string
	args      []any
}

type scriptedConnection struct {
	queryResults   []queryResult
	queryCalls     []queryCall
	execStatements []string
	execArgs       [][]any
	execErrors     map[int]error
	closeCalls     int
	closeErr       error
	label          string
	events         *[]string
}

func (c *scriptedConnection) QueryBool(_ context.Context, statement string, args ...any) (bool, error) {
	c.queryCalls = append(c.queryCalls, queryCall{statement: statement, args: append([]any(nil), args...)})
	index := len(c.queryCalls) - 1
	if index >= len(c.queryResults) {
		return false, errors.New("unexpected query")
	}
	return c.queryResults[index].value, c.queryResults[index].err
}

func (c *scriptedConnection) Exec(_ context.Context, statement string, args ...any) error {
	c.execStatements = append(c.execStatements, statement)
	c.execArgs = append(c.execArgs, append([]any(nil), args...))
	if c.events != nil {
		*c.events = append(*c.events, c.label+":exec:"+statement)
	}
	if err := c.execErrors[len(c.execStatements)-1]; err != nil {
		return err
	}
	return nil
}

func (c *scriptedConnection) Close(_ context.Context) error {
	c.closeCalls++
	if c.events != nil {
		*c.events = append(*c.events, c.label+":close")
	}
	return c.closeErr
}

type scriptedConnector struct {
	connections  map[string][]Connection
	connectCalls []string
	connectErr   error
}

func (c *scriptedConnector) Connect(_ context.Context, database string) (Connection, error) {
	c.connectCalls = append(c.connectCalls, database)
	if c.connectErr != nil {
		return nil, c.connectErr
	}
	available := c.connections[database]
	if len(available) == 0 {
		return nil, errors.New("unexpected database")
	}
	connection := available[0]
	c.connections[database] = available[1:]
	return connection, nil
}

func assertQuery(t *testing.T, connection *scriptedConnection, index int, statement string, arg any) {
	t.Helper()
	if len(connection.queryCalls) <= index {
		t.Fatalf("query %d missing", index)
	}
	call := connection.queryCalls[index]
	if call.statement != statement || len(call.args) != 1 || call.args[0] != arg {
		t.Fatalf("query %d = %#v, want statement %q arg %#v", index, call, statement, arg)
	}
}

func assertStatements(t *testing.T, got []string, want []string) {
	t.Helper()
	if !equalStrings(got, want) {
		t.Fatalf("statements:\n got: %#v\nwant: %#v", got, want)
	}
}

func assertExecArgs(t *testing.T, connection *scriptedConnection, index int, want ...any) {
	t.Helper()
	if len(connection.execArgs) <= index {
		t.Fatalf("exec args %d missing", index)
	}
	got := connection.execArgs[index]
	if len(got) != len(want) {
		t.Fatalf("exec args %d = %#v, want %#v", index, got, want)
	}
	for argumentIndex := range got {
		if got[argumentIndex] != want[argumentIndex] {
			t.Fatalf("exec args %d = %#v, want %#v", index, got, want)
		}
	}
}

func equalStrings(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
