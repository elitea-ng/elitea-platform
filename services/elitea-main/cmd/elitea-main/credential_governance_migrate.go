package main

// `elitea-main credential-governance migrate-references` (ADR-0039, #1124):
// find the existing toolkit configurations a governed agent would need and
// propose, or stage, a CredentialReference and a TargetBinding for each.
//
// Usage:
//
//	elitea-main credential-governance migrate-references [--project N] [--json]          # dry run (default)
//	elitea-main credential-governance migrate-references --apply --operator USER_ID [--project N] [--json]
//	elitea-main credential-governance migrate-references --apply --operator USER_ID --copy-to vault [--policy-revision ID]
//
// It reads DATABASE_URL (and, for --copy-to vault, SECRETS_MASTER_KEY and the
// ELITEA_SECRETSTORE_VAULT_* variables) from the environment, as the service
// does. It runs whether or not governance is on for the running service.
//
// THE RULE. A unit is one toolkit row of a tenant project (p_N.elitea_tools):
//
//   - jira, confluence, qtest, aha: the toolkit holds exactly one non-private
//     configuration reference ({"elitea_title", "private": false}) naming a
//     configuration of the same type in the SAME project. The target is the
//     configuration's base_url, canonicalised by the provider adapter. If the
//     provider's secret field (jira/confluence: token, else api_key; qtest:
//     qtest_api_token; aha: api_key) holds a sealed {{secret.NAME}}, the
//     class is static_secret in pgvault at locator NAME, version 1 (pgvault
//     secrets are single-version). With no secret, the class is
//     oauth_delegated when the provider accepts it (the consent flow supplies
//     the material). A plaintext secret is skipped (secret_not_sealed).
//   - jira, confluence basic auth (#1481): with no token, a sealed api_key
//     and a non-empty username (plain or sealed) are the account's API token
//     and email. The reference then points at ONE composite secret holding
//     the key map {"token", "username"}, written by --apply: in pgvault at
//     governed_<configuration uuid hex>, or at the --copy-to vault path. A
//     token (a PAT, Bearer) wins over api_key, as it does for a legacy run,
//     and stays a token-only reference to the existing secret.
//   - sharepoint: the same configuration rule; the target is site_url and the
//     class is always oauth_delegated (Entra on-behalf-of). Its client secret
//     is never referenced.
//   - mcp: no configuration reference. The target is the canonical resource
//     URI of settings.url, oauth_delegated. A toolkit with static headers or a
//     URL with no canonical form is skipped (toolkit_not_governable).
//
// Toolkits that name one configuration share one reference. The idempotency
// key is stored as migrated_from (unique per project among references that
// are not retired): "configuration:<uuid>:<backend>" or "mcp:<toolkit id>".
// A second --apply finds the first's rows; an --apply after a retire stages
// a replacement. A toolkit whose settings are not a JSON object is reported
// as skipped (settings_unreadable), never left out.
//
// --operator is an auth_core user id that must exist; it is recorded in its
// canonical decimal form, so "007" and "7" are one person to the two-person
// rule.
//
// WHAT IT NEVER DOES. A dry run writes nothing: it builds no secret writer
// or ledger, and settle returns before any store write. --apply writes `staged` references and their target
// bindings only, with a security audit event each. It never activates,
// grants, attaches or edits a toolkit: those are the admin API's, by people.
// No secret value moves unless --copy-to vault is given or the reference is
// a composite basic-auth key map; then the value is written with
// check-and-set version 0 through the staged-write ledger (#1113) to
// Vault/OpenBao KV v2 or to a NEW pgvault name, and the pgvault secrets the
// configuration names stay, because legacy runs still read them. The
// reference is staged first at generation 0 and takes the written version
// once the write lands.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore/backends"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore/pgvault"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const (
	credentialGovernanceCommand   = "credential-governance"
	migrateReferencesSubcommand   = "migrate-references"
	migrateReferencesRunTimeout   = 30 * time.Minute
	migrateReferencesCopyScope    = repos.CredentialReferenceStagingScope
	migrateReferencesOAuthLocator = repos.CredentialReferenceOAuthLocator
)

// migrateAction is what happened to one unit.
type migrateAction string

const (
	migrateProposed migrateAction = "propose"
	migrateStaged   migrateAction = "staged"
	migrateExists   migrateAction = "exists"
	migrateSkipped  migrateAction = "skipped"
)

