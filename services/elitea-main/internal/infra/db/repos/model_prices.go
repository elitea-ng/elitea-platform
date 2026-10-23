package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// ErrAmountVisibilityUnavailable reports an AmountVisibility built without a
// database pool.
var ErrAmountVisibilityUnavailable = errors.New("amount visibility: no database pool")

// AmountVisibility is THE rule for whether a caller may see platform cost
// figures for a project: the owner of a personal project may (its spend is
// their own, and token-based integrations land there), and so may a holder of
// the project's `admin` role. The budget routes' can_see_amounts and the model
// catalogue's prices both ask it, so a member refused the amounts on one
// surface cannot read them off the other.
//
// A failure to ASK is returned, never answered as "no": callers decide what a
// failed read means for their response.
type AmountVisibility struct {
	pool *pgxpool.Pool
}

// NewAmountVisibility builds the rule over the given pool.
func NewAmountVisibility(pool *pgxpool.Pool) *AmountVisibility {
	return &AmountVisibility{pool: pool}
}

// CanSeeAmounts reports whether userID may see cost figures for projectID.
func (v *AmountVisibility) CanSeeAmounts(ctx context.Context, projectID, userID int64) (bool, error) {
	personal, err := v.IsPersonalProject(ctx, projectID, userID)
	if err != nil {
		return false, err
	}
	if personal {
		return true, nil
	}
	return v.IsProjectAdmin(ctx, projectID, userID)
}

// IsProjectAdmin reports whether the user holds the project's `admin` role —
// the check `admin_check_user_is_admin` performed in the reference, asked
// directly of the tables legacyrbac.PostgresResolver reads.
func (v *AmountVisibility) IsProjectAdmin(ctx context.Context, projectID, userID int64) (bool, error) {
	if v == nil || v.pool == nil {
		return false, ErrAmountVisibilityUnavailable
	}
	var isAdmin bool
	err := v.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM public.auth_core__project_user_role AS assignment
    JOIN public.auth_core__project_role AS project_role
      ON project_role.id = assignment.role_id
     AND project_role.project_id = assignment.project_id
    WHERE assignment.project_id = $1
      AND assignment.user_id = $2
      AND project_role.name = 'admin'
)`, projectID, userID).Scan(&isAdmin)
	if err != nil {
		return false, fmt.Errorf("resolve project admin: %w", err)
	}
	return isAdmin, nil
}

// IsPersonalProject reports whether the project is this user's own personal
// project. The predicate is the one the admin project listing uses
// (internal/api/v2/admin/projects.go personalProjectPredicate) resolved to the
// specific owner.
func (v *AmountVisibility) IsPersonalProject(ctx context.Context, projectID, userID int64) (bool, error) {
	if v == nil || v.pool == nil {
		return false, ErrAmountVisibilityUnavailable
	}
	// The name is composed here rather than in SQL: `'project_user_' || $2` puts
	// the parameter in a text context, and pgx then refuses to encode an int64
	// as text at all ("cannot find encode plan").
	var personal bool
	err := v.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1 AND name = $2)`,
		projectID, "project_user_"+strconv.FormatInt(userID, 10)).Scan(&personal)
	if err != nil {
		return false, fmt.Errorf("resolve personal project: %w", err)
	}
	return personal, nil
}

// modelPricesSQL reads the gateway's own price catalogue — the table its budget
// gate prices every call with (services/elitea-llm-gateway/internal/cost,
// modelPriceSQL) — for a set of model names in one statement. The NUMERIC
// columns cross as text so no price is ever rounded through a float. The cache
// columns are deliberately not read: the gateway reads none of them and bills
// cached input at the input price.
const modelPricesSQL = `
SELECT provider, model_name,
       input_cost_per_1m_tokens::text,
       output_cost_per_1m_tokens::text
FROM gateway.gateway_models
WHERE model_name = ANY($1::text[])`

// ModelPriceRepository serves the model catalogue's per-1M-token prices to a
// caller who may see amounts.
type ModelPriceRepository struct {
	pool       *pgxpool.Pool
	visibility *AmountVisibility
}

// NewModelPriceRepository builds the price reader over the given pool.
func NewModelPriceRepository(pool *pgxpool.Pool) *ModelPriceRepository {
	return &ModelPriceRepository{pool: pool, visibility: NewAmountVisibility(pool)}
}

// ModelPrices returns the price of each name the catalogue rule
// (configurationapp.MatchModelPrices) can resolve, or nothing at all when the
// caller may not see amounts for the project.
func (r *ModelPriceRepository) ModelPrices(
	ctx context.Context, projectID, userID int64, names []string,
) (map[string]configurationapp.ModelPrice, error) {
	if r == nil || r.pool == nil || len(names) == 0 {
		return nil, nil
	}
	visible, err := r.visibility.CanSeeAmounts(ctx, projectID, userID)
	if err != nil {
		return nil, fmt.Errorf("model prices: %w", err)
	}
	if !visible {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, modelPricesSQL, configurationapp.ModelPriceLookupNames(names))
	if err != nil {
		return nil, fmt.Errorf("model prices: query: %w", err)
	}
	defer rows.Close()
	var priceRows []configurationapp.ModelPriceRow
	for rows.Next() {
		var row configurationapp.ModelPriceRow
		if err := rows.Scan(&row.Provider, &row.ModelName, &row.Input, &row.Output); err != nil {
			return nil, fmt.Errorf("model prices: scan: %w", err)
		}
		priceRows = append(priceRows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("model prices: rows: %w", err)
	}
	return configurationapp.MatchModelPrices(names, priceRows), nil
}
