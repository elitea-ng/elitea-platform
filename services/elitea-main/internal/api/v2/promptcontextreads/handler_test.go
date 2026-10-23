package promptcontextreads

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

type currentChatVault struct {
	project map[string]centrysecrets.Secret
	admin   map[string]centrysecrets.Secret
	invalid map[string]bool
}

func (vault *currentChatVault) Lookup(string) (centrysecrets.Secret, error) {
	return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
}
func (vault *currentChatVault) LookupRegular(string) (centrysecrets.Secret, error) {
	return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
}
func (vault *currentChatVault) LookupProjectID(string) (centrysecrets.Secret, error) {
	return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
}
func (vault *currentChatVault) LookupRegularProjectID(string) (centrysecrets.Secret, error) {
	return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
}
func (vault *currentChatVault) LookupRegularInteger(string) (centrysecrets.Secret, error) {
	return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
}
func (vault *currentChatVault) LookupPythonInteger(name string) (centrysecrets.Secret, error) {
	if vault.invalid[name] {
		return centrysecrets.Secret{}, centrysecrets.ErrInvalidSecret
	}
	if secret, found := vault.project[name]; found {
		return secret, nil
	}
	return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
}
func (vault *currentChatVault) LookupRegularPythonInteger(name string) (centrysecrets.Secret, error) {
	if vault.invalid[name] {
		return centrysecrets.Secret{}, centrysecrets.ErrInvalidSecret
	}
	if secret, found := vault.admin[name]; found {
		return secret, nil
	}
	return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
}

type currentChatVaultLoader struct {
	project      storage.SecretVault
	admin        storage.SecretVault
	projectErr   error
	adminErr     error
	projectCalls int
	adminCalls   int
}

func (loader *currentChatVaultLoader) LoadProjectVault(
	context.Context,
	int64,
) (storage.SecretVault, error) {
	loader.projectCalls++
	return loader.project, loader.projectErr
}

func (loader *currentChatVaultLoader) LoadAdminVault(
	context.Context,
) (storage.SecretVault, error) {
	loader.adminCalls++
	return loader.admin, loader.adminErr
}