// migrateItem is one output line (--json: one object per line).
type migrateItem struct {
	Action                string `json:"action"`
	Reason                string `json:"reason,omitempty"`
	ProjectID             int64  `json:"project_id"`
	ToolkitID             int64  `json:"toolkit_id"`
	ToolkitType           string `json:"toolkit_type"`
	ConfigurationUUID     string `json:"configuration_uuid"`
	ConfigurationTitle    string `json:"configuration_title"`
	ProviderType          string `json:"provider_type"`
	Class                 string `json:"class"`
	TargetIdentity        string `json:"target_identity"`
	TargetBindingID       string `json:"target_binding_id"`
	CredentialReferenceID string `json:"credential_reference_id"`
	Backend               string `json:"backend"`
	Locator               string `json:"locator"`
	Generation            int64  `json:"generation"`
	SecretLocation        string `json:"secret_location"`
	State                 string `json:"state"`
	// Material is the key set the secret holds for a static secret:
	// "token", or "token+username" for a composite basic-auth key map.
	Material     string `json:"material,omitempty"`
	action       migrateAction
	migratedFrom string
	secretName   string
	// username is the basic-auth account of a composite reference: plain
	// text, or usernameSecret names the sealed pgvault secret holding it.
	username       string
	usernameSecret string
}

// keyMap reports a composite basic-auth reference.
func (item migrateItem) keyMap() bool {
	return item.username != "" || item.usernameSecret != ""
}

// migrateProvider is the configuration rule of one toolkit type.
type migrateProvider struct {
	targetField  string
	secretFields []string
	oauthOnly    bool
	// basicSecretField with a non-empty usernameField is basic auth: the
	// secret is the account's API token and the username its account
	// (#1481). It is used only when no earlier secret field is set.
	basicSecretField string
	usernameField    string
}

var migrateProviders = map[string]migrateProvider{
	"jira": {targetField: "base_url", secretFields: []string{"token", "api_key"},
		basicSecretField: "api_key", usernameField: "username"},
	"confluence": {targetField: "base_url", secretFields: []string{"token", "api_key"},
		basicSecretField: "api_key", usernameField: "username"},
	"qtest":      {targetField: "base_url", secretFields: []string{"qtest_api_token"}},
	"aha":        {targetField: "base_url", secretFields: []string{"api_key"}},
	"sharepoint": {targetField: "site_url", oauthOnly: true},
}

// migrateSecrets is what --copy-to vault, and a composite key map, read
// from and write to. vault is nil when --copy-to vault is not given.
type migrateSecrets struct {
	source secretstore.SecretStore // pgvault
	vault  secretstore.SecretStore
	mount  string
}

// migrateDeps are the seams a test replaces. secrets builds the vault
// destination only when needVault is set.
type migrateDeps struct {
	connect func(ctx context.Context, dsn string) (*pgxpool.Pool, error)
	secrets func(lookup func(string) (string, bool), pool *pgxpool.Pool, needVault bool) (migrateSecrets, error)
	newID   func() string
}

func defaultMigrateDeps() migrateDeps {
	return migrateDeps{connect: connectMigratePool, secrets: migrateSecretsFromEnv, newID: uuid.NewString}
}

// connectMigratePool opens the pool. It sets no session parameter: the
// DATABASE_URL usually names PgBouncer in transaction pooling mode, which
// refuses a startup parameter such as default_transaction_read_only, and a
// session-level SET there would leak to other clients. A dry run stays
// read-only by construction (see settle).
func connectMigratePool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 4
	return pgxpool.NewWithConfig(ctx, cfg)
}

// migrateSecretsFromEnv builds the pgvault source and the Vault/OpenBao
// destination the same way the credential broker does. Without needVault
// it builds the pgvault store alone (a composite key map in pgvault).
func migrateSecretsFromEnv(lookup func(string) (string, bool), pool *pgxpool.Pool, needVault bool) (migrateSecrets, error) {
	masterKey, err := encodedVaultMasterKeyFromEnv(lookup)
	if err != nil {
		return migrateSecrets{}, err
	}
	if masterKey == nil {
		// Main's start-up rule: no key only with the development opt-out,
		// whose project vault keys are stored unwrapped.
		getenv := func(name string) string { value, _ := lookup(name); return value }
		if _, err := requireVaultMasterKey(getenv); err != nil {
			return migrateSecrets{}, fmt.Errorf("reading the pgvault secret: %w", err)
		}
	}
	reader, err := storage.NewPostgresSecretVaultLoader(pool, masterKey)
	if err != nil {
		return migrateSecrets{}, err
	}
	writer, err := repos.NewCurrentSecretVaultRepository(pool, masterKey)
	if err != nil {
		return migrateSecrets{}, err
	}
	source, err := pgvault.New(pool, reader, writer)
	if err != nil {
		return migrateSecrets{}, err
	}
	if !needVault {
		return migrateSecrets{source: source}, nil
	}
	external, _, err := backends.VaultFromEnv(lookup)
	if err != nil {
		return migrateSecrets{}, err
	}
	if external == nil {
		return migrateSecrets{}, errors.New("--copy-to vault needs ELITEA_SECRETSTORE_VAULT_* configured")
	}
	return migrateSecrets{source: source, vault: external, mount: external.KVMount()}, nil
}

