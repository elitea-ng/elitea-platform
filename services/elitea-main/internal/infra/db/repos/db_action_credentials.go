package repos

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/dbaction"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

const dbConfigurationPermission = "configurations.configuration.details"

// DBActionUnsecreter resolves {{secret.NAME}} placeholders of one frozen SQL
// configuration from the project vault (storage.CurrentVaultUnsecreter).
type DBActionUnsecreter interface {
	Unsecret(ctx context.Context, projectID int32, data map[string]any) (map[string]any, error)
}

// DBActionCredentials resolves DB action rules at start time into frozen
// sealed configuration references (ready for CurrentConfigurationsMaterializer)
// and provides the claim-time SQL credential materialization helper for 5e-3.
type DBActionCredentials struct {
	store      sharedStore
	unsecreter DBActionUnsecreter
}

func NewDBActionCredentials(pool *pgxpool.Pool, unsecreter DBActionUnsecreter) (*DBActionCredentials, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, dbaction.ErrUnavailable
	}
	return &DBActionCredentials{store: store, unsecreter: unsecreter}, nil
}

// LoadFrozenRule resolves one enabled project DB action rule and its sealed
// `sql` configuration reference in a single RepeatableRead read-only
// transaction (atomic MVCC snapshot across rule, RBAC, and configuration),
// enforcing:
//   - rule belongs to projectID, passes Validate, and is enabled;
//   - executing actor holds configurations.configuration.details in projectID;
//   - referenced configuration has type='sql' and belongs to projectID;
//   - password is a sealed vault secret reference (never raw plaintext);
//   - private configurations are admitted only when owned by both the executing
//     actor and the rule's last writer.
//
// The returned SQLConfiguration keeps vault secrets sealed and carries
// CurrentFrozenConfigurationMarker so CurrentConfigurationsMaterializer
// unsecrets strictly the password at Worker claim time.
func (c *DBActionCredentials) LoadFrozenRule(
	ctx context.Context,
	projectID, actorID int64,
	ruleName string,
) (dbaction.FrozenRule, error) {
	if ctx == nil {
		return dbaction.FrozenRule{}, dbaction.ErrPolicyDenied
	}
	if err := ctx.Err(); err != nil {
		return dbaction.FrozenRule{}, err
	}
	if projectID <= 0 || projectID > 2147483647 || actorID <= 0 || !dbaction.ValidRuleName(ruleName) {
		return dbaction.FrozenRule{}, dbaction.ErrPolicyDenied
	}
	var frozen dbaction.FrozenRule
	err := c.store.WithinTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(tx sqlExecutor) error {
		rule, err := readDBActionRule(ctx, tx, projectID, ruleName, false)
		if err != nil {
			if isContextErr(ctx, err) {
				return contextErr(ctx, err)
			}
			if errors.Is(err, ErrDBActionRuleNotFound) || errors.Is(err, ErrDBActionRuleStoredInvalid) {
				return dbaction.ErrPolicyDenied
			}
			return dbaction.ErrUnavailable
		}
		if !rule.Enabled {
			return dbaction.ErrPolicyDenied
		}
		sealedConfig, err := sqlConfigurationFreeze(ctx, tx, projectID, actorID, rule.UpdatedBy, rule.ConfigurationID, rule.Dialect)
		if err != nil {
			return err
		}
		frozen = dbaction.FrozenRule{
			Name:             rule.Name,
			RuleID:           rule.ID,
			Revision:         rule.Revision,
			Dialect:          rule.Dialect,
			DatabaseName:     rule.DatabaseName,
			Mode:             rule.Mode,
			MarkerTable:      rule.MarkerTable,
			ConfigurationID:  rule.ConfigurationID,
			SQLConfiguration: sealedConfig,
			PolicyDigest:     rule.PolicyDigest(),
		}
		return nil
	})
	if err != nil {
		if isContextErr(ctx, err) {
			return dbaction.FrozenRule{}, contextErr(ctx, err)
		}
		return dbaction.FrozenRule{}, err
	}
	return frozen, nil
}