func TestCurrentChatConfigReaderPreservesDefaultsPrecedenceAndBigIntegers(t *testing.T) {
	project := &currentChatVault{project: map[string]centrysecrets.Secret{
		"chat_max_upload_count": {
			Value:  "123456789012345678901234567890",
			Hidden: false,
		},
		"chat_max_image_upload_count": {Value: "17", Hidden: true},
	}}
	admin := &currentChatVault{admin: map[string]centrysecrets.Secret{
		"chat_max_upload_count":       {Value: "99"},
		"chat_max_upload_size_mb":     {Value: "250"},
		"chat_max_image_upload_count": {Value: "22"},
	}}
	loader := &currentChatVaultLoader{project: project, admin: admin}
	reader, err := NewCurrentChatConfigVaultReader(loader)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reader.GetCurrentChatConfig(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if result.ChatMaxUploadCount.String() != "123456789012345678901234567890" ||
		result.ChatMaxUploadSizeMB.String() != "250" ||
		result.ChatMaxFileUploadSizeMB.String() != "150" ||
		result.ChatMaxImageUploadCount.String() != "17" ||
		result.ChatMaxImageUploadSizeMB.String() != "3" {
		t.Fatalf("result=%+v", result)
	}
	if loader.adminCalls != 1 || loader.projectCalls != 1 {
		t.Fatalf("vault loads admin=%d project=%d", loader.adminCalls, loader.projectCalls)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"chat_max_upload_count":123456789012345678901234567890,"chat_max_upload_size_mb":250,"chat_max_file_upload_size_mb":150,"chat_max_image_upload_count":17,"chat_max_image_upload_size_mb":3}` {
		t.Fatalf("encoded=%s", encoded)
	}
}

func TestCurrentChatConfigMaximumPython312ResponseIsBounded(t *testing.T) {
	const pythonMaxDigits = 4300
	maximum := "-" + strings.Repeat("9", pythonMaxDigits)
	projectValues := make(map[string]centrysecrets.Secret, len(currentChatIntegerDefaults))
	for _, item := range currentChatIntegerDefaults {
		projectValues[item.name] = centrysecrets.Secret{Value: maximum}
	}
	reader, err := NewCurrentChatConfigVaultReader(&currentChatVaultLoader{
		project: &currentChatVault{project: projectValues},
		admin:   &currentChatVault{},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := reader.GetCurrentChatConfig(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	zeroEncoded, err := json.Marshal(CurrentChatConfig{
		ChatMaxUploadCount:       json.Number("0"),
		ChatMaxUploadSizeMB:      json.Number("0"),
		ChatMaxFileUploadSizeMB:  json.Number("0"),
		ChatMaxImageUploadCount:  json.Number("0"),
		ChatMaxImageUploadSizeMB: json.Number("0"),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantBytes := len(zeroEncoded) - len(currentChatIntegerDefaults) +
		len(currentChatIntegerDefaults)*(pythonMaxDigits+1)
	if len(encoded) != wantBytes {
		t.Fatalf("maximum response bytes=%d want=%d", len(encoded), wantBytes)
	}
}

func TestCurrentChatConfigReaderFailsOnPresentMalformedOrUnavailableVault(t *testing.T) {
	tests := []struct {
		name   string
		loader *currentChatVaultLoader
	}{
		{
			name: "malformed project winner",
			loader: &currentChatVaultLoader{
				project: &currentChatVault{invalid: map[string]bool{
					"chat_max_upload_count": true,
				}},
				admin: &currentChatVault{admin: map[string]centrysecrets.Secret{
					"chat_max_upload_count": {Value: "99"},
				}},
			},
		},
		{
			name: "missing admin vault",
			loader: &currentChatVaultLoader{
				adminErr: errors.New("secret-canary"),
			},
		},
		{
			name: "missing project vault",
			loader: &currentChatVaultLoader{
				admin:      &currentChatVault{},
				projectErr: errors.New("secret-canary"),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader, err := NewCurrentChatConfigVaultReader(test.loader)
			if err != nil {
				t.Fatal(err)
			}
			_, err = reader.GetCurrentChatConfig(context.Background(), 7)
			if !errors.Is(err, ErrCurrentChatConfigUnavailable) ||
				containsError(err, "secret-canary") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCurrentProjectContextDataMatchesCurrentPydanticProjection(t *testing.T) {
	tests := []struct {
		name        string
		data        string
		wantContent string
		wantEnabled bool
		wantError   bool
	}{
		{name: "empty object defaults", data: "{}", wantEnabled: true},
		{name: "content only", data: `{"content":"Project context"}`, wantContent: "Project context", wantEnabled: true},
		{name: "boolean false", data: `{"enabled":false}`, wantEnabled: false},
		{name: "pydantic string false", data: `{"enabled":"off"}`, wantEnabled: false},
		{name: "pydantic numeric true", data: `{"enabled":1.0}`, wantEnabled: true},
		{name: "sql null corrupt", data: "", wantError: true},
		{name: "json null corrupt", data: "null", wantError: true},
		{name: "false scalar corrupt", data: "false", wantError: true},
		{name: "zero scalar corrupt", data: "0", wantError: true},
		{name: "empty string corrupt", data: `""`, wantError: true},
		{name: "truthy string corrupt", data: `"context"`, wantError: true},
		{name: "empty list corrupt", data: "[]", wantError: true},
		{name: "truthy list corrupt", data: "[1]", wantError: true},
		{name: "null content corrupt", data: `{"content":null}`, wantError: true},
		{name: "invalid boolean corrupt", data: `{"enabled":"truthy"}`, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content, enabled, err := parseCurrentProjectContextData([]byte(test.data))
			if test.wantError {
				if !errors.Is(err, ErrCurrentProjectContextCorrupt) {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil || content != test.wantContent || enabled != test.wantEnabled {
				t.Fatalf("content=%q enabled=%t error=%v", content, enabled, err)
			}
		})
	}
}

func TestCurrentProjectContextFormatsNaivePydanticDatetime(t *testing.T) {
	if got := formatCurrentNaiveDateTime(time.Date(2026, 7, 27, 13, 14, 15, 0, time.UTC)); got != "2026-07-27T13:14:15" {
		t.Fatalf("whole second=%q", got)
	}
	if got := formatCurrentNaiveDateTime(time.Date(2026, 7, 27, 13, 14, 15, 120000000, time.FixedZone("offset", 7200))); got != "2026-07-27T13:14:15.120000" {
		t.Fatalf("microseconds=%q", got)
	}
}

type currentChatReaderStub struct {
	result    CurrentChatConfig
	err       error
	projectID int64
	calls     int
}

func (reader *currentChatReaderStub) GetCurrentChatConfig(
	_ context.Context,
	projectID int64,
) (CurrentChatConfig, error) {
	reader.calls++
	reader.projectID = projectID
	return reader.result, reader.err
}

type currentProjectContextReaderStub struct {
	result    CurrentProjectContext
	err       error
	projectID int64
	calls     int
}

func (reader *currentProjectContextReaderStub) GetCurrentProjectContext(
	_ context.Context,
	projectID int64,
) (CurrentProjectContext, error) {
	reader.calls++
	reader.projectID = projectID
	return reader.result, reader.err
}

func TestCurrentRoutesPreservePathsPermissionsAndResponseShapes(t *testing.T) {
	if CurrentChatConfigPath != "/api/v2/elitea_core/chat_config/prompt_lib/{projectID}" ||
		CurrentProjectContextPath != "/api/v2/elitea_core/project_context/prompt_lib/{projectID}/project-context" ||
		CurrentPromptContextMode != auth.PermissionModeDefault ||
		CurrentChatConfigPermission != "models.chat.conversation.details" ||
		CurrentProjectContextPermission != "models.project_context.view" ||
		CurrentProjectContextTimeout != 5*time.Second {
		t.Fatal("current prompt-context route contract drifted")
	}

	updated := "2026-07-27T13:14:15.120000"
	id := int32(19)
	chat := &currentChatReaderStub{result: CurrentChatConfig{
		ChatMaxUploadCount:       json.Number("10"),
		ChatMaxUploadSizeMB:      json.Number("150"),
		ChatMaxFileUploadSizeMB:  json.Number("150"),
		ChatMaxImageUploadCount:  json.Number("10"),
		ChatMaxImageUploadSizeMB: json.Number("3"),
	}}
	project := &currentProjectContextReaderStub{result: CurrentProjectContext{
		ID: &id, Content: "Context", Enabled: true, UpdatedAt: &updated,
	}}
	permissions := currentPermissionResolverFunc(func(
		_ context.Context,
		_ auth.User,
		mode string,
		projectID string,
	) (auth.PermissionResolution, error) {
		if mode != auth.PermissionModeDefault || projectID != "7" {
			t.Fatalf("mode=%q project=%q", mode, projectID)
		}
		return auth.PermissionResolution{
			UserID: 11,
			Permissions: []string{
				CurrentChatConfigPermission,
				CurrentProjectContextPermission,
			},
		}, nil
	})
	routes := newCurrentRoutesForTest(t, chat, project, permissions)

	for _, test := range []struct {
		path string
		want string
	}{
		{
			path: "/api/v2/elitea_core/chat_config/prompt_lib/007",
			want: `{"chat_max_upload_count":10,"chat_max_upload_size_mb":150,"chat_max_file_upload_size_mb":150,"chat_max_image_upload_count":10,"chat_max_image_upload_size_mb":3}` + "\n",
		},
		{
			path: "/api/v2/elitea_core/project_context/prompt_lib/007/project-context",
			want: `{"id":19,"content":"Context","enabled":true,"updated_at":"2026-07-27T13:14:15.120000"}` + "\n",
		},
		{
			path: "/api/v2/elitea_core/chat_config/prompt_lib/٠٠٧",
			want: `{"chat_max_upload_count":10,"chat_max_upload_size_mb":150,"chat_max_file_upload_size_mb":150,"chat_max_image_upload_count":10,"chat_max_image_upload_size_mb":3}` + "\n",
		},
		{
			path: "/api/v2/elitea_core/project_context/prompt_lib/००७/project-context",
			want: `{"id":19,"content":"Context","enabled":true,"updated_at":"2026-07-27T13:14:15.120000"}` + "\n",
		},
	} {
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, currentRequest(http.MethodGet, test.path, true, "10.0.0.8:43120"))
		if response.Code != http.StatusOK || response.Body.String() != test.want {
			t.Fatalf("path=%s status=%d body=%q", test.path, response.Code, response.Body.String())
		}
	}
	if chat.calls != 2 || chat.projectID != 7 || project.calls != 2 || project.projectID != 7 {
		t.Fatalf("chat=%d/%d project=%d/%d", chat.calls, chat.projectID, project.calls, project.projectID)
	}
}

func TestCurrentRoutesAuthenticateAndAuthorizeBeforeReads(t *testing.T) {
	for _, test := range []struct {
		name        string
		path        string
		auth        bool
		trusted     bool
		permissions []string
		want        int
	}{
		{name: "missing auth", path: "/api/v2/elitea_core/chat_config/prompt_lib/7", trusted: true, want: http.StatusUnauthorized},
		{name: "forged X-Auth-* identity without a session", path: "/api/v2/elitea_core/chat_config/prompt_lib/7", want: http.StatusUnauthorized},
		{name: "chat permission denied", path: "/api/v2/elitea_core/chat_config/prompt_lib/7", auth: true, trusted: true, permissions: []string{CurrentProjectContextPermission}, want: http.StatusForbidden},
		{name: "context permission denied", path: "/api/v2/elitea_core/project_context/prompt_lib/7/project-context", auth: true, trusted: true, permissions: []string{CurrentChatConfigPermission}, want: http.StatusForbidden},
		{name: "invalid converter path", path: "/api/v2/elitea_core/chat_config/prompt_lib/not-an-id", auth: true, trusted: true, permissions: []string{CurrentChatConfigPermission}, want: http.StatusNotFound},
		{name: "unicode numeric but non-decimal path", path: "/api/v2/elitea_core/project_context/prompt_lib/²/project-context", auth: true, trusted: true, permissions: []string{CurrentProjectContextPermission}, want: http.StatusNotFound},
		{name: "overflowing decimal path", path: "/api/v2/elitea_core/chat_config/prompt_lib/9223372036854775808", auth: true, trusted: true, permissions: []string{CurrentChatConfigPermission}, want: http.StatusNotFound},
		{name: "wrong mode", path: "/api/v2/elitea_core/chat_config/default/7", auth: true, trusted: true, permissions: []string{CurrentChatConfigPermission}, want: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			chat := &currentChatReaderStub{}
			project := &currentProjectContextReaderStub{}
			permissions := currentPermissionResolverFunc(func(
				context.Context,
				auth.User,
				string,
				string,
			) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{UserID: 11, Permissions: test.permissions}, nil
			})
			routes, err := NewCurrentRoutes(
				chat,
				project,
				apimw.AuthConfig{
					PrincipalValidator: currentPrincipalValidatorFunc(
						func(_ context.Context, user auth.User) (auth.User, error) {
							return user, nil
						},
					),
					FormSessions: authtest.FormSessions{},
				},
				permissions,
			)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			routes.ServeHTTP(response, currentRequest(http.MethodGet, test.path, test.auth, "10.0.0.8:43120"))
			if response.Code != test.want || chat.calls != 0 || project.calls != 0 {
				t.Fatalf("status=%d want=%d chat=%d context=%d body=%q", response.Code, test.want, chat.calls, project.calls, response.Body.String())
			}
		})
	}
}

func TestCurrentPromptContextCompositionFailsClosed(t *testing.T) {
	if _, err := NewCurrentChatConfigVaultReader(nil); err == nil {
		t.Fatal("nil vault loader was accepted")
	}
	if _, err := NewCurrentProjectContextRepository(nil); err == nil {
		t.Fatal("nil database was accepted")
	}
	var chatReader *CurrentChatConfigVaultReader
	if _, err := chatReader.GetCurrentChatConfig(context.Background(), 7); !errors.Is(err, ErrInvalidCurrentChatConfigRequest) {
		t.Fatalf("nil chat reader error=%v", err)
	}
	var contextReader *CurrentProjectContextRepository
	if _, err := contextReader.GetCurrentProjectContext(context.Background(), 7); !errors.Is(err, ErrInvalidCurrentProjectContextRequest) {
		t.Fatalf("nil project-context reader error=%v", err)
	}

	chat := &currentChatReaderStub{}
	project := &currentProjectContextReaderStub{}
	principal := currentPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
		return user, nil
	})
	permissions := currentPermissionResolverFunc(func(
		context.Context,
		auth.User,
		string,
		string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{}, nil
	})
	authConfig := apimw.AuthConfig{
		PrincipalValidator: principal,
		FormSessions:       authtest.FormSessions{},
	}
	for name, test := range map[string]struct {
		chat        CurrentChatConfigReader
		project     CurrentProjectContextReader
		authConfig  apimw.AuthConfig
		permissions auth.PermissionResolver
	}{
		"missing chat reader":    {project: project, authConfig: authConfig, permissions: permissions},
		"missing context reader": {chat: chat, authConfig: authConfig, permissions: permissions},
		"missing RBAC resolver":  {chat: chat, project: project, authConfig: authConfig},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewCurrentRoutes(
				test.chat,
				test.project,
				test.authConfig,
				test.permissions,
			); !errors.Is(err, ErrInvalidCurrentPromptContextRoute) {
				t.Fatalf("error=%v", err)
			}
		})
	}

	var routes *CurrentRoutes
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, CurrentChatConfigPath, nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("nil route status=%d", response.Code)
	}
}

// An OIDC-only deployment has no FormGraph, so PrincipalValidator and
// FormSessions are both nil and its only credential is the
// session cookie. NewCurrentRoutes must COMPOSE there — requiring the two
// validators is what kept the chat-config route unservable in the E2E stack
// and every other SSO-only install (#194) — and it must still refuse to read
// anything for a request that carries no session.
//
// The second case is the security half of that relaxation: a Form session
// cookie must NOT authenticate a deployment that composed no Form session
// reader, and no reader may be touched.
func TestCurrentRoutesComposeWithSessionOnlyAuthAndStillRejectUnauthenticated(t *testing.T) {
	for _, test := range []struct {
		name          string
		authenticated bool
	}{
		{name: "no credential at all"},
		{name: "a Form cookie with no Form reader composed", authenticated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			chat := &currentChatReaderStub{}
			project := &currentProjectContextReaderStub{}
			permissions := currentPermissionResolverFunc(func(
				context.Context,
				auth.User,
				string,
				string,
			) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{UserID: 11, Permissions: []string{CurrentChatConfigPermission}}, nil
			})
			routes, err := NewCurrentRoutes(
				chat,
				project,
				apimw.AuthConfig{SessionSecret: "e2e-session-secret"},
				permissions,
			)
			if err != nil {
				t.Fatalf("session-only composition rejected: %v", err)
			}
			response := httptest.NewRecorder()
			routes.ServeHTTP(response, currentRequest(
				http.MethodGet,
				"/api/v2/elitea_core/chat_config/prompt_lib/7",
				test.authenticated,
				"10.0.0.8:43120",
			))
			if response.Code != http.StatusUnauthorized || chat.calls != 0 || project.calls != 0 {
				t.Fatalf("status=%d chat=%d context=%d body=%q", response.Code, chat.calls, project.calls, response.Body.String())
			}
		})
	}
}

func newCurrentRoutesForTest(
	t *testing.T,
	chat CurrentChatConfigReader,
	project CurrentProjectContextReader,
	permissions auth.PermissionResolver,
) *CurrentRoutes {
	t.Helper()
	routes, err := NewCurrentRoutes(
		chat,
		project,
		apimw.AuthConfig{
			PrincipalValidator: currentPrincipalValidatorFunc(
				func(_ context.Context, user auth.User) (auth.User, error) {
					return user, nil
				},
			),
			FormSessions: authtest.FormSessions{},
		},
		permissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	return routes
}

func currentRequest(method, path string, authenticated bool, remoteAddr string) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	request.RemoteAddr = remoteAddr
	if authenticated {
		authtest.SignIn(request, "11")
	} else {
		// No credential, but the forged X-Auth-* identity the retired edge
		// projection used: it must authenticate nobody (#1359).
		authtest.ForgeXAuth(request, "11")
	}
	return request
}

type currentPermissionResolverFunc func(
	context.Context,
	auth.User,
	string,
	string,
) (auth.PermissionResolution, error)

func (function currentPermissionResolverFunc) ResolvePermissions(
	ctx context.Context,
	user auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	return function(ctx, user, mode, projectID)
}

type currentPrincipalValidatorFunc func(context.Context, auth.User) (auth.User, error)

func (function currentPrincipalValidatorFunc) ValidatePrincipal(
	ctx context.Context,
	user auth.User,
) (auth.User, error) {
	return function(ctx, user)
}

func containsError(err error, text string) bool {
	return err != nil && text != "" && strings.Contains(err.Error(), text)
}

// TestCurrentChatConfigReaderReadsAnAbsentVaultAsDefaults covers a fresh
// deployment: no admin vault, and no project vault until something writes a
// secret. Both loads answer storage.ErrVaultAbsent.
//
// That used to fail the read, so the chat configuration was unavailable on a
// clean install even though all five values have built-in defaults for exactly
// this case. A vault that EXISTS and will not open still fails: its values are
// there and unread.
func TestCurrentChatConfigReaderReadsAnAbsentVaultAsDefaults(t *testing.T) {
	loader := &currentChatVaultLoader{
		projectErr: storage.ErrVaultAbsent,
		adminErr:   storage.ErrVaultAbsent,
	}
	reader, err := NewCurrentChatConfigVaultReader(loader)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reader.GetCurrentChatConfig(context.Background(), 7)
	if err != nil {
		t.Fatalf("an absent vault must read as the defaults: %v", err)
	}
	if result.ChatMaxUploadCount.String() != "10" ||
		result.ChatMaxUploadSizeMB.String() != "150" ||
		result.ChatMaxFileUploadSizeMB.String() != "150" ||
		result.ChatMaxImageUploadCount.String() != "10" ||
		result.ChatMaxImageUploadSizeMB.String() != "3" {
		t.Fatalf("result=%+v", result)
	}
}

func TestCurrentChatConfigReaderStillFailsOnAnUnreadableVault(t *testing.T) {
	loader := &currentChatVaultLoader{adminErr: storage.ErrContentUnavailable}
	reader, err := NewCurrentChatConfigVaultReader(loader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.GetCurrentChatConfig(context.Background(), 7); !errors.Is(
		err, ErrCurrentChatConfigUnavailable,
	) {
		t.Fatalf("err=%v", err)
	}
}