// runCredentialGovernance dispatches `elitea-main credential-governance`.
func runCredentialGovernance(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != migrateReferencesSubcommand {
		_, _ = fmt.Fprintln(stderr, "usage: elitea-main credential-governance migrate-references [--apply --operator USER_ID] [--project N] [--copy-to vault] [--json]")
		return 2
	}
	return runMigrateReferences(ctx, args[1:], lookup, stdout, stderr, defaultMigrateDeps())
}

type migrateOptions struct {
	apply          bool
	projectID      int64
	operator       string
	copyToVault    bool
	policyRevision string
	json           bool
}

func runMigrateReferences(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer, deps migrateDeps) int {
	flags := flag.NewFlagSet(credentialGovernanceCommand+" "+migrateReferencesSubcommand, flag.ContinueOnError)
	flags.SetOutput(stderr)
	dryRun := flags.Bool("dry-run", false, "list the proposed references and write nothing (the default)")
	apply := flags.Bool("apply", false, "stage the references (state staged only)")
	project := flags.Int64("project", 0, "one tenant project id (default: every project)")
	operator := flags.String("operator", "", "with --apply: the auth_core user id recorded as staged_by")
	copyTo := flags.String("copy-to", "", "\"vault\": copy each static secret from pgvault to Vault/OpenBao (CAS, ledger)")
	revision := flags.String("policy-revision", "", "with --copy-to vault: the policy revision the secret store calls name (default: the configured revision)")
	asJSON := flags.Bool("json", false, "print one JSON object per toolkit per line")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	opts := migrateOptions{apply: *apply, projectID: *project, operator: strings.TrimSpace(*operator),
		copyToVault: *copyTo == "vault", policyRevision: strings.TrimSpace(*revision), json: *asJSON}
	switch {
	case *dryRun && *apply:
		_, _ = fmt.Fprintln(stderr, "--dry-run and --apply exclude each other")
		return 2
	case *copyTo != "" && *copyTo != "vault":
		_, _ = fmt.Fprintln(stderr, "--copy-to takes only \"vault\"")
		return 2
	case *project < 0:
		_, _ = fmt.Fprintln(stderr, "--project must be a project id")
		return 2
	case !opts.apply && opts.operator != "":
		_, _ = fmt.Fprintln(stderr, "--operator only applies together with --apply")
		return 2
	}
	if opts.apply {
		// The two-person rule compares ids: "007" is user 7, so it is
		// rendered canonically before it is recorded or compared.
		_, canonical, err := repos.CanonicalUserID(opts.operator)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "--apply needs --operator: the numeric auth_core user id of the operator who stages")
			return 2
		}
		opts.operator = canonical
	}
	if opts.apply && opts.policyRevision == "" {
		// A composite key map is written under the configured revision too;
		// without one, only that write stops (writeMaterial).
		opts.policyRevision, _ = lookup(profile.EnvPolicyRevision)
		opts.policyRevision = strings.TrimSpace(opts.policyRevision)
		if opts.copyToVault && opts.policyRevision == "" {
			_, _ = fmt.Fprintf(stderr, "--copy-to vault needs --policy-revision or %s\n", profile.EnvPolicyRevision)
			return 2
		}
	}
	dsn, _ := lookup("DATABASE_URL")
	if strings.TrimSpace(dsn) == "" {
		_, _ = fmt.Fprintln(stderr, "DATABASE_URL is required")
		return 2
	}

	ctx, cancel := context.WithTimeout(ctx, migrateReferencesRunTimeout)
	defer cancel()
	pool, err := deps.connect(ctx, dsn)
	if err != nil {
		// pgx's error can quote the DSN; say what failed, not with what.
		_, _ = fmt.Fprintln(stderr, "connect to the platform database (DATABASE_URL) failed")
		return 1
	}
	defer pool.Close()
	if opts.apply {
		known, err := operatorExists(ctx, pool, opts.operator)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "migrate-references stopped: read the operator:", err)
			return 1
		}
		if !known {
			_, _ = fmt.Fprintf(stderr, "--operator %s is not an auth_core user id\n", opts.operator)
			return 2
		}
	}
	var secrets migrateSecrets
	if opts.apply && opts.copyToVault {
		if secrets, err = deps.secrets(lookup, pool, true); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
	}
	registry, err := credentialadapters.NewBuiltinRegistry()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	audit, err := securityaudit.NewPostgresAppender("elitea-main/migrate-references")
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	m := &referenceMigration{pool: pool, store: repos.NewCredentialReferences(pool, audit), registry: registry,
		opts: opts, secrets: secrets, newID: deps.newID}
	if opts.apply {
		if m.ledger, err = secretstore.NewLedger(pool); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if opts.apply && !opts.copyToVault {
		// The pgvault store is opened only when a composite key map is
		// written, so an --apply with none needs no master key.
		m.openSecrets = func() (migrateSecrets, error) { return deps.secrets(lookup, pool, false) }
	}
	items, err := m.run(ctx)
	printMigrateItems(stdout, items, opts)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "migrate-references stopped:", err)
		return 1
	}
	return 0
}

