// Package pgvector provisions the per-project PostgreSQL isolation used by the
// current Elitea vector-store implementation. Configuration lookup and vault
// persistence deliberately remain application-layer responsibilities.
package pgvector

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// PasswordRequiredError means the project role already exists but the caller
// did not supply its vault-backed password. The error deliberately carries no
// project identity or secret. Callers should reload the project vault and retry;
// password rotation is a separate explicit intent.
type PasswordRequiredError struct{}

func (*PasswordRequiredError) Error() string {
	return "pgvector: stored project password required"
}

const (
	passwordLength       = 20
	passwordAlphabet     = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	maxPasswordBytes     = 4096
	maxPostgresNameBytes = 63
	maxHostBytes         = 1024
	maxConnectionOptions = 4096
	closeTimeout         = 5 * time.Second
)

var (
	// ErrInvalidRequest means the provisioning input cannot identify a safe,
	// bounded PostgreSQL target.
	ErrInvalidRequest = errors.New("pgvector: invalid provisioning request")
	// ErrInvalidConnector means the provisioner has no usable database connector.
	ErrInvalidConnector = errors.New("pgvector: invalid connector")
	// ErrProvisioning means a PostgreSQL operation failed. Dependency errors are
	// intentionally not wrapped because they can contain a DSN or SQL statement
	// with a password; cancellation and deadline errors remain distinguishable.
	ErrProvisioning = errors.New("pgvector: provisioning failed")
)

// Mode selects the current per-project isolation behavior.
type Mode uint8

const (
	// ModeDatabaseRole creates one database and login role per project. This is
	// the current default and the intended production mode.
	ModeDatabaseRole Mode = iota
	// ModeSchema preserves the current compatibility mode: it creates a schema
	// in the source database and connects with the source administrator.
	ModeSchema
)

// Connection is one dedicated, auto-commit PostgreSQL session. Implementations
// are owned by one Provision call and need not be safe for concurrent use.
// QueryBool must scan exactly one boolean result.
type Connection interface {
	QueryBool(ctx context.Context, statement string, args ...any) (bool, error)
	Exec(ctx context.Context, statement string, args ...any) error
	Close(ctx context.Context) error
}

// Connector opens a dedicated session against a database using the trusted
// source PgVector administrator credentials supplied by composition. It must
// not include credentials or its DSN in returned errors.
type Connector interface {
	Connect(ctx context.Context, database string) (Connection, error)
}

// AdminConnection is the already-resolved public PgVector bootstrap target.
// It is used only to validate names and build the current SQLAlchemy connection
// string; Connector owns the corresponding live transport credentials.
type AdminConnection struct {
	User                    string
	Password                string
	Host                    string
	Port                    uint16
	Database                string
	ProjectConnectionParams string
}

// Request contains one project's provisioning intent. An empty Password asks
// Provisioner to generate the current 20-character alphanumeric password only
// when the project role does not exist. It never implicitly rotates an existing
// role; that case returns PasswordRequiredError.
type Request struct {
	ProjectID            int64
	Admin                AdminConnection
	Mode                 Mode
	UseExistingAdminUser bool
	Password             string
}

// Result is the plaintext material the trusted application layer must persist
// in the project's encrypted vault. Callers must not log or serialize it into
// commands, events, traces, or ordinary configuration snapshots.
type Result struct {
	Status           string
	Password         string
	ConnectionString string
	ProjectDatabase  string
	ProjectRole      string
	User             string
	Schema           string
}

// Provisioner synchronously owns at most two PostgreSQL connections while it
// keeps the administrator session lock across target-database setup. It starts
// no goroutines and is safe for concurrent use when Connector is.
type Provisioner struct {
	connector Connector
	random    io.Reader
}

// Handoff persists Result while the provisioner's per-project PostgreSQL
// advisory lock is still held. Implementations must not retain Result and must
// return redacted errors because it contains plaintext project material.
type Handoff func(context.Context, Result) error

// NewProvisioner creates a per-project PgVector provisioner.
func NewProvisioner(connector Connector) (*Provisioner, error) {
	if connector == nil {
		return nil, ErrInvalidConnector
	}
	return &Provisioner{connector: connector, random: rand.Reader}, nil
}

// Provision converges PostgreSQL to the requested project state. Catalog
// checks plus post-error rechecks make retries idempotent, including a
// concurrent create that wins after this call's initial existence check.
func (p *Provisioner) Provision(ctx context.Context, request Request) (Result, error) {
	return p.provision(ctx, request, nil)
}

