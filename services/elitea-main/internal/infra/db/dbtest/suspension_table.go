package dbtest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// InstallSuspensionTable creates elitea_runtime.execution_suspensions from
// shared 0194 on pool's database. platformconfig.LoadGuardrails reads it next
// to centry.platform_config, so a harness that builds platform_config by hand
// and reaches the guardrail read needs it too. Only the part of 0194 that
// creates the table and its indexes applies: the rest alters
// agent_execution_jobs, which such a harness does not hold.
func InstallSuspensionTable(ctx context.Context, pool *pgxpool.Pool) error {
	body, err := platformmigrations.Files.ReadFile("shared/0194_execution_suspensions.sql")
	if err != nil {
		return fmt.Errorf("dbtest: read 0194: %w", err)
	}
	table, _, found := strings.Cut(string(body), "ALTER TABLE elitea_runtime.agent_execution_jobs")
	if !found {
		return errors.New("dbtest: 0194 no longer alters agent_execution_jobs; update InstallSuspensionTable")
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS elitea_runtime;\n"+table); err != nil {
		return fmt.Errorf("dbtest: create the suspension table: %w", err)
	}
	return nil
}
