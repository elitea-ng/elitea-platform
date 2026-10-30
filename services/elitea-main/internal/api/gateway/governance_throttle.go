package gateway

// governance_throttle.go — the throttle scope of a rate_limit entry
// (ADR-0041 decision 4).
//
// A rate_limit entry may name `scope.agent_ids` and `scope.user_ids`. The LLM
// gateway then counts the entry per agent or per user and refuses with 429
// when the ceiling is reached (elitea-llm-gateway internal/policy). The
// gateway rejects a malformed entry at load; this file refuses it on write, so
// the operator sees the reason at once:
//
//   - The fields are lists of positive whole-number ids, at most
//     maxThrottleScopeIDs long. The gateway uses the same bound.
//   - Only a rate_limit entry may carry them. No other type reads them, so on
//     another type they would apply the entry more widely than authored.
//   - `scope.agent_ids` needs exactly one `scope.project_ids` entry. Agent ids
//     are application ids, and application ids are per project.
//   - Every id must exist: each agent in that project's applications table and
//     each user in public.auth_core__user. A throttle on an id that does not
//     exist never matches a request.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// maxThrottleScopeIDs bounds scope.agent_ids and scope.user_ids. It matches
// policy.MaxThrottleScopeIDs in the gateway (a separate Go module).
const maxThrottleScopeIDs = 100

// throttleScope is the parsed throttle part of a scope.
type throttleScope struct {
	projectIDs []int64
	agentIDs   []int64
	userIDs    []int64
}

// parseThrottleScope reads the throttle dimensions of data's scope. It returns
// the zero value when the scope names none.
func parseThrottleScope(data map[string]any) (throttleScope, error) {
	scope, _ := data["scope"].(map[string]any)
	if scope == nil {
		return throttleScope{}, nil
	}
	agents, err := throttleIDList(scope, "agent_ids")
	if err != nil {
		return throttleScope{}, err
	}
	users, err := throttleIDList(scope, "user_ids")
	if err != nil {
		return throttleScope{}, err
	}
	var projects []int64
	if raw, ok := scope["project_ids"].([]any); ok {
		for _, v := range raw {
			if id, ok := wholeID(v); ok {
				projects = append(projects, id)
			}
		}
	}
	return throttleScope{projectIDs: projects, agentIDs: agents, userIDs: users}, nil
}

// throttleIDList reads one list strictly: a bad entry is an error, never a
// dropped id.
func throttleIDList(scope map[string]any, key string) ([]int64, error) {
	value, present := scope[key]
	if !present || value == nil {
		return nil, nil
	}
	raw, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("scope.%s must be a list of ids", key)
	}
	if len(raw) > maxThrottleScopeIDs {
		return nil, fmt.Errorf("scope.%s names %d ids; the limit is %d. Use a project rate limit instead",
			key, len(raw), maxThrottleScopeIDs)
	}
	out := make([]int64, 0, len(raw))
	seen := map[int64]bool{}
	for _, v := range raw {
		id, ok := wholeID(v)
		if !ok {
			return nil, fmt.Errorf("scope.%s entry %v is not a positive whole-number id", key, v)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// wholeID reads a positive whole-number id from a JSON number or a numeric
// string.
func wholeID(v any) (int64, bool) {
	switch n := v.(type) {
	case string:
		id, err := strconv.ParseInt(n, 10, 64)
		return id, err == nil && id > 0
	default:
		f, ok := toFloat(v)
		if !ok || f != math.Trunc(f) || f < 1 || f > 1<<53 {
			return 0, false
		}
		return int64(f), true
	}
}

// validateThrottleScope checks the throttle scope's shape. It needs no
// database; checkThrottleIDsExist does the rest.
func validateThrottleScope(rowType string, data map[string]any) error {
	ts, err := parseThrottleScope(data)
	if err != nil {
		return err
	}
	if len(ts.agentIDs) == 0 && len(ts.userIDs) == 0 {
		return nil
	}
	if rowType != "rate_limit" {
		return errors.New("scope.agent_ids and scope.user_ids apply to rate_limit entries only")
	}
	if len(ts.agentIDs) > 0 && len(ts.projectIDs) != 1 {
		return errors.New("scope.agent_ids needs exactly one entry in scope.project_ids: the project " +
			"that owns the agents, because agent ids are per project")
	}
	return nil
}

// checkThrottleIDsExist refuses a throttle that names an agent or a user that
// does not exist. It writes the response and returns false on a refusal.
func (h *GovernanceHandler) checkThrottleIDsExist(ctx context.Context, w http.ResponseWriter, data map[string]any) bool {
	ts, err := parseThrottleScope(data)
	if err != nil {
		// validateGovernanceRow already refused this shape.
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	if len(ts.userIDs) > 0 {
		var found int64
		if err := h.db.QueryRow(ctx,
			`SELECT count(*) FROM public.auth_core__user WHERE id = ANY($1::bigint[])`, ts.userIDs,
		).Scan(&found); err != nil {
			writeError(w, http.StatusInternalServerError, "could not check scope.user_ids")
			return false
		}
		if found != int64(len(ts.userIDs)) {
			writeError(w, http.StatusBadRequest, "scope.user_ids names a user that does not exist")
			return false
		}
	}
	if len(ts.agentIDs) > 0 {
		schema, err := tenantschema.QuoteInt(ts.projectIDs[0])
		if err != nil {
			writeError(w, http.StatusBadRequest, "scope.project_ids does not name a valid project")
			return false
		}
		var found int64
		if err := h.db.QueryRow(ctx,
			`SELECT count(*) FROM `+schema+`.applications WHERE id = ANY($1::bigint[])`, ts.agentIDs,
		).Scan(&found); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
				writeError(w, http.StatusBadRequest, "scope.project_ids names a project that does not exist")
				return false
			}
			writeError(w, http.StatusInternalServerError, "could not check scope.agent_ids")
			return false
		}
		if found != int64(len(ts.agentIDs)) {
			writeError(w, http.StatusBadRequest, "scope.agent_ids names an agent that does not exist in that project")
			return false
		}
	}
	return true
}
