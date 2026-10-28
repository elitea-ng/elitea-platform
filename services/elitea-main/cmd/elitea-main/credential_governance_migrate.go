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
// No secret value moves unless --copy-to vault is given; then the pgvault
// value is written to Vault/OpenBao KV v2 with check-and-set version 0
// through the staged-write ledger (#1113), and the pgvault secret stays,
// because legacy runs still read it. The reference is staged first at
// generation 0 and takes the written version once the copy lands.

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
	migrateReferencesCopyScope    = "credential-migration"
	migrateReferencesOAuthLocator = "oauth_grant"
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
	action                migrateAction
	migratedFrom          string
	secretName            string
}

// migrateProvider is the configuration rule of one toolkit type.
type migrateProvider struct {
	targetField  string
	secretFields []string
	oauthOnly    bool
}

var migrateProviders = map[string]migrateProvider{
	"jira":       {targetField: "base_url", secretFields: []string{"token", "api_key"}},
	"confluence": {targetField: "base_url", secretFields: []string{"token", "api_key"}},
	"qtest":      {targetField: "base_url", secretFields: []string{"qtest_api_token"}},
	"aha":        {targetField: "base_url", secretFields: []string{"api_key"}},
	"sharepoint": {targetField: "site_url", oauthOnly: true},
}

// migrateSecrets is what --copy-to vault reads from and writes to.
type migrateSecrets struct {
	source secretstore.SecretStore // pgvault
	vault  secretstore.SecretStore
	mount  string
}

// migrateDeps are the seams a test replaces.
type migrateDeps struct {
	connect func(ctx context.Context, dsn string) (*pgxpool.Pool, error)
	secrets func(lookup func(string) (string, bool), pool *pgxpool.Pool) (migrateSecrets, error)
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
// destination the same way the credential broker does.
func migrateSecretsFromEnv(lookup func(string) (string, bool), pool *pgxpool.Pool) (migrateSecrets, error) {
	masterKey, err := encodedVaultMasterKeyFromEnv(lookup)
	if err != nil {
		return migrateSecrets{}, err
	}
	if masterKey == nil {
		// Main's start-up rule: no key only with the development opt-out,
		// whose project vault keys are stored unwrapped.
		getenv := func(name string) string { value, _ := lookup(name); return value }
		if _, err := requireVaultMasterKey(getenv); err != nil {
			return migrateSecrets{}, fmt.Errorf("--copy-to vault reads the pgvault secret: %w", err)
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
	if opts.copyToVault && opts.policyRevision == "" {
		opts.policyRevision, _ = lookup(profile.EnvPolicyRevision)
		opts.policyRevision = strings.TrimSpace(opts.policyRevision)
		if opts.policyRevision == "" {
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
		if secrets, err = deps.secrets(lookup, pool); err != nil {
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
	if opts.apply && opts.copyToVault {
		if m.ledger, err = secretstore.NewLedger(pool); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
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
	ledger   *secretstore.Ledger
	newID    func() string
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
			break
		}
	}
	if !descriptor.Accepts(class) {
		return skip(item, "class_not_supported")
	}
	item.Class = string(class)
	if class == credentialgrant.ClassStaticSecret {
		if m.opts.copyToVault {
			item.Backend = secretstore.BackendVault
			item.Locator = migrateVaultPath(project, item.ConfigurationUUID)
		} else {
			item.Locator, item.Generation = item.secretName, 1
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

// copyPending is a --copy-to vault reference staged at generation 0 whose
// copy has not completed (a crash between the staging and the write).
func copyPending(rec repos.CredentialReferenceRecord) bool {
	return rec.Backend == secretstore.BackendVault && rec.Class == credentialgrant.ClassStaticSecret &&
		rec.Generation == 0 && rec.State == credentialgrant.ReferenceStaged
}

// settle looks the unit up and, with --apply, stages it.
//
// A --copy-to vault reference is staged FIRST, at generation 0, so the
// audit decision that authorises the pgvault read, the ledger row and the
// Vault write all name the id of a reference that exists. The copy then
// records the version it wrote as the reference's generation (activation
// refuses a static secret at generation 0). A rerun that finds a reference
// still at generation 0 completes its copy under that same id.
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
		if item.Backend == secretstore.BackendVault {
			staged.Generation = 0
		}
		result, err := m.store.StageReference(ctx, staged, m.newID)
		if err != nil {
			return err
		}
		rec, created = result.Record, result.Created
	}
	if copyPending(rec) {
		version, err := m.copyToVault(ctx, *item, rec.ID)
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

// copyToVault copies one pgvault secret to Vault/OpenBao for the staged
// reference referenceID. The audit decision that authorises the read is
// appended first; its id is the read's StoreContext.DecisionID. The write is
// check-and-set 0 through the ledger. The path is fixed per configuration,
// so a rerun after a failure finds the secret it wrote: on a check-and-set
// conflict the path's CURRENT version is read, and it is used only when its
// bytes equal the pgvault value. Anything else stops the run.
func (m *referenceMigration) copyToVault(ctx context.Context, item migrateItem, referenceID string) (int64, error) {
	if m.secrets.source == nil || m.secrets.vault == nil || m.ledger == nil {
		return 0, errors.New("--copy-to vault: the secret stores are not configured")
	}
	decisionID := m.newID()
	if err := m.store.RecordSecretCopy(ctx, item.ProjectID, referenceID, decisionID, m.opts.operator); err != nil {
		return 0, err
	}
	sc := secretstore.StoreContext{
		TenantProjectID: item.ProjectID, CredentialRefID: referenceID, DecisionID: decisionID,
		WorkloadScope: migrateReferencesCopyScope, PolicyRevisionID: m.opts.policyRevision,
	}
	source, err := m.secrets.source.Read(ctx, sc,
		secretstore.SecretRef{Backend: secretstore.BackendPGVault, Path: item.secretName}, 1)
	if err != nil {
		return 0, fmt.Errorf("read the pgvault secret: %w", err)
	}
	defer source.Value.Destroy()
	destination := secretstore.SecretRef{Backend: secretstore.BackendVault, Mount: m.secrets.mount, Path: item.Locator}
	version, err := m.ledger.StagedWrite(ctx, m.secrets.vault, sc, destination, source.Value, 0)
	if errors.Is(err, secretstore.ErrVersionConflict) {
		meta, metaErr := m.secrets.vault.Metadata(ctx, sc, destination)
		if metaErr != nil {
			return 0, fmt.Errorf("the Vault path already holds a secret whose version could not be read: %w", metaErr)
		}
		if meta.CurrentVersion <= 0 {
			return 0, errors.New("the Vault path already holds a secret with no current version; nothing was overwritten")
		}
		written, readErr := m.secrets.vault.Read(ctx, sc, destination, meta.CurrentVersion)
		if readErr != nil {
			return 0, fmt.Errorf("the Vault path already holds a secret that could not be read back: %w", readErr)
		}
		defer written.Value.Destroy()
		if !bytes.Equal(written.Value.UnsafeBytes(), source.Value.UnsafeBytes()) {
			return 0, errors.New("the Vault path already holds a different secret; nothing was overwritten")
		}
		return meta.CurrentVersion, nil
	}
	if err != nil {
		return 0, fmt.Errorf("write the Vault secret: %w", err)
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