// operatorExists reports whether the canonical user id names an
// auth_core user.
func operatorExists(ctx context.Context, pool *pgxpool.Pool, operator string) (bool, error) {
	id, _, err := repos.CanonicalUserID(operator)
	if err != nil {
		return false, nil
	}
	var found bool
	err = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public.auth_core__user WHERE id = $1)`, id).Scan(&found)
	return found, err
}

type referenceMigration struct {
	pool     *pgxpool.Pool
	store    *repos.CredentialReferences
	registry *credentialadapters.Registry
	opts     migrateOptions
	secrets  migrateSecrets
	// openSecrets opens secrets on first need (an --apply without
	// --copy-to vault that writes a composite key map to pgvault).
	openSecrets func() (migrateSecrets, error)
	ledger      *secretstore.Ledger
	newID       func() string
	// migrated memoises FindMigrated per project and source for one run:
	// toolkits sharing a configuration read it once.
	migrated map[string]repos.CredentialReferenceRecord
}

func (m *referenceMigration) projects(ctx context.Context) ([]int64, error) {
	if m.opts.projectID > 0 {
		return []int64{m.opts.projectID}, nil
	}
	rows, err := m.pool.Query(ctx, `SELECT id::bigint FROM centry.project ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

func (m *referenceMigration) run(ctx context.Context) ([]migrateItem, error) {
	projects, err := m.projects(ctx)
	if err != nil {
		return nil, err
	}
	var items []migrateItem
	for _, project := range projects {
		planned, err := m.plan(ctx, project)
		if err != nil {
			return items, fmt.Errorf("project %d: %w", project, err)
		}
		for _, item := range planned {
			if item.action != migrateSkipped {
				if err := m.settle(ctx, &item); err != nil {
					return items, fmt.Errorf("project %d toolkit %d: %w", project, item.ToolkitID, err)
				}
			}
			item.Action = string(item.action)
			items = append(items, item)
		}
	}
	return items, nil
}

func (m *referenceMigration) tableExists(ctx context.Context, project int64, table string) (bool, error) {
	name, err := tenantschema.Name(strconv.FormatInt(project, 10))
	if err != nil {
		return false, nil
	}
	var found bool
	err = m.pool.QueryRow(ctx, `SELECT to_regclass(quote_ident($1) || '.' || $2) IS NOT NULL`, name, table).Scan(&found)
	return found, err
}

type migrateToolkit struct {
	id          int64
	toolkitType string
	settings    map[string]any
	// unreadable is a settings value that does not decode to an object.
	unreadable bool
}

// migrateConfiguration is one row of p_N.configuration.
type migrateConfiguration struct {
	uuid, configurationType, data string
}

// plan reads one project's toolkits and applies the rule. It writes nothing.
func (m *referenceMigration) plan(ctx context.Context, project int64) ([]migrateItem, error) {
	schema, err := tenantschema.QuoteInt(project)
	if err != nil {
		return nil, err
	}
	if ok, err := m.tableExists(ctx, project, "elitea_tools"); err != nil || !ok {
		return nil, err
	}
	hasConfigurations, err := m.tableExists(ctx, project, "configuration")
	if err != nil {
		return nil, err
	}
	rows, err := m.pool.Query(ctx, fmt.Sprintf(`SELECT id::bigint, type, settings::text FROM %s.elitea_tools ORDER BY id`, schema))
	if err != nil {
		return nil, fmt.Errorf("read toolkits: %w", err)
	}
	var toolkits []migrateToolkit
	for rows.Next() {
		var (
			t   migrateToolkit
			raw string
		)
		if err := rows.Scan(&t.id, &t.toolkitType, &raw); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read toolkits: %w", err)
		}
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&t.settings) != nil {
			t.settings, t.unreadable = nil, true
		}
		toolkits = append(toolkits, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read toolkits: %w", err)
	}
	var configurations map[string]migrateConfiguration
	if hasConfigurations {
		if configurations, err = m.configurations(ctx, project, schema); err != nil {
			return nil, err
		}
	}
	var items []migrateItem
	for _, t := range toolkits {
		item, include, err := m.planToolkit(project, configurations, t)
		if err != nil {
			return nil, err
		}
		if include {
			items = append(items, item)
		}
	}
	return items, nil
}

