// Package publicproject resolves the ONE id of the platform's public project —
// the project whose `shared = true` configurations every other project may use,
// and whose schema holds the published catalogue.
//
// # WHY THIS PACKAGE EXISTS
//
// The same value was configured in four different environment variables, read
// by six different call sites, with three different defaults:
//
//   - `ELITEA_AI_PROJECT_ID` (default 1) — internal/api/middleware/project.go,
//     cmd/elitea-main/configurations_config.go, and the LLM gateway.
//   - `AI_PROJECT_ID` (default 1) — the older name of the same variable.
//   - `PUBLIC_PROJECT_ID` (default 1) — internal/api/v2/eliteacore and
//     internal/api/v2/conversations.
//   - `SHARED_PROJECT_ID` (default 4) — a SECOND accepted project in the two
//     publish guards in internal/api/v2/eliteacore/handler.go.
//
// Nothing checked that the four agreed. They name one thing, so a deployment
// whose public project is not id 1 was correct on some surfaces and wrong on
// the others, with no error anywhere: the admin panel writes a shared
// credential into `p_{one value}` and the gateway resolves it out of
// `p_{another}`, so the credential is stored, listed, reported healthy, and
// resolves for nobody.
//
// `SHARED_PROJECT_ID` was drift rather than a real second project. The
// reference compares a published agent's `model_project_id` against exactly one
// id (legacy/plugins/elitea_core/utils/publish_utils.py `_check_shared_llm`,
// which takes a single `public_project_id`), and the reference reads that id
// from one place (`elitea_core/utils/utils.py get_public_project_id`, default
// 1). This package restores that: one id, one default.
//
// # TWO ENTRY POINTS, ON PURPOSE
//
// Resolve is STRICT and runs once, at startup: it refuses a value that is not a
// project id, and it refuses two names that disagree. An operator reads the
// reason on the terminal that started the process.
//
// ID is LENIENT and runs per request: it takes the first name that carries a
// usable id and otherwise gives the default. It cannot fail, because a request
// handler has no way to report a configuration error to the caller that the
// caller could act on. The strict check at startup is what makes the lenient
// one safe — a process that reached a request handler has already proved its
// names agree.
package publicproject

import (
	"fmt"
	"os"
	"strconv"
)

// Default mirrors pylon's elitea_config "ai_project_id" default (1).
const Default = 1

// Canonical is the one variable an operator should set. It is the name the LLM
// gateway reads (services/elitea-llm-gateway/internal/config), the name
// cmd/elitea-main/configurations_config.go requires, and the name the Helm
// chart derives every component's copy from.
const Canonical = "ELITEA_AI_PROJECT_ID"

// Deprecated names still resolve, so no deployed values file breaks, but each
// one that carries a value is reported at startup. They are listed in
// precedence order after Canonical.
//
// `SHARED_PROJECT_ID` is here rather than gone because a deployment that set it
// must keep working; it is an ALIAS now, not a second project.
var Deprecated = []string{"AI_PROJECT_ID", "PUBLIC_PROJECT_ID", "SHARED_PROJECT_ID"}

// names is the full precedence order: the canonical name first.
func names() []string {
	all := make([]string, 0, 1+len(Deprecated))
	all = append(all, Canonical)
	return append(all, Deprecated...)
}

// Resolution is what Resolve found.
type Resolution struct {
	// ID is the resolved public project id.
	ID int
	// Source is the variable the id came from, or "" when nothing was set and
	// ID is Default.
	Source string
	// DeprecatedNames lists the deprecated variables that carried a value, in
	// precedence order. Log a deprecation warning for each.
	DeprecatedNames []string
}

// Resolve reads the public project id from lookup and refuses a configuration
// that names more than one project.
//
// It returns Default with an empty Source when no name is set: that is the
// documented behaviour of every call site this package replaced, and changing
// it would take the shared block away from every deployment that never set the
// variable.
func Resolve(lookup func(string) (string, bool)) (Resolution, error) {
	if lookup == nil {
		return Resolution{}, fmt.Errorf("public project environment lookup is required")
	}
	res := Resolution{ID: Default}
	for _, name := range names() {
		raw, present := lookup(name)
		if !present || raw == "" {
			continue
		}
		id, err := strconv.Atoi(raw)
		if err != nil || id <= 0 {
			return Resolution{}, fmt.Errorf("%s is invalid: it must be a positive project id, got %q", name, raw)
		}
		if name != Canonical {
			res.DeprecatedNames = append(res.DeprecatedNames, name)
		}
		if res.Source == "" {
			res.ID = id
			res.Source = name
			continue
		}
		if id != res.ID {
			return Resolution{}, fmt.Errorf(
				"%s=%d and %s=%d name different public projects; they are one setting, so set only %s",
				res.Source, res.ID, name, id, Canonical)
		}
	}
	return res, nil
}

// ID is the per-request resolution. See the package comment for why it is
// lenient where Resolve is strict.
func ID() int {
	for _, name := range names() {
		if raw := os.Getenv(name); raw != "" {
			if id, err := strconv.Atoi(raw); err == nil && id > 0 {
				return id
			}
		}
	}
	return Default
}

// IDString is ID rendered for the string comparisons the publish guards make
// against a jsonb `model_project_id`.
func IDString() string {
	return strconv.Itoa(ID())
}
