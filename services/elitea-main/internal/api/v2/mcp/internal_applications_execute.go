package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	applicationsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applications"
	eliteacoreapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

const maxInternalApplicationResultBytes = 1 << 20

type internalApplicationExecution struct {
	status int
	body   []byte
}

type internalApplicationExecutor interface {
	Execute(
		context.Context,
		int64,
		int64,
		internalApplicationOperation,
		map[string]any,
	) (internalApplicationExecution, error)
}

type postgresInternalApplicationExecutor struct {
	applications *applicationsapi.Handler
	eliteacore   *eliteacoreapi.Handler
	pool         *pgxpool.Pool
}

func newPostgresInternalApplicationExecutor(pool *pgxpool.Pool) internalApplicationExecutor {
	if pool == nil {
		return nil
	}
	return &postgresInternalApplicationExecutor{
		applications: applicationsapi.NewHandler(repos.NewApplicationsRepo(pool), pool),
		eliteacore:   eliteacoreapi.NewHandler(pool),
		pool:         pool,
	}
}

func (executor *postgresInternalApplicationExecutor) Execute(
	ctx context.Context,
	projectID int64,
	actorID int64,
	operation internalApplicationOperation,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	if executor == nil || executor.pool == nil || executor.applications == nil || executor.eliteacore == nil {
		return internalApplicationExecution{}, errors.New("internal application executor is unavailable")
	}
	if ctx == nil || projectID <= 0 || actorID <= 0 {
		return internalApplicationExecution{}, errors.New("internal application request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return internalApplicationExecution{}, err
	}

	project := strconv.FormatInt(projectID, 10)
	switch operation {
	case internalListApplications:
		actor := strconv.FormatInt(actorID, 10)
		ctx = auth.ContextWithUser(ctx, auth.User{ID: actor, UserID: actor})
		query := url.Values{}
		for _, name := range []string{"query", "agents_type", "limit", "offset", "tags", "author_id", "statuses", "my_liked", "ids", "without_tags", "trend_start_period", "trend_end_period", "sort_by", "sort_order"} {
			if value, present := arguments[name]; present {
				valid := false
				switch name {
				case "my_liked", "without_tags":
					_, valid = value.(bool)
				case "author_id", "limit", "offset":
					switch value.(type) {
					case json.Number, int, int32, int64, float64:
						valid = true
					}
				default:
					_, valid = value.(string)
				}
				if !valid {
					return jsonExecution(http.StatusBadRequest, map[string]any{"error": "invalid " + name + " argument type"})
				}
				query.Set(name, scalarArgument(value))
			}
		}
		return invokeInternalHandler(ctx, http.MethodGet, query, nil,
			map[string]string{"projectID": project}, executor.applications.List)
	case internalCreateApplication:
		return invokeInternalHandler(ctx, http.MethodPost, nil, bodyWithout(arguments, "project_id"),
			map[string]string{"projectID": project}, executor.applications.Create)
	case internalGetApplication:
		applicationID, err := requiredPositiveID(arguments, "application_id")
		if err != nil {
			return internalApplicationExecution{}, err
		}
		return executor.getApplication(ctx, project, applicationID, stringArgument(arguments["version_name"]))
	case internalCreateVersion:
		applicationID, err := requiredPositiveID(arguments, "application_id")
		if err != nil {
			return internalApplicationExecution{}, err
		}
		return invokeInternalHandler(ctx, http.MethodPost, nil,
			bodyWithout(arguments, "project_id", "application_id"),
			map[string]string{"projectID": project, "applicationID": applicationID},
			executor.applications.CreateVersion)
	case internalGetVersion:
		applicationID, versionID, err := applicationVersionIDs(arguments)
		if err != nil {
			return internalApplicationExecution{}, err
		}
		result, invokeErr := invokeInternalHandler(ctx, http.MethodGet, nil, nil,
			map[string]string{"projectID": project, "applicationID": applicationID, "versionID": versionID},
			executor.applications.GetVersion)
		return addInstructionsHash(result, invokeErr)
	case internalUpdateVersion:
		return executor.updateVersion(ctx, projectID, actorID, arguments)
	case internalPatchInstructions:
		return executor.patchInstructions(ctx, projectID, actorID, arguments)
	case internalUpdateAgentRelation:
		childApplicationID, childVersionID, err := applicationVersionIDs(arguments)
		if err != nil {
			return internalApplicationExecution{}, err
		}
		parentApplicationID, err := requiredPositiveID(arguments, "entity_id")
		if err != nil {
			return internalApplicationExecution{}, err
		}
		parentVersionID, err := requiredPositiveID(arguments, "entity_version_id")
		if err != nil {
			return internalApplicationExecution{}, err
		}
		body := map[string]any{
			"application_id": parentApplicationID,
			"version_id":     parentVersionID,
			"has_relation":   arguments["has_relation"],
		}
		return invokeInternalHandler(ctx, http.MethodPatch, nil, body,
			map[string]string{
				"projectID": project, "appID": childApplicationID, "versionID": childVersionID,
			}, executor.eliteacore.UpdateApplicationRelation)
	default:
		return internalApplicationExecution{}, errors.New("unknown internal application operation")
	}
}

func (executor *postgresInternalApplicationExecutor) getApplication(
	ctx context.Context,
	projectID string,
	applicationID string,
	versionName string,
) (internalApplicationExecution, error) {
	result, err := invokeInternalHandler(ctx, http.MethodGet, nil, nil,
		map[string]string{"projectID": projectID, "applicationID": applicationID},
		executor.applications.Get)
	if err != nil || result.status >= http.StatusBadRequest {
		return result, err
	}

	var applicationBody map[string]any
	if json.Unmarshal(result.body, &applicationBody) != nil {
		return internalApplicationExecution{}, errors.New("internal application response is invalid")
	}
	schema, ok := projectSchema(projectID)
	if !ok {
		return internalApplicationExecution{}, errors.New("internal application project is invalid")
	}
	versionID, err := executor.resolveApplicationVersion(ctx, schema, applicationID, versionName, applicationBody)
	if err != nil {
		return jsonExecution(http.StatusNotFound, map[string]any{"error": "application version not found"})
	}
	version, err := invokeInternalHandler(ctx, http.MethodGet, nil, nil,
		map[string]string{"projectID": projectID, "applicationID": applicationID, "versionID": versionID},
		executor.applications.GetVersion)
	version, err = addInstructionsHash(version, err)
	if err != nil || version.status >= http.StatusBadRequest {
		return version, err
	}
	var versionBody map[string]any
	if json.Unmarshal(version.body, &versionBody) != nil {
		return internalApplicationExecution{}, errors.New("internal application response is invalid")
	}
	applicationBody["version_details"] = versionBody
	return jsonExecution(http.StatusOK, applicationBody)
}

func (executor *postgresInternalApplicationExecutor) resolveApplicationVersion(
	ctx context.Context,
	schema string,
	applicationID string,
	versionName string,
	applicationBody map[string]any,
) (string, error) {
	if versionName = strings.TrimSpace(versionName); versionName != "" {
		var versionID string
		err := executor.pool.QueryRow(ctx, fmt.Sprintf(`
			SELECT id::text FROM %s.application_versions
			WHERE application_id = $1 AND name = $2`, schema), applicationID, versionName).Scan(&versionID)
		return versionID, err
	}

	if meta, ok := applicationBody["meta"].(map[string]any); ok {
		if candidate := scalarArgument(meta["default_version_id"]); candidate != "" {
			if _, valid := parsePositiveInt64(candidate); valid {
				var versionID string
				err := executor.pool.QueryRow(ctx, fmt.Sprintf(`
					SELECT id::text FROM %s.application_versions
					WHERE application_id = $1 AND id = $2`, schema), applicationID, candidate).Scan(&versionID)
				if err == nil {
					return versionID, nil
				}
				if !errors.Is(err, pgx.ErrNoRows) {
					return "", err
				}
			}
		}
	}

	var versionID string
	err := executor.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT id::text FROM %s.application_versions
		WHERE application_id = $1
		ORDER BY (name = 'base') DESC, created_at DESC, id DESC
		LIMIT 1`, schema), applicationID).Scan(&versionID)
	return versionID, err
}

func addInstructionsHash(
	result internalApplicationExecution,
	err error,
) (internalApplicationExecution, error) {
	if err != nil || result.status >= http.StatusBadRequest {
		return result, err
	}
	var body map[string]any
	if json.Unmarshal(result.body, &body) != nil {
		return internalApplicationExecution{}, errors.New("internal application response is invalid")
	}
	instructions, _ := body["instructions"].(string)
	body["instructions_sha256"] = instructionsSHA256(instructions)
	return jsonExecution(result.status, body)
}

func applicationVersionIDs(arguments map[string]any) (string, string, error) {
	applicationID, err := requiredPositiveID(arguments, "application_id")
	if err != nil {
		return "", "", err
	}
	versionID, err := requiredPositiveID(arguments, "version_id")
	if err != nil {
		return "", "", err
	}
	return applicationID, versionID, nil
}

func requiredPositiveID(arguments map[string]any, name string) (string, error) {
	value, present := arguments[name]
	if !present {
		return "", fmt.Errorf("%s is required", name)
	}
	text := scalarArgument(value)
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil || parsed <= 0 {
		return "", fmt.Errorf("%s must be a positive integer", name)
	}
	return strconv.FormatInt(parsed, 10), nil
}

func scalarArgument(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case int:
		return strconv.Itoa(typed)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return ""
	}
}

func stringArgument(value any) string {
	text, _ := value.(string)
	return text
}

func bodyWithout(arguments map[string]any, names ...string) map[string]any {
	excluded := make(map[string]struct{}, len(names))
	for _, name := range names {
		excluded[name] = struct{}{}
	}
	body := make(map[string]any, len(arguments)-len(excluded))
	for name, value := range arguments {
		if _, skip := excluded[name]; !skip {
			body[name] = value
		}
	}
	return body
}

type boundedInternalResponseWriter struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	overflow bool
}

func (writer *boundedInternalResponseWriter) Header() http.Header {
	if writer.header == nil {
		writer.header = make(http.Header)
	}
	return writer.header
}

func (writer *boundedInternalResponseWriter) WriteHeader(status int) {
	if writer.status == 0 {
		writer.status = status
	}
}

func (writer *boundedInternalResponseWriter) Write(value []byte) (int, error) {
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	remaining := maxInternalApplicationResultBytes - writer.body.Len()
	if remaining <= 0 {
		writer.overflow = true
		return len(value), nil
	}
	if len(value) > remaining {
		_, _ = writer.body.Write(value[:remaining])
		writer.overflow = true
		return len(value), nil
	}
	_, _ = writer.body.Write(value)
	return len(value), nil
}

func invokeInternalHandler(
	ctx context.Context,
	method string,
	query url.Values,
	body map[string]any,
	params map[string]string,
	handler http.HandlerFunc,
) (internalApplicationExecution, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return internalApplicationExecution{}, errors.New("internal application arguments are invalid")
		}
		reader = bytes.NewReader(encoded)
	}
	target := "https://internal.invalid/"
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return internalApplicationExecution{}, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	routeContext := chi.NewRouteContext()
	for name, value := range params {
		routeContext.URLParams.Add(name, value)
	}
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))

	writer := &boundedInternalResponseWriter{}
	handler(writer, request)
	if writer.overflow {
		return internalApplicationExecution{}, errors.New("internal application response is too large")
	}
	status := writer.status
	if status == 0 {
		status = http.StatusOK
	}
	return internalApplicationExecution{status: status, body: bytes.TrimSpace(writer.body.Bytes())}, nil
}

func jsonExecution(status int, value any) (internalApplicationExecution, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return internalApplicationExecution{}, err
	}
	return internalApplicationExecution{status: status, body: encoded}, nil
}