// configurations reads one project's configurations once, keyed by
// elitea_title (unique per project).
func (m *referenceMigration) configurations(ctx context.Context, project int64, schema string) (map[string]migrateConfiguration, error) {
	rows, err := m.pool.Query(ctx, fmt.Sprintf(
		`SELECT elitea_title, uuid::text, type, data::text FROM %s.configuration WHERE project_id = $1`, schema), project)
	if err != nil {
		return nil, fmt.Errorf("read configurations: %w", err)
	}
	defer rows.Close()
	out := map[string]migrateConfiguration{}
	for rows.Next() {
		var (
			title string
			c     migrateConfiguration
		)
		if err := rows.Scan(&title, &c.uuid, &c.configurationType, &c.data); err != nil {
			return nil, fmt.Errorf("read configurations: %w", err)
		}
		out[title] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read configurations: %w", err)
	}
	return out, nil
}

func skip(item migrateItem, reason string) (migrateItem, bool, error) {
	item.action, item.Reason = migrateSkipped, reason
	return item, true, nil
}

// planToolkit applies the rule to one toolkit. include is false for a
// toolkit that holds no credential the rule knows about. A nil
// configurations map is a project with no configuration table.
func (m *referenceMigration) planToolkit(project int64, configurations map[string]migrateConfiguration, t migrateToolkit) (migrateItem, bool, error) {
	item := migrateItem{ProjectID: project, ToolkitID: t.id, ToolkitType: t.toolkitType, ProviderType: t.toolkitType,
		Backend: secretstore.BackendPGVault}
	if t.unreadable {
		// Never silently absent: the operator must see the toolkit the run
		// could not judge.
		return skip(item, "settings_unreadable")
	}
	if t.settings == nil {
		return item, false, nil
	}
	if t.toolkitType == "mcp" {
		return m.planMCP(item, t)
	}
	refs := configurationapp.ToolkitCredentialRefs(t.settings)
	provider, known := migrateProviders[t.toolkitType]
	switch {
	case len(refs) == 0:
		return item, false, nil
	case !known:
		return skip(item, "provider_not_governed")
	case len(refs) > 1:
		return skip(item, "credential_reference_ambiguous")
	}
	ref := refs[0]
	item.ConfigurationTitle = ref.Title
	if ref.Private {
		return skip(item, "private_credential_not_governed")
	}
	configuration, found := configurations[ref.Title]
	if !found || ref.Title == "" {
		return skip(item, "configuration_not_in_project")
	}
	item.ConfigurationUUID = configuration.uuid
	if configuration.configurationType != t.toolkitType {
		return skip(item, "configuration_type_mismatch")
	}
	var data map[string]any
	if json.Unmarshal([]byte(configuration.data), &data) != nil {
		return skip(item, "configuration_unreadable")
	}
	descriptor, ok := m.registry.Descriptor(t.toolkitType)
	if !ok {
		return skip(item, "provider_not_governed")
	}
	rawTarget, _ := data[provider.targetField].(string)
	var err error
	if item.TargetIdentity, err = descriptor.CanonicalizeTarget(rawTarget); err != nil {
		return skip(item, "target_not_canonical")
	}
	class := credentialgrant.ClassOAuthDelegated
	if !provider.oauthOnly {
		for _, field := range provider.secretFields {
			value, _ := data[field].(string)
			if value == "" {
				continue
			}
			name, sealed := sealedSecretName(value)
			if !sealed {
				return skip(item, "secret_not_sealed")
			}
			class, item.secretName = credentialgrant.ClassStaticSecret, name
			if field == provider.basicSecretField && provider.usernameField != "" {
				// Basic auth (#1481): the account travels with the token.
				username, _ := data[provider.usernameField].(string)
				if username = strings.TrimSpace(username); username != "" {
					if sealedName, sealed := sealedSecretName(username); sealed {
						item.usernameSecret = sealedName
					} else if strings.Contains(username, "{{secret.") {
						return skip(item, "secret_not_sealed")
					} else {
						item.username = username
					}
				}
			}
			break
		}
	}
	if !descriptor.Accepts(class) {
		return skip(item, "class_not_supported")
	}
	item.Class = string(class)
	if class == credentialgrant.ClassStaticSecret {
		item.Material = "token"
		switch {
		case m.opts.copyToVault:
			item.Backend = secretstore.BackendVault
			item.Locator = migrateVaultPath(project, item.ConfigurationUUID)
		case item.keyMap():
			// A new pgvault name holds the key map; it is written at
			// --apply, so the reference starts at generation 0.
			item.Locator = migrateKeyMapName(item.ConfigurationUUID)
		default:
			item.Locator, item.Generation = item.secretName, 1
		}
		if item.keyMap() {
			item.Material = "token+username"
		}
	} else {
		item.Locator = migrateReferencesOAuthLocator
	}
	item.migratedFrom = "configuration:" + item.ConfigurationUUID + ":" + item.Backend
	item.action = migrateProposed
	return item, true, nil
}

