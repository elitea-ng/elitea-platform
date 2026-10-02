package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

// ProjectContextRepo reads a project's single Project Context row for the chat
// start path (#946): the row Settings > Project Context writes through
// internal/api/v2/eliteacore's UpdateProjectContext and the
// `project_context_builder` chat module writes through the runtime's own
// builder endpoint, over the SAME tenant `configuration` row
// internal/api/v2/promptcontextreads serves back to that screen.
//
// One row, one meaning: what the user sees on the settings screen is what a
// turn in that project is given.
type ProjectContextRepo struct {
	pool *pgxpool.Pool
}

func NewProjectContextRepo(pool *pgxpool.Pool) *ProjectContextRepo {
	return &ProjectContextRepo{pool: pool}
}

var _ agentexecutionapp.CurrentProjectContextTextResolver = (*ProjectContextRepo)(nil)

// ResolveCurrentProjectContext answers the project's stored context, or the
// zero value when the project has no row at all.
//
// DEFAULTS MATCH THE READ ROUTE, NOT GO'S ZERO VALUE. An absent `enabled` key
// means ENABLED — `parseCurrentProjectContextData`
// (internal/api/v2/promptcontextreads/handler.go) has always read it that way,
// the legacy writer omitted the key, and the admission gate this replaced
// spelled the same default out as `COALESCE(data ->> 'enabled', 'true')`. A
// row written before the toggle existed is therefore injected, which is what
// the screen showing it as on already promises. A MISSING row is a different
// thing and stays Enabled=false with empty content: there is nothing to
// inject, and no toggle state to honour.
//
// A malformed `data` document answers the zero value rather than an error. The
// caller fails open either way (projectcontext.go), so an error here would only
// convert "this project's context is unreadable" into the identical
// no-injection outcome after one more round trip — but it would also make the
// chat log a failure on every turn of a project whose row a legacy writer left
// odd, for a feature the user may not even be using.
func (r *ProjectContextRepo) ResolveCurrentProjectContext(
	ctx context.Context,
	projectID int64,
) (agentexecutionapp.CurrentProjectContext, error) {
	if r == nil || r.pool == nil {
		return agentexecutionapp.CurrentProjectContext{}, errors.New("project context: no pool")
	}
	if projectID <= 0 {
		return agentexecutionapp.CurrentProjectContext{}, errors.New("project context: invalid project id")
	}
	schema, err := tenantSchema(fmt.Sprintf("%d", projectID))
	if err != nil {
		return agentexecutionapp.CurrentProjectContext{}, err
	}
	var data []byte
	query := fmt.Sprintf(
		`SELECT data FROM %s.configuration WHERE project_id = $1 AND type = 'project_context' LIMIT 1`,
		schema,
	)
	if err := r.pool.QueryRow(ctx, query, projectID).Scan(&data); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return agentexecutionapp.CurrentProjectContext{}, nil
		}
		return agentexecutionapp.CurrentProjectContext{}, fmt.Errorf("project context: read: %w", err)
	}
	return decodeProjectContextData(data), nil
}

// decodeProjectContextData is the parse half, split out so it can be tested
// without a database — the shapes that matter are all shapes of the stored
// JSON, not of the query.
//
// The `enabled` key is read with the SAME leniency the screen's own read route
// applies (parseCurrentPydanticBool, internal/api/v2/promptcontextreads/
// handler.go): the legacy writer was a Pydantic model and stored booleans as
// `true`, `"true"`, `"on"`, `1` and friends, and a row the settings screen
// renders as ON must not be silently left out of the prompt because this
// reader was stricter than that screen. Anything it cannot read as a boolean
// keeps the enabled-by-default answer, matching the route.
func decodeProjectContextData(data []byte) agentexecutionapp.CurrentProjectContext {
	if len(data) == 0 {
		return agentexecutionapp.CurrentProjectContext{}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return agentexecutionapp.CurrentProjectContext{}
	}
	resolved := agentexecutionapp.CurrentProjectContext{Enabled: true}
	if raw, found := fields["content"]; found {
		var content string
		if err := json.Unmarshal(raw, &content); err == nil {
			resolved.Content = content
		}
	}
	if raw, found := fields["enabled"]; found {
		if enabled, ok := decodeProjectContextBool(raw); ok {
			resolved.Enabled = enabled
		}
	}
	return resolved
}

// decodeProjectContextBool accepts the boolean spellings the legacy Pydantic
// writer produced, in the same set the read route accepts.
func decodeProjectContextBool(raw json.RawMessage) (bool, bool) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return false, false
	}
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		switch strings.ToLower(typed) {
		case "1", "on", "t", "true", "y", "yes":
			return true, true
		case "0", "off", "f", "false", "n", "no":
			return false, true
		}
		return false, false
	case json.Number:
		switch typed.String() {
		case "0":
			return false, true
		case "1":
			return true, true
		}
		return false, false
	}
	return false, false
}
