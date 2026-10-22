package promptcontextreads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	CurrentChatConfigPath           = "/api/v2/elitea_core/chat_config/prompt_lib/{projectID}"
	CurrentProjectContextPath       = "/api/v2/elitea_core/project_context/prompt_lib/{projectID}/project-context"
	CurrentPromptContextMode        = auth.PermissionModeDefault
	CurrentChatConfigPermission     = "models.chat.conversation.details"
	CurrentProjectContextPermission = "models.project_context.view"
	CurrentProjectContextTimeout    = 5 * time.Second
)

const (
	currentChatConfigRoutePath     = "/api/v2/elitea_core/chat_config/prompt_lib/{projectID}"
	currentProjectContextRoutePath = "/api/v2/elitea_core/project_context/prompt_lib/{projectID}/project-context"
)

var (
	ErrInvalidCurrentPromptContextRoute    = errors.New("invalid current prompt-context read route dependencies")
	ErrInvalidCurrentChatConfigRequest     = errors.New("current chat configuration request is invalid")
	ErrInvalidCurrentProjectContextRequest = errors.New("current project context request is invalid")
	ErrCurrentChatConfigUnavailable        = errors.New("current chat configuration is unavailable")
	ErrCurrentProjectContextCorrupt        = errors.New("current project context is corrupt")
)

var currentChatIntegerDefaults = [...]struct {
	name  string
	value string
}{
	{name: "chat_max_upload_count", value: "10"},
	{name: "chat_max_upload_size_mb", value: "150"},
	{name: "chat_max_file_upload_size_mb", value: "150"},
	{name: "chat_max_image_upload_count", value: "10"},
	{name: "chat_max_image_upload_size_mb", value: "3"},
}

// CurrentChatConfig is the exact five-key successful response returned by the
// current chat_config GET. json.Number preserves Python's arbitrary-precision
// JSON integer output without routing values through binary64.
type CurrentChatConfig struct {
	ChatMaxUploadCount       json.Number `json:"chat_max_upload_count"`
	ChatMaxUploadSizeMB      json.Number `json:"chat_max_upload_size_mb"`
	ChatMaxFileUploadSizeMB  json.Number `json:"chat_max_file_upload_size_mb"`
	ChatMaxImageUploadCount  json.Number `json:"chat_max_image_upload_count"`
	ChatMaxImageUploadSizeMB json.Number `json:"chat_max_image_upload_size_mb"`
}

type CurrentChatConfigReader interface {
	GetCurrentChatConfig(context.Context, int64) (CurrentChatConfig, error)
}

type pythonIntegerVault interface {
	LookupPythonInteger(string) (centrysecrets.Secret, error)
	LookupRegularPythonInteger(string) (centrysecrets.Secret, error)
}

// CurrentChatConfigVaultReader reuses the existing encrypted Centry vault
// loader. One request loads exactly the admin and project snapshots, then
// applies current get_all_secrets precedence: admin regular, project hidden,
// project regular. Admin hidden values are never shared.
type CurrentChatConfigVaultReader struct {
	vaults storage.SecretVaultLoader
}

func NewCurrentChatConfigVaultReader(
	vaults storage.SecretVaultLoader,
) (*CurrentChatConfigVaultReader, error) {
	if vaults == nil {
		return nil, errors.New("current chat configuration vault loader is required")
	}
	return &CurrentChatConfigVaultReader{vaults: vaults}, nil
}

// loadChatConfigVault reads an absent vault as an empty one. See the call site.
func loadChatConfigVault(load func() (storage.SecretVault, error)) (storage.SecretVault, error) {
	vault, err := load()
	if errors.Is(err, storage.ErrVaultAbsent) {
		return storage.AbsentSecretVault(), nil
	}
	if err != nil {
		return nil, err
	}
	return vault, nil
}