func (m *referenceMigration) planMCP(item migrateItem, t migrateToolkit) (migrateItem, bool, error) {
	descriptor, ok := m.registry.Descriptor("mcp")
	if !ok {
		return skip(item, "provider_not_governed")
	}
	if headers, ok := t.settings["headers"].(map[string]any); ok && len(headers) > 0 {
		return skip(item, "toolkit_not_governable")
	}
	rawURL, _ := t.settings["url"].(string)
	target, err := descriptor.CanonicalizeTarget(rawURL)
	if err != nil {
		return skip(item, "toolkit_not_governable")
	}
	item.TargetIdentity, item.Class, item.Locator = target, string(credentialgrant.ClassOAuthDelegated), migrateReferencesOAuthLocator
	item.migratedFrom = "mcp:" + strconv.FormatInt(t.id, 10)
	item.action = migrateProposed
	return item, true, nil
}

// sealedSecretName reads NAME from a whole `{{secret.NAME}}` value.
func sealedSecretName(value string) (string, bool) {
	if !configurationapp.IsCurrentSecretReference(value) {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(value, "{{secret."), "}}")
	return name, name != ""
}

func migrateVaultPath(project int64, configurationUUID string) string {
	return "elitea/projects/" + strconv.FormatInt(project, 10) + "/credentials/" + configurationUUID
}

// migrateKeyMapName is the pgvault name of a configuration's composite
// basic-auth secret: fixed per configuration (a rerun finds what it wrote)
// and inside the vault's name rule.
func migrateKeyMapName(configurationUUID string) string {
	return "governed_" + strings.ReplaceAll(configurationUUID, "-", "")
}

// appendJSONString appends src as a JSON string. It copies bytes, never
// through a Go string, so the caller can clear every copy of the secret.
func appendJSONString(dst, src []byte) []byte {
	const hex = "0123456789abcdef"
	dst = append(dst, '"')
	for _, c := range src {
		switch {
		case c == '"' || c == '\\':
			dst = append(dst, '\\', c)
		case c < 0x20:
			dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
		default:
			dst = append(dst, c)
		}
	}
	return append(dst, '"')
}

// keyMapValue is the composite secret {"token":…,"username":…}: the key map
// the credential broker shapes into the catalogue's static_secret keys.
func keyMapValue(token, username []byte) []byte {
	// The worst-case escape is six bytes per byte: sized once, the buffer
	// never reallocates and leaves no uncleared copy behind.
	out := make([]byte, 0, 6*(len(token)+len(username))+32)
	out = append(out, `{"token":`...)
	out = appendJSONString(out, token)
	out = append(out, `,"username":`...)
	out = appendJSONString(out, username)
	return append(out, '}')
}

// findMigrated is FindMigrated, memoised for the run. A miss is remembered
// as the zero record.
func (m *referenceMigration) findMigrated(ctx context.Context, project int64, migratedFrom string) (repos.CredentialReferenceRecord, bool, error) {
	key := migratedKey(project, migratedFrom)
	if rec, ok := m.migrated[key]; ok {
		return rec, rec.ID != "", nil
	}
	rec, found, err := m.store.FindMigrated(ctx, project, migratedFrom)
	if err != nil {
		return repos.CredentialReferenceRecord{}, false, err
	}
	m.remember(project, migratedFrom, rec)
	return rec, found, nil
}

func (m *referenceMigration) remember(project int64, migratedFrom string, rec repos.CredentialReferenceRecord) {
	if m.migrated == nil {
		m.migrated = map[string]repos.CredentialReferenceRecord{}
	}
	m.migrated[migratedKey(project, migratedFrom)] = rec
}

func migratedKey(project int64, migratedFrom string) string {
	return strconv.FormatInt(project, 10) + "/" + migratedFrom
}

// copyPending is a static reference staged at generation 0 whose secret
// write has not completed (a crash between the staging and the write): a
// --copy-to vault copy, or a composite key map in pgvault. A token-only
// pgvault reference is staged at generation 1 and is never pending.
func copyPending(rec repos.CredentialReferenceRecord) bool {
	return (rec.Backend == secretstore.BackendVault || rec.Backend == secretstore.BackendPGVault) &&
		rec.Class == credentialgrant.ClassStaticSecret &&
		rec.Generation == 0 && rec.State == credentialgrant.ReferenceStaged
}

