package main

import (
	"fmt"
	"log/slog"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/publicproject"
)

// resolvePublicProject settles the ONE id of the platform's public project and
// refuses to continue when the environment names more than one.
//
// # WHY IT IS A REFUSAL AND NOT A WARNING
//
// This value ends up in a schema name. The admin panel writes a shared
// credential into `p_{id}`, the LLM gateway resolves shared credentials out of
// `p_{id}`, the publish guards compare a version's `model_project_id` against
// it, and the SPA decides "is this the public project?" with it. Two names that
// disagree therefore do not produce a wrong answer on one surface — they
// produce a system where the credential is stored, listed, and reported
// healthy, and resolves for nobody. Nothing logs, nothing 500s, and every probe
// stays green. That is the failure a boot refusal costs one message to remove.
//
// It runs EARLY, before the database pool and before any handler is composed,
// for the same reason the vault master-key check above it does: the fault it
// catches is invisible everywhere later.
//
// A deprecated name is a WARNING rather than a refusal. `PUBLIC_PROJECT_ID`,
// `SHARED_PROJECT_ID` and `AI_PROJECT_ID` are deployed today, so refusing them
// would break a running installation to fix a naming problem. They keep
// working, and the log line names the variable to remove.
func resolvePublicProject(logger *slog.Logger, lookup func(string) (string, bool)) (int, error) {
	resolution, err := publicproject.Resolve(lookup)
	if err != nil {
		return 0, fmt.Errorf("resolve the public project id: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	for _, name := range resolution.DeprecatedNames {
		logger.Warn("deprecated public project variable",
			"variable", name,
			"use", publicproject.Canonical,
			"public_project_id", resolution.ID)
	}
	return resolution.ID, nil
}