func (reader *CurrentChatConfigVaultReader) GetCurrentChatConfig(
	ctx context.Context,
	projectID int64,
) (CurrentChatConfig, error) {
	if reader == nil || reader.vaults == nil || ctx == nil || projectID <= 0 {
		return CurrentChatConfig{}, ErrInvalidCurrentChatConfigRequest
	}
	if err := ctx.Err(); err != nil {
		return CurrentChatConfig{}, err
	}

	// An ABSENT vault is not a failure: it is a scope that has stored no
	// secret, which is the state of a fresh deployment, and every one of the
	// five values below already has a built-in default for exactly that case.
	// Refusing it made the chat configuration read 503 on a clean install. A
	// vault that exists and will not open stays a failure.
	adminSnapshot, err := loadChatConfigVault(func() (storage.SecretVault, error) {
		return reader.vaults.LoadAdminVault(ctx)
	})
	if err != nil {
		return CurrentChatConfig{}, ErrCurrentChatConfigUnavailable
	}
	projectSnapshot, err := loadChatConfigVault(func() (storage.SecretVault, error) {
		return reader.vaults.LoadProjectVault(ctx, projectID)
	})
	if err != nil {
		return CurrentChatConfig{}, ErrCurrentChatConfigUnavailable
	}
	adminVault, adminOK := adminSnapshot.(pythonIntegerVault)
	projectVault, projectOK := projectSnapshot.(pythonIntegerVault)
	if !adminOK || !projectOK {
		return CurrentChatConfig{}, ErrCurrentChatConfigUnavailable
	}

	values := [len(currentChatIntegerDefaults)]json.Number{}
	for index, item := range currentChatIntegerDefaults {
		value, lookupErr := lookupCurrentChatInteger(projectVault, adminVault, item.name)
		switch {
		case lookupErr == nil:
			values[index] = json.Number(value)
		case errors.Is(lookupErr, centrysecrets.ErrSecretNotFound):
			values[index] = json.Number(item.value)
		default:
			return CurrentChatConfig{}, ErrCurrentChatConfigUnavailable
		}
	}
	return CurrentChatConfig{
		ChatMaxUploadCount:       values[0],
		ChatMaxUploadSizeMB:      values[1],
		ChatMaxFileUploadSizeMB:  values[2],
		ChatMaxImageUploadCount:  values[3],
		ChatMaxImageUploadSizeMB: values[4],
	}, nil
}

func lookupCurrentChatInteger(
	project, admin pythonIntegerVault,
	name string,
) (string, error) {
	projectSecret, err := project.LookupPythonInteger(name)
	if err == nil {
		return projectSecret.Value, nil
	}
	if !errors.Is(err, centrysecrets.ErrSecretNotFound) {
		return "", err
	}
	adminSecret, err := admin.LookupRegularPythonInteger(name)
	if err != nil {
		return "", err
	}
	return adminSecret.Value, nil
}

var _ CurrentChatConfigReader = (*CurrentChatConfigVaultReader)(nil)

// CurrentProjectContext is the exact current GET presentation. UpdatedAt is a
// preformatted naive timestamp because the current SQLAlchemy column is
// TIMESTAMP WITHOUT TIME ZONE and Pydantic does not append a UTC designator.
type CurrentProjectContext struct {
	ID        *int32  `json:"id"`
	Content   string  `json:"content"`
	Enabled   bool    `json:"enabled"`
	UpdatedAt *string `json:"updated_at"`
}

type CurrentProjectContextReader interface {
	GetCurrentProjectContext(context.Context, int64) (CurrentProjectContext, error)
}

type CurrentProjectContextRepository struct {
	tenants *tenant.Executor
}

func NewCurrentProjectContextRepository(
	pool *pgxpool.Pool,
) (*CurrentProjectContextRepository, error) {
	if pool == nil {
		return nil, errors.New("current project context database is required")
	}
	return &CurrentProjectContextRepository{tenants: tenant.NewExecutor(pool)}, nil
}

func (repository *CurrentProjectContextRepository) GetCurrentProjectContext(
	ctx context.Context,
	projectID int64,
) (CurrentProjectContext, error) {
	if repository == nil || repository.tenants == nil || ctx == nil || projectID <= 0 {
		return CurrentProjectContext{}, ErrInvalidCurrentProjectContextRequest
	}
	if err := ctx.Err(); err != nil {
		return CurrentProjectContext{}, err
	}

	result := CurrentProjectContext{Content: "", Enabled: true}
	queryContext, cancel := context.WithTimeout(ctx, CurrentProjectContextTimeout)
	defer cancel()
	err := repository.tenants.WithinTx(
		queryContext,
		tenant.Project{ID: projectID},
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly},
		func(ctx context.Context, tx pgx.Tx) error {
			var (
				id        int32
				data      []byte
				updatedAt *time.Time
			)
			err := tx.QueryRow(ctx, `
SELECT id, data, updated_at
FROM configuration
WHERE project_id = $1
  AND type = 'project_context'
LIMIT 1`,
				projectID,
			).Scan(&id, &data, &updatedAt)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("get current project context: %w", err)
			}

			result.ID = &id
			if updatedAt != nil {
				formatted := formatCurrentNaiveDateTime(*updatedAt)
				result.UpdatedAt = &formatted
			}
			content, enabled, parseErr := parseCurrentProjectContextData(data)
			if parseErr != nil {
				return parseErr
			}
			result.Content = content
			result.Enabled = enabled
			return nil
		},
	)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CurrentProjectContext{}, ctxErr
		}
		return CurrentProjectContext{}, err
	}
	return result, nil
}