// ProvisionWithHandoff is Provision with a cross-store convergence boundary.
// The callback runs after the database/role or schema is ready and before the
// administrator connection releases the per-project advisory lock. This keeps
// concurrent successful provisioners from committing vault material out of
// PostgreSQL role-password order. External PostgreSQL effects and a different
// durable store still cannot be one atomic transaction; a failed handoff must
// be retried with the same authoritative reconciliation policy.
func (p *Provisioner) ProvisionWithHandoff(
	ctx context.Context,
	request Request,
	handoff Handoff,
) (Result, error) {
	if handoff == nil {
		return Result{}, ErrInvalidRequest
	}
	return p.provision(ctx, request, handoff)
}

func (p *Provisioner) provision(ctx context.Context, request Request, handoff Handoff) (Result, error) {
	if ctx == nil || p == nil || p.connector == nil || p.random == nil {
		return Result{}, ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	database, role, err := validateRequest(request)
	if err != nil {
		return Result{}, err
	}
	if request.Mode == ModeSchema {
		return p.provisionSchema(ctx, request, database, role, handoff)
	}

	admin, err := p.connect(ctx, request.Admin.Database)
	if err != nil {
		return Result{}, err
	}
	if err := acquireProjectLock(ctx, admin, request.ProjectID); err != nil {
		closeBestEffort(ctx, admin)
		return Result{}, err
	}

	password := request.Password
	status := "created with existing password"
	if password == "" {
		roleExisted, existsErr := projectRoleExists(ctx, admin, role)
		if existsErr != nil {
			closeBestEffort(ctx, admin)
			return Result{}, existsErr
		}
		if roleExisted {
			closeBestEffort(ctx, admin)
			return Result{}, &PasswordRequiredError{}
		}
		password, err = generatePassword(p.random)
		if err != nil {
			closeBestEffort(ctx, admin)
			return Result{}, fmt.Errorf("%w: generate project password", ErrProvisioning)
		}
		if err := exec(ctx, admin, "create project role", createRoleSQL(role, password)); err != nil {
			closeBestEffort(ctx, admin)
			return Result{}, err
		}
		status = "created with new password"
	} else {
		roleExisted, ensureErr := ensureRole(ctx, admin, role, password)
		if ensureErr != nil {
			closeBestEffort(ctx, admin)
			return Result{}, ensureErr
		}
		if roleExisted {
			status = "password reset"
		}
	}
	if err := ensureDatabase(ctx, admin, database); err != nil {
		closeBestEffort(ctx, admin)
		return Result{}, err
	}
	if err := grantDatabase(ctx, admin, database, role); err != nil {
		closeBestEffort(ctx, admin)
		return Result{}, err
	}
	if request.UseExistingAdminUser {
		if err := grantDatabase(ctx, admin, database, request.Admin.User); err != nil {
			closeBestEffort(ctx, admin)
			return Result{}, err
		}
	}
	project, err := p.connect(ctx, database)
	if err != nil {
		closeBestEffort(ctx, admin)
		return Result{}, err
	}
	if err := grantPublicSchema(ctx, project, role); err != nil {
		closeBestEffort(ctx, project)
		closeBestEffort(ctx, admin)
		return Result{}, err
	}
	if request.UseExistingAdminUser {
		if err := grantPublicSchema(ctx, project, request.Admin.User); err != nil {
			closeBestEffort(ctx, project)
			closeBestEffort(ctx, admin)
			return Result{}, err
		}
	}
	if err := exec(ctx, project, "create vector extension", createVectorExtensionSQL); err != nil {
		closeBestEffort(ctx, project)
		closeBestEffort(ctx, admin)
		return Result{}, err
	}
	if err := closeOwned(ctx, project); err != nil {
		closeBestEffort(ctx, admin)
		return Result{}, err
	}

	connectionUser := role
	connectionPassword := password
	if request.UseExistingAdminUser {
		connectionUser = request.Admin.User
		connectionPassword = request.Admin.Password
	}

	result := Result{
		Status:           status,
		Password:         password,
		ConnectionString: sqlalchemyURL(request.Admin, connectionUser, connectionPassword, database, ""),
		ProjectDatabase:  database,
		ProjectRole:      role,
		User:             connectionUser,
	}
	if handoff != nil {
		if err := handoff(ctx, result); err != nil {
			closeBestEffort(ctx, admin)
			return Result{}, handoffError(ctx, err)
		}
	}
	if err := ctx.Err(); err != nil {
		closeBestEffort(ctx, admin)
		return Result{}, err
	}
	// Closing the administrator session is the lock release. Keep it last so a
	// competing process cannot mutate the role before this result is complete.
	if err := closeOwned(ctx, admin); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (p *Provisioner) provisionSchema(
	ctx context.Context,
	request Request,
	schema string,
	role string,
	handoff Handoff,
) (Result, error) {
	admin, err := p.connect(ctx, request.Admin.Database)
	if err != nil {
		return Result{}, err
	}
	if err := acquireProjectLock(ctx, admin, request.ProjectID); err != nil {
		closeBestEffort(ctx, admin)
		return Result{}, err
	}
	if err := ensureSchema(ctx, admin, schema); err != nil {
		closeBestEffort(ctx, admin)
		return Result{}, err
	}

	// The current schema compatibility mode relies on the source database's
	// bootstrap vector extension and intentionally creates no role or grants.
	result := Result{
		Status:           "created",
		Password:         request.Admin.Password,
		ConnectionString: sqlalchemyURL(request.Admin, request.Admin.User, request.Admin.Password, request.Admin.Database, schema),
		ProjectDatabase:  schema,
		ProjectRole:      role,
		User:             request.Admin.User,
		Schema:           schema,
	}
	if handoff != nil {
		if err := handoff(ctx, result); err != nil {
			closeBestEffort(ctx, admin)
			return Result{}, handoffError(ctx, err)
		}
	}
	if err := ctx.Err(); err != nil {
		closeBestEffort(ctx, admin)
		return Result{}, err
	}
	if err := closeOwned(ctx, admin); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (p *Provisioner) connect(ctx context.Context, database string) (Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	connection, err := p.connector.Connect(ctx, database)
	if err != nil {
		return nil, operationError(ctx, "connect", err)
	}
	if connection == nil {
		return nil, fmt.Errorf("%w: connect", ErrProvisioning)
	}
	return connection, nil
}

func validateRequest(request Request) (database string, role string, err error) {
	// The current centry.project primary key is PostgreSQL int4. Using that
	// proven bound gives the advisory lock a collision-free namespace/project
	// key pair instead of hashing tenant identity into a potentially colliding
	// single bigint key.
	if request.ProjectID <= 0 || request.ProjectID > math.MaxInt32 ||
		(request.Mode != ModeDatabaseRole && request.Mode != ModeSchema) {
		return "", "", ErrInvalidRequest
	}
	if !validPostgresName(request.Admin.User) ||
		!validPostgresName(request.Admin.Database) ||
		!validHost(request.Admin.Host) || request.Admin.Port == 0 ||
		!validPassword(request.Admin.Password) ||
		!validProjectConnectionParameters(request.Admin.ProjectConnectionParams) ||
		(request.Mode == ModeDatabaseRole && !validPassword(request.Password)) {
		return "", "", ErrInvalidRequest
	}

	database = ProjectDatabaseName(request.ProjectID)
	role = ProjectRoleName(request.ProjectID)
	if !validPostgresName(database) || !validPostgresName(role) {
		return "", "", ErrInvalidRequest
	}
	return database, role, nil
}

func validPostgresName(value string) bool {
	if value == "" || len(value) > maxPostgresNameBytes || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validHost(value string) bool {
	if value == "" || len(value) > maxHostBytes || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character == '\x00' || character == '\r' || character == '\n' {
			return false
		}
	}
	return true
}

func validPassword(value string) bool {
	return len(value) <= maxPasswordBytes && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

// ProjectConnectionParametersFromURL returns a canonical, bounded subset of
// PostgreSQL connection parameters that must remain on the project-owned DSN.
// Unknown parameters are intentionally not copied into worker-visible material.
func ProjectConnectionParametersFromURL(connectionString string) (string, error) {
	if connectionString == "" || len(connectionString) > maxConnectionOptions*4 ||
		strings.ContainsAny(connectionString, "\x00\r\n") {
		return "", ErrInvalidRequest
	}
	parsed, err := url.Parse(connectionString)
	if err != nil {
		return "", ErrInvalidRequest
	}
	if parsed.Scheme == "" {
		// pgx also accepts keyword/value DSNs. Their original TLS file paths cannot
		// be reconstructed safely from pgx.ConnConfig, so production resolution
		// must supply a URL when these parameters need propagation.
		return "", nil
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return "", ErrInvalidRequest
	}

	source, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", ErrInvalidRequest
	}
	filtered := make(url.Values)
	for name, values := range source {
		if !allowedProjectConnectionParameter(name) {
			continue
		}
		if len(values) != 1 || !validProjectConnectionParameter(name, values[0]) {
			return "", ErrInvalidRequest
		}
		filtered.Set(name, values[0])
	}
	encoded := filtered.Encode()
	if len(encoded) > maxConnectionOptions {
		return "", ErrInvalidRequest
	}
	return encoded, nil
}

func validProjectConnectionParameters(encoded string) bool {
	if encoded == "" {
		return true
	}
	if len(encoded) > maxConnectionOptions || strings.ContainsAny(encoded, "\x00\r\n") {
		return false
	}
	values, err := url.ParseQuery(encoded)
	if err != nil || len(values) == 0 {
		return false
	}
	for name, entries := range values {
		if !allowedProjectConnectionParameter(name) || len(entries) != 1 ||
			!validProjectConnectionParameter(name, entries[0]) {
			return false
		}
	}
	return values.Encode() == encoded
}

func allowedProjectConnectionParameter(name string) bool {
	switch name {
	case "channel_binding", "connect_timeout", "load_balance_hosts", "sslcert",
		"sslcrl", "sslkey", "sslmode", "sslnegotiation", "sslrootcert",
		"target_session_attrs":
		return true
	default:
		return false
	}
}

func validProjectConnectionParameter(name string, value string) bool {
	switch name {
	case "sslmode":
		return oneOf(value, "disable", "allow", "prefer", "require", "verify-ca", "verify-full")
	case "channel_binding":
		return oneOf(value, "disable", "prefer", "require")
	case "sslnegotiation":
		return oneOf(value, "postgres", "direct")
	case "target_session_attrs":
		return oneOf(value, "any", "read-write", "read-only", "primary", "standby", "prefer-standby")
	case "load_balance_hosts":
		return oneOf(value, "disable", "random")
	case "connect_timeout":
		seconds, err := strconv.Atoi(value)
		return err == nil && seconds >= 1 && seconds <= 300
	case "sslrootcert", "sslcert", "sslkey", "sslcrl":
		return value != "" && len(value) <= maxConnectionOptions && utf8.ValidString(value) &&
			!strings.ContainsAny(value, "\x00\r\n")
	default:
		return false
	}
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func generatePassword(reader io.Reader) (string, error) {
	// Rejection sampling avoids modulo bias. Eight fixed-size reads bound work;
	// exhausting them has negligible probability with crypto/rand.Reader.
	var password [passwordLength]byte
	var randomBytes [passwordLength]byte
	written := 0
	for attempt := 0; attempt < 8 && written < len(password); attempt++ {
		if _, err := io.ReadFull(reader, randomBytes[:]); err != nil {
			return "", err
		}
		for _, randomByte := range randomBytes {
			const unbiasedLimit = 256 - (256 % len(passwordAlphabet))
			if int(randomByte) >= unbiasedLimit {
				continue
			}
			password[written] = passwordAlphabet[int(randomByte)%len(passwordAlphabet)]
			written++
			if written == len(password) {
				break
			}
		}
	}
	if written != len(password) {
		return "", io.ErrNoProgress
	}
	return string(password[:]), nil
}

func sqlalchemyURL(admin AdminConnection, user string, password string, database string, schema string) string {
	path := "/" + database
	parsed := url.URL{
		Scheme:  "postgresql+psycopg",
		User:    url.UserPassword(user, password),
		Host:    net.JoinHostPort(admin.Host, strconv.FormatUint(uint64(admin.Port), 10)),
		Path:    path,
		RawPath: "/" + url.PathEscape(database),
	}
	parameters, _ := url.ParseQuery(admin.ProjectConnectionParams)
	legacySchemaOption := ""
	if schema != "" {
		// Preserve the current SQLAlchemy search_path query shape. Project schema
		// names are derived exclusively from a validated positive integer.
		legacySchemaOption = "-csearch_path=" + schema + ",public"
		parameters.Set("options", legacySchemaOption)
	}
	parsed.RawQuery = parameters.Encode()
	if legacySchemaOption != "" {
		// The current Python contract leaves the search_path comma literal. Keep
		// that byte shape while url.Values continues to encode every other value.
		encodedOption := url.QueryEscape(legacySchemaOption)
		parsed.RawQuery = strings.Replace(
			parsed.RawQuery,
			"options="+encodedOption,
			"options="+strings.ReplaceAll(encodedOption, "%2C", ","),
			1,
		)
	}
	return parsed.String()
}

func closeOwned(ctx context.Context, connection Connection) error {
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()
	if err := connection.Close(cleanupContext); err != nil {
		return operationError(cleanupContext, "close connection", err)
	}
	return nil
}

func closeBestEffort(ctx context.Context, connection Connection) {
	_ = closeOwned(ctx, connection)
}

func operationError(ctx context.Context, operation string, err error) error {
	if ctx != nil {
		if contextError := ctx.Err(); contextError != nil {
			return contextError
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return fmt.Errorf("%w: %s", ErrProvisioning, operation)
}

func handoffError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextError := ctx.Err(); contextError != nil {
			return contextError
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return err
}