// MaterializeSQLCredential unsecrets strictly the sealed `password` reference
// of a frozen `sql` configuration map for a claimed Worker, enforces a strict
// flat key allowlist, validates all target/credential bounds, and returns a
// redacted-by-type MaterializedSQLCredential.
func (c *DBActionCredentials) MaterializeSQLCredential(
	ctx context.Context,
	projectID int64,
	dialect string,
	frozenConfig map[string]any,
) (dbaction.MaterializedSQLCredential, error) {
	if ctx == nil {
		return dbaction.MaterializedSQLCredential{}, dbaction.ErrPolicyDenied
	}
	if err := ctx.Err(); err != nil {
		return dbaction.MaterializedSQLCredential{}, err
	}
	if c.unsecreter == nil || projectID <= 0 || projectID > 2147483647 || frozenConfig == nil {
		return dbaction.MaterializedSQLCredential{}, dbaction.ErrPolicyDenied
	}
	if !validFlatFrozenSQLConfig(frozenConfig) {
		return dbaction.MaterializedSQLCredential{}, dbaction.ErrPolicyDenied
	}
	marked, _ := frozenConfig[configurationapp.CurrentFrozenConfigurationMarker].(bool)
	configType, _ := frozenConfig["configuration_type"].(string)
	configUUID, _ := frozenConfig["configuration_uuid"].(string)
	if !marked || configType != "sql" || !validFrozenUUID(configUUID) {
		return dbaction.MaterializedSQLCredential{}, dbaction.ErrPolicyDenied
	}
	ownerProjectID, ok := frozenProjectID(frozenConfig["configuration_project_id"])
	if !ok || int64(ownerProjectID) != projectID {
		return dbaction.MaterializedSQLCredential{}, dbaction.ErrPolicyDenied
	}
	rawPassword, ok := frozenConfig["password"].(string)
	if !ok || !configurationapp.IsCurrentSecretReference(rawPassword) || len(rawPassword) > dbaction.MaxSQLPasswordBytes {
		return dbaction.MaterializedSQLCredential{}, dbaction.ErrPolicyDenied
	}
	// Unsecret strictly the sealed password field; never pass host, username,
	// or nested structures through vault expansion.
	unsecretedPasswordMap, err := c.unsecreter.Unsecret(ctx, ownerProjectID, map[string]any{"password": rawPassword})
	if err != nil {
		if isContextErr(ctx, err) {
			return dbaction.MaterializedSQLCredential{}, contextErr(ctx, err)
		}
		if errors.Is(err, configurationapp.ErrInvalidCurrentExpansion) ||
			errors.Is(err, configurationapp.ErrCurrentExpansionNotFound) ||
			errors.Is(err, configurationapp.ErrCurrentExpansionRecursion) ||
			errors.Is(err, configurationapp.ErrCurrentExpansionForbidden) ||
			strings.Contains(err.Error(), "secret expansion rejected") {
			return dbaction.MaterializedSQLCredential{}, dbaction.ErrPolicyDenied
		}
		return dbaction.MaterializedSQLCredential{}, dbaction.ErrUnavailable
	}
	materialized := make(map[string]any, len(frozenConfig))
	for k, v := range frozenConfig {
		if k == configurationapp.CurrentFrozenConfigurationMarker {
			continue
		}
		materialized[k] = v
	}
	materialized["password"] = unsecretedPasswordMap["password"]
	return dbaction.ParseMaterializedSQLCredential(dialect, materialized)
}