func parseCurrentProjectContextData(data []byte) (string, bool, error) {
	trimmed := bytes.TrimSpace(data)
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return "", false, ErrCurrentProjectContextCorrupt
	}
	if err := expectCurrentJSONEnd(decoder); err != nil {
		return "", false, ErrCurrentProjectContextCorrupt
	}

	content := ""
	if raw, found := fields["content"]; found {
		trimmedContent := bytes.TrimSpace(raw)
		if len(trimmedContent) < 2 || trimmedContent[0] != '"' ||
			json.Unmarshal(trimmedContent, &content) != nil {
			return "", false, ErrCurrentProjectContextCorrupt
		}
	}
	enabled := true
	if raw, found := fields["enabled"]; found {
		var ok bool
		enabled, ok = parseCurrentPydanticBool(raw)
		if !ok {
			return "", false, ErrCurrentProjectContextCorrupt
		}
	}
	return content, enabled, nil
}

func parseCurrentPydanticBool(raw json.RawMessage) (bool, bool) {
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("true")) {
		return true, true
	}
	if bytes.Equal(trimmed, []byte("false")) {
		return false, true
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return false, false
		}
		switch strings.ToLower(value) {
		case "1", "on", "t", "true", "y", "yes":
			return true, true
		case "0", "off", "f", "false", "n", "no":
			return false, true
		default:
			return false, false
		}
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil || expectCurrentJSONEnd(decoder) != nil {
		return false, false
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	switch {
	case err != nil:
		return false, false
	case value == 0:
		return false, true
	case value == 1:
		return true, true
	default:
		return false, false
	}
}

func expectCurrentJSONEnd(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return ErrCurrentProjectContextCorrupt
	}
	return err
}

func formatCurrentNaiveDateTime(value time.Time) string {
	base := value.Format("2006-01-02T15:04:05")
	if microseconds := value.Nanosecond() / 1000; microseconds != 0 {
		return base + fmt.Sprintf(".%06d", microseconds)
	}
	return base
}

var _ CurrentProjectContextReader = (*CurrentProjectContextRepository)(nil)

type CurrentRoutes struct {
	handler http.Handler
}

func NewCurrentRoutes(
	chat CurrentChatConfigReader,
	projectContext CurrentProjectContextReader,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentRoutes, error) {
	// PrincipalValidator and FormSessions are optional — when nil
	// the auth middleware falls back to session-cookie verification, which is
	// the only credential OIDC-only deployments have (no
	// ELITEA_AUTH_CONFIG_FILE, hence no FormGraph). Requiring them made these
	// routes composable ONLY under Form auth, so
	// `GET /api/v2/elitea_core/chat_config/prompt_lib/{projectID}` — the URL
	// `features/artifacts`' chatConfigApi actually calls — could not be served
	// in the E2E stack or in any other OIDC-only deployment (#194). Same
	// relaxation, and the same reason, as
	// notificationsapi.NewCurrentNotificationEventsRoute (#152) and
	// v2projects.NewCurrentProjectListRoute.
	//
	// This does NOT weaken authorization: apimw.Auth still rejects an
	// unauthenticated request, and RequireResolvedPermissionsForProject still
	// resolves models.chat.conversation.details (chat config) or
	// models.project_context.view (project context) against the requested
	// project before either handler runs.
	if chat == nil || projectContext == nil || permissions == nil {
		return nil, ErrInvalidCurrentPromptContextRoute
	}

	authenticate := apimw.Auth(authConfig)
	chatEndpoint := http.Handler(http.HandlerFunc(
		(&currentHandler{chat: chat, projectContext: projectContext}).getChatConfig,
	))
	chatEndpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentPromptContextMode,
		currentPromptProjectID,
		CurrentChatConfigPermission,
	)(chatEndpoint)
	chatEndpoint = authenticate(chatEndpoint)
	chatEndpoint = requireCurrentIntegerConverter(chatEndpoint)

	contextEndpoint := http.Handler(http.HandlerFunc(
		(&currentHandler{chat: chat, projectContext: projectContext}).getProjectContext,
	))
	contextEndpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentPromptContextMode,
		currentPromptProjectID,
		CurrentProjectContextPermission,
	)(contextEndpoint)
	contextEndpoint = authenticate(contextEndpoint)
	contextEndpoint = requireCurrentIntegerConverter(contextEndpoint)

	router := chi.NewRouter()
	router.Method(http.MethodGet, currentChatConfigRoutePath, chatEndpoint)
	router.Method(http.MethodGet, currentProjectContextRoutePath, contextEndpoint)
	router.NotFound(writeCurrentNotFound)
	return &CurrentRoutes{handler: router}, nil
}