// settle looks the unit up and, with --apply, stages it.
//
// A reference whose secret this run writes (--copy-to vault, or a composite
// key map) is staged FIRST, at generation 0, so the audit decision that
// authorises the pgvault read, the ledger row and the write all name the id
// of a reference that exists. The write then records the version it wrote
// as the reference's generation (activation refuses a static secret at
// generation 0). A rerun that finds a reference still at generation 0
// completes its write under that same id.
func (m *referenceMigration) settle(ctx context.Context, item *migrateItem) error {
	existing, found, err := m.findMigrated(ctx, item.ProjectID, item.migratedFrom)
	if err != nil {
		return err
	}
	if found && (!m.opts.apply || !copyPending(existing)) {
		item.fill(existing)
		item.action = migrateExists
		return nil
	}
	// The dry run's last statement: everything below writes.
	if !m.opts.apply {
		item.State = string(credentialgrant.ReferenceStaged)
		item.TargetBindingID, err = m.store.FindTarget(ctx, item.ProjectID, item.ProviderType, item.TargetIdentity)
		item.SecretLocation = item.location(m.secrets.mount)
		return err
	}
	rec, created := existing, false
	if !found {
		staged := repos.StagedReference{
			TenantProjectID: item.ProjectID, ProviderType: item.ProviderType, TargetIdentity: item.TargetIdentity,
			Class: credentialgrant.CredentialClass(item.Class), Backend: item.Backend, Locator: item.Locator,
			Generation: item.Generation, MigratedFrom: item.migratedFrom, StagedBy: m.opts.operator,
		}
		if item.Backend == secretstore.BackendVault || item.keyMap() {
			staged.Generation = 0
		}
		result, err := m.store.StageReference(ctx, staged, m.newID)
		if err != nil {
			return err
		}
		rec, created = result.Record, result.Created
	}
	if copyPending(rec) {
		version, err := m.writeMaterial(ctx, *item, rec)
		if err != nil {
			return err
		}
		if rec, err = m.store.SetCopiedGeneration(ctx, rec.ID, version, m.opts.operator); err != nil {
			return err
		}
		created = true
	}
	m.remember(item.ProjectID, item.migratedFrom, rec)
	item.fill(rec)
	item.action = migrateStaged
	if !created {
		item.action = migrateExists
	}
	return nil
}

// writeMaterial writes the secret of the staged reference rec: the pgvault
// token copied to Vault/OpenBao (--copy-to vault), or, for basic auth, the
// composite key map {"token","username"} to Vault or to a new pgvault name
// (#1481). The audit decision that authorises the reads is appended first;
// its id is the reads' StoreContext.DecisionID. The write is check-and-set 0
// through the ledger. The destination is fixed per configuration, so a
// rerun after a failure finds the secret it wrote: on a check-and-set
// conflict the destination's CURRENT version is read, and it is used only
// when its bytes equal the value. Anything else stops the run.
func (m *referenceMigration) writeMaterial(ctx context.Context, item migrateItem, rec repos.CredentialReferenceRecord) (int64, error) {
	if m.secrets.source == nil && m.openSecrets != nil {
		secrets, err := m.openSecrets()
		if err != nil {
			return 0, err
		}
		m.secrets, m.openSecrets = secrets, nil
	}
	var store secretstore.SecretStore
	destination := secretstore.SecretRef{Backend: rec.Backend, Path: rec.Locator}
	switch rec.Backend {
	case secretstore.BackendVault:
		store, destination.Mount = m.secrets.vault, m.secrets.mount
	case secretstore.BackendPGVault:
		store = m.secrets.source
	}
	if m.secrets.source == nil || store == nil || m.ledger == nil {
		return 0, errors.New("migrate-references: the secret stores are not configured")
	}
	if m.opts.policyRevision == "" {
		return 0, fmt.Errorf("writing a secret needs --policy-revision or %s", profile.EnvPolicyRevision)
	}
	decisionID := m.newID()
	if err := m.store.RecordSecretCopy(ctx, item.ProjectID, rec.ID, decisionID, m.opts.operator, rec.Backend); err != nil {
		return 0, err
	}
	sc := secretstore.StoreContext{
		TenantProjectID: item.ProjectID, CredentialRefID: rec.ID, DecisionID: decisionID,
		WorkloadScope: migrateReferencesCopyScope, PolicyRevisionID: m.opts.policyRevision,
	}
	readPGVault := func(name string) (secretstore.SecretVersion, error) {
		secret, err := m.secrets.source.Read(ctx, sc, secretstore.SecretRef{Backend: secretstore.BackendPGVault, Path: name}, 1)
		if err != nil {
			return secretstore.SecretVersion{}, fmt.Errorf("read the pgvault secret: %w", err)
		}
		return secret, nil
	}
	token, err := readPGVault(item.secretName)
	if err != nil {
		return 0, err
	}
	defer token.Value.Destroy()
	value := token.Value
	if item.keyMap() {
		username := []byte(item.username)
		if item.usernameSecret != "" {
			sealed, err := readPGVault(item.usernameSecret)
			if err != nil {
				return 0, err
			}
			defer sealed.Value.Destroy()
			username = sealed.Value.UnsafeBytes()
		}
		raw := keyMapValue(token.Value.UnsafeBytes(), username)
		value = secretstore.NewRuntimeSecret(raw)
		clear(raw)
		defer value.Destroy()
	}
	version, err := m.ledger.StagedWrite(ctx, store, sc, destination, value, 0)
	if errors.Is(err, secretstore.ErrVersionConflict) {
		meta, metaErr := store.Metadata(ctx, sc, destination)
		if metaErr != nil {
			return 0, fmt.Errorf("the %s destination already holds a secret whose version could not be read: %w", rec.Backend, metaErr)
		}
		if meta.CurrentVersion <= 0 {
			return 0, fmt.Errorf("the %s destination already holds a secret with no current version; nothing was overwritten", rec.Backend)
		}
		written, readErr := store.Read(ctx, sc, destination, meta.CurrentVersion)
		if readErr != nil {
			return 0, fmt.Errorf("the %s destination already holds a secret that could not be read back: %w", rec.Backend, readErr)
		}
		defer written.Value.Destroy()
		if !bytes.Equal(written.Value.UnsafeBytes(), value.UnsafeBytes()) {
			return 0, fmt.Errorf("the %s destination already holds a different secret; nothing was overwritten", rec.Backend)
		}
		return meta.CurrentVersion, nil
	}
	if err != nil {
		return 0, fmt.Errorf("write the %s secret: %w", rec.Backend, err)
	}
	return version, nil
}