func sqlConfigurationFreeze(
	ctx context.Context,
	tx sqlExecutor,
	projectID, actorID, ruleUpdatedBy int64,
	configurationID int32,
	dialect string,
) (map[string]any, error) {
	executor, ok := tx.(pgxExecutor)
	if !ok || projectID <= 0 || projectID > 2147483647 || actorID <= 0 || configurationID <= 0 {
		return nil, dbaction.ErrPolicyDenied
	}
	result, err := legacyrbac.NewTransactionResolver(executor.queryer).ResolvePermissions(
		ctx,
		auth.User{UserID: strconv.FormatInt(actorID, 10)},
		auth.PermissionModeDefault,
		strconv.FormatInt(projectID, 10),
	)
	if isContextErr(ctx, err) {
		return nil, contextErr(ctx, err)
	}
	if errors.Is(err, auth.ErrPermissionDenied) {
		return nil, dbaction.ErrPolicyDenied
	}
	if err != nil {
		return nil, dbaction.ErrUnavailable
	}
	permitted := false
	for _, granted := range result.Permissions {
		if granted == dbConfigurationPermission {
			permitted = true
			break
		}
	}
	if result.UserID != actorID || !permitted {
		return nil, dbaction.ErrPolicyDenied
	}

	table := pgx.Identifier{"p_" + strconv.FormatInt(projectID, 10), "configuration"}.Sanitize()
	var (
		uuid   string
		kind   string
		data   string
		shared bool
		author *int64
	)
	err = tx.QueryRow(ctx, `SELECT uuid::text, type, data::text, shared, author_id FROM `+table+` WHERE id = $1 AND project_id = $2`,
		configurationID, int32(projectID)).Scan(&uuid, &kind, &data, &shared, &author)
	if isContextErr(ctx, err) {
		return nil, contextErr(ctx, err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, dbaction.ErrPolicyDenied
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "3F000" || pgErr.Code == "42P01") {
			return nil, dbaction.ErrPolicyDenied
		}
		return nil, dbaction.ErrUnavailable
	}
	privateAllowed := author != nil && *author == actorID && ruleUpdatedBy > 0 && *author == ruleUpdatedBy
	if kind != "sql" || (!shared && author != nil && !privateAllowed) || len(data) > maxDBConfigurationData || !validFrozenUUID(uuid) {
		return nil, dbaction.ErrPolicyDenied
	}
	parsed, err := decodeSQLConfigurationJSON([]byte(data))
	if err != nil {
		return nil, dbaction.ErrPolicyDenied
	}
	host, port, err := dbaction.ParseSQLTarget(dialect, parsed)
	if err != nil {
		return nil, dbaction.ErrPolicyDenied
	}
	username, _ := parsed["username"].(string)
	password, passwordOK := parsed["password"].(string)
	// Require a sealed vault secret reference so legacy plaintext passwords
	// can never be copied into durable start-time execution snapshots.
	if !passwordOK || !configurationapp.IsCurrentSecretReference(password) || len(password) > dbaction.MaxSQLPasswordBytes || strings.ContainsRune(password, '\x00') {
		return nil, dbaction.ErrPolicyDenied
	}
	return map[string]any{
		configurationapp.CurrentFrozenConfigurationMarker: true,
		"configuration_uuid":                              uuid,
		"configuration_project_id":                        int32(projectID),
		"configuration_type":                              "sql",
		"host":                                            host,
		"port":                                            int(port),
		"username":                                        username,
		"password":                                        password,
	}, nil
}

func validFlatFrozenSQLConfig(cfg map[string]any) bool {
	if len(cfg) == 0 || len(cfg) > 9 {
		return false
	}
	for k, v := range cfg {
		switch k {
		case configurationapp.CurrentFrozenConfigurationMarker,
			"configuration_uuid", "configuration_project_id", "configuration_type",
			"host", "port", "username", "password", "dialect":
		default:
			return false
		}
		switch v.(type) {
		case nil, bool, string, int, int32, int64, float64, json.Number:
		default:
			return false
		}
	}
	return true
}

func validFrozenUUID(uuid string) bool {
	if uuid == "" || len(uuid) > configurationapp.MaxCurrentToolkitSettingsIdentifier {
		return false
	}
	for _, r := range uuid {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func frozenProjectID(raw any) (int32, bool) {
	var parsed int64
	switch v := raw.(type) {
	case int32:
		parsed = int64(v)
	case int64:
		parsed = v
	case int:
		parsed = int64(v)
	case json.Number:
		n, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return 0, false
		}
		parsed = n
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Trunc(v) != v || v < 1 || v > math.MaxInt32 {
			return 0, false
		}
		parsed = int64(v)
	default:
		return 0, false
	}
	return int32(parsed), parsed > 0 && parsed <= math.MaxInt32
}

func isContextErr(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func contextErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