func (routes *CurrentRoutes) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if routes == nil || routes.handler == nil {
		http.NotFound(writer, request)
		return
	}
	routes.handler.ServeHTTP(writer, request)
}

type currentHandler struct {
	chat           CurrentChatConfigReader
	projectContext CurrentProjectContextReader
}

func (handler *currentHandler) getChatConfig(writer http.ResponseWriter, request *http.Request) {
	projectID, ok := currentPromptPostgresID(chi.URLParam(request, "projectID"))
	if !ok {
		writeCurrentFailure(writer)
		return
	}
	result, err := handler.chat.GetCurrentChatConfig(request.Context(), projectID)
	if err != nil {
		writeCurrentFailure(writer)
		return
	}
	writeCurrentJSON(writer, http.StatusOK, result)
}

func (handler *currentHandler) getProjectContext(writer http.ResponseWriter, request *http.Request) {
	projectID, ok := currentPromptPostgresID(chi.URLParam(request, "projectID"))
	if !ok {
		writeCurrentFailure(writer)
		return
	}
	result, err := handler.projectContext.GetCurrentProjectContext(request.Context(), projectID)
	if err != nil {
		writeCurrentFailure(writer)
		return
	}
	writeCurrentJSON(writer, http.StatusOK, result)
}

func currentPromptProjectID(request *http.Request) (string, bool) {
	projectID, ok := parseCurrentIntegerConverter(chi.URLParam(request, "projectID"))
	if !ok {
		return "", false
	}
	return strconv.FormatInt(projectID, 10), true
}

func currentPromptPostgresID(value string) (int64, bool) {
	projectID, ok := parseCurrentIntegerConverter(value)
	return projectID, ok && projectID > 0
}

func requireCurrentIntegerConverter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if _, ok := parseCurrentIntegerConverter(chi.URLParam(request, "projectID")); !ok {
			writeCurrentNotFound(writer, request)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func parseCurrentIntegerConverter(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}
	var result int64
	for _, candidate := range value {
		digit, ok := currentDecimalDigit(candidate)
		if !ok || result > (math.MaxInt64-digit)/10 {
			return 0, false
		}
		result = result*10 + digit
	}
	return result, true
}

func currentDecimalDigit(value rune) (int64, bool) {
	for _, range16 := range unicode.Digit.R16 {
		if value < rune(range16.Lo) || value > rune(range16.Hi) ||
			(value-rune(range16.Lo))%rune(range16.Stride) != 0 {
			continue
		}
		return int64(((value - rune(range16.Lo)) / rune(range16.Stride)) % 10), true
	}
	for _, range32 := range unicode.Digit.R32 {
		if value < rune(range32.Lo) || value > rune(range32.Hi) ||
			(value-rune(range32.Lo))%rune(range32.Stride) != 0 {
			continue
		}
		return int64(((value - rune(range32.Lo)) / rune(range32.Stride)) % 10), true
	}
	return 0, false
}

func writeCurrentFailure(writer http.ResponseWriter) {
	writeCurrentJSON(writer, http.StatusInternalServerError, map[string]string{
		"message": "Internal Server Error",
	})
}

func writeCurrentNotFound(writer http.ResponseWriter, _ *http.Request) {
	writeCurrentJSON(writer, http.StatusNotFound, map[string]string{
		"message": "The requested URL was not found on the server. If you entered the URL manually please check your spelling and try again.",
	})
}

func writeCurrentJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