func (item *migrateItem) fill(rec repos.CredentialReferenceRecord) {
	item.CredentialReferenceID, item.TargetBindingID = rec.ID, rec.TargetBindingID
	item.Backend, item.Locator, item.Generation = rec.Backend, rec.Locator, rec.Generation
	item.Class, item.State, item.TargetIdentity = string(rec.Class), string(rec.State), rec.TargetIdentity
	item.SecretLocation = item.location("")
}

func (item *migrateItem) location(mount string) string {
	if item.Class != string(credentialgrant.ClassStaticSecret) {
		return ""
	}
	if item.Backend == secretstore.BackendVault {
		if mount == "" {
			mount = "<kv mount>"
		}
		version := "<new>"
		if item.Generation > 0 {
			version = strconv.FormatInt(item.Generation, 10)
		}
		return "vault:" + mount + "/" + item.Locator + "@" + version
	}
	if item.Generation <= 0 {
		// A composite key map this run has not written yet.
		return "pgvault:" + item.Locator + "@<new>"
	}
	return "pgvault:" + item.Locator + "@" + strconv.FormatInt(item.Generation, 10)
}

func printMigrateItems(w io.Writer, items []migrateItem, opts migrateOptions) {
	if opts.json {
		encoder := json.NewEncoder(w)
		for _, item := range items {
			_ = encoder.Encode(item)
		}
		return
	}
	counts := map[string]int{}
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "ACTION\tPROJECT\tTOOLKIT\tTYPE\tCLASS\tTARGET\tSECRET\tREFERENCE\tSTATE\tREASON")
	for _, item := range items {
		counts[item.Action]++
		_, _ = fmt.Fprintf(table, "%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", item.Action, item.ProjectID, item.ToolkitID,
			item.ToolkitType, dash(item.Class), dash(item.TargetIdentity), dash(item.SecretLocation),
			dash(item.CredentialReferenceID), dash(item.State), dash(item.Reason))
	}
	_ = table.Flush()
	actions := make([]string, 0, len(counts))
	for action := range counts {
		actions = append(actions, action)
	}
	slices.Sort(actions)
	parts := make([]string, 0, len(actions))
	for _, action := range actions {
		parts = append(parts, fmt.Sprintf("%d %s", counts[action], action))
	}
	summary := strings.Join(parts, ", ")
	if summary == "" {
		summary = "nothing to migrate"
	}
	_, _ = fmt.Fprintf(w, "\n%d toolkit(s): %s.\n", len(items), summary)
	if opts.apply {
		_, _ = fmt.Fprintln(w, "References are staged only. Next, through the admin API: activate each (by a user other than the operator),")
		_, _ = fmt.Fprintln(w, "attach it to its toolkits, pin the toolkits to the agent binding, then propose and approve a grant.")
	} else {
		_, _ = fmt.Fprintln(w, "Dry run: nothing was written. Run again with --apply --operator USER_ID to stage.")
	}
}

func dash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
