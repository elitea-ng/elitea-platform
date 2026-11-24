package repos

import (
	"context"
	"errors"
	"strconv"

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
// `sql` configuration reference in a single read-only transaction, enforcing:
//   - rule belongs to projectID, passes Validate, and is enabled;
//   - executing actor holds configurations.configuration.details in projectID;
//   - referenced configuration has type='sql' and belongs to projectID;
//   - private configurations are admitted only when owned by both the executing
//     actor and the rule's last writer (prevents privilege escalation via a
//     private credential bound by another admin).
//
// The returned SQLConfiguration keeps vault secrets sealed and carries
// CurrentFrozenConfigurationMarker so CurrentConfigurationsMaterializer
// unsecrets it strictly at Worker claim time.
func (c *DBActionCredentials) LoadFrozenRule(
	ctx context.Context,
	projectID, actorID int64,
	ruleName string,
) (dbaction.FrozenRule, error) {
	if ctx == nil || projectID <= 0 || projectID > 2147483647 || actorID <= 0 || !dbaction.ValidRuleName(ruleName) {
		return dbaction.FrozenRule{}, dbaction.ErrPolicyDenied
	}
	var frozen dbaction.FrozenRule
	err := c.store.WithinTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx sqlExecutor) error {
		rule, err := readDBActionRule(ctx, tx, projectID, ruleName, false)
		if err != nil {
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
		return dbaction.FrozenRule{}, err
	}
	return frozen, nil
}

// MaterializeSQLCredential unsecrets a frozen `sql` configuration map strictly
// for a claimed Worker, validates its target/credential bounds, and returns a
// redacted-by-type MaterializedSQLCredential ready for Worker SQL execution.
func (c *DBActionCredentials) MaterializeSQLCredential(
	ctx context.Context,
	projectID int64,
	dialect string,
	frozenConfig map[string]any,
) (dbaction.MaterializedSQLCredential, error) {
	if ctx == nil || c.unsecreter == nil || projectID <= 0 || projectID > 2147483647 || frozenConfig == nil {
		return dbaction.MaterializedSQLCredential{}, dbaction.ErrPolicyDenied
	}
	marked, _ := frozenConfig[configurationapp.CurrentFrozenConfigurationMarker].(bool)
	configType, _ := frozenConfig["configuration_type"].(string)
	if !marked || configType != "sql" {
		return dbaction.MaterializedSQLCredential{}, dbaction.ErrPolicyDenied
	}
	ownerProjectID, ok := frozenProjectID(frozenConfig["configuration_project_id"])
	if !ok || int64(ownerProjectID) != projectID {
		return dbaction.MaterializedSQLCredential{}, dbaction.ErrPolicyDenied
	}
	unsecreted, err := c.unsecreter.Unsecret(ctx, ownerProjectID, frozenConfig)
	if err != nil {
		return dbaction.MaterializedSQLCredential{}, dbaction.ErrUnavailable
	}
	return dbaction.ParseMaterializedSQLCredential(dialect, unsecreted)
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
	if kind != "sql" || (!shared && author != nil && !privateAllowed) || len(data) > maxDBConfigurationData {
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
	// Never allow caller-supplied data to carry an unverified marker or forge
	// another project's vault owner; construct a fresh sealed configuration map
	// containing only the vetted SQL fields plus Main-owned freeze metadata.
	username, _ := parsed["username"].(string)
	password, passwordOK := parsed["password"].(string)
	if !passwordOK || password == "" {
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

func frozenProjectID(raw any) (int32, bool) {
	switch v := raw.(type) {
	case int32:
		return v, v > 0
	case int64:
		return int32(v), v > 0 && v <= 2147483647
	case int:
		return int32(v), v > 0 && v <= 2147483647
	default:
		return 0, false
	}
}
