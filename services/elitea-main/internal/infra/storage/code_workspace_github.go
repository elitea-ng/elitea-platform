package storage

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/egress"
)

type CodeRepositoryFile struct {
	File    CodeWorkspaceFile
	Content []byte
}

// Capability implementations retain credentials only inside Main acquisition.
type CodeRepositoryCapability interface {
	Preflight(context.Context, map[string]any) error
	Acquire(context.Context, map[string]any, CodeWorkspaceSelection, CodeWorkspacePolicy) ([]CodeRepositoryFile, error)
}

type CodeRepositoryCapabilities struct {
	byType map[string]CodeRepositoryCapability
}

func NewCodeRepositoryCapabilities(capabilities map[string]CodeRepositoryCapability) (*CodeRepositoryCapabilities, error) {
	if len(capabilities) == 0 || len(capabilities) > 16 {
		return nil, ErrCodeWorkspaceInvalid
	}
	owned := make(map[string]CodeRepositoryCapability, len(capabilities))
	for kind, capability := range capabilities {
		if kind == "" || len(kind) > 64 || capability == nil {
			return nil, ErrCodeWorkspaceInvalid
		}
		owned[kind] = capability
	}
	return &CodeRepositoryCapabilities{owned}, nil
}
func (r *CodeRepositoryCapabilities) capability(kind string) (CodeRepositoryCapability, error) {
	if r == nil {
		return nil, ErrCodeWorkspaceCapability
	}
	c, found := r.byType[kind]
	if !found {
		return nil, ErrCodeWorkspaceCapability
	}
	return c, nil
}

// GitHub supports the existing anonymous, token, and basic read authorities.
// The current GitHub toolkit also refuses App authentication for repository reads.
type CodeWorkspaceGitHub struct {
	allowed *egresslib.Allowlist
	client  *http.Client
}

func NewCodeWorkspaceGitHub(allowed *egresslib.Allowlist) (*CodeWorkspaceGitHub, error) {
	if allowed == nil || !allowed.Configured() {
		return nil, ErrCodeWorkspaceCapability
	}
	// The host allowlist names the GitHub base; the egress guard decides where
	// that name may resolve, re-checks every dial, and never uses a proxy.
	transport := egress.New(allowed).Transport()
	return &CodeWorkspaceGitHub{allowed, &http.Client{Transport: transport, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return ErrCodeWorkspaceUnavailable }}}, nil
}

// Close releases idle connections. Repeated calls are safe.
func (g *CodeWorkspaceGitHub) Close() {
	if g != nil && g.client != nil {
		g.client.CloseIdleConnections()
	}
}

func githubWorkspaceConfig(settings map[string]any) (*url.URL, string, map[string]any, error) {
	configuration, ok := settings["github_configuration"].(map[string]any)
	repository, repositoryOK := settings["repository"].(string)
	base, baseOK := configuration["base_url"].(string)
	parsed, err := url.Parse(base)
	if !ok || !repositoryOK || !baseOK || err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" ||
		parsed.Path != "" && parsed.Path != "/" && parsed.Path != "/api/v3" && parsed.Path != "/api/v3/" {
		return nil, "", nil, ErrCodeWorkspaceCapability
	}
	repository, err = githubWorkspaceRepository(repository)
	if err != nil {
		return nil, "", nil, err
	}
	return parsed, repository, configuration, nil
}

// Normalize only the existing saved toolkit value. It never selects an origin.
func githubWorkspaceRepository(value string) (string, error) {
	if value == "" || len(value) > 512 || strings.ContainsAny(value, "\x00\r\n") {
		return "", ErrCodeWorkspaceCapability
	}
	repository := value
	if strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") {
		parsed, err := url.Parse(value)
		if err != nil || parsed.User != nil || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
			return "", ErrCodeWorkspaceCapability
		}
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) != 2 {
			return "", ErrCodeWorkspaceCapability
		}
		repository = parts[0] + "/" + parts[1]
	} else if strings.HasPrefix(value, "git@") {
		host, path, ok := strings.Cut(strings.TrimPrefix(value, "git@"), ":")
		if !ok || host == "" || strings.ContainsAny(host, " /\\\t") {
			return "", ErrCodeWorkspaceCapability
		}
		repository = path
	}
	repository = strings.TrimSuffix(repository, ".git")
	parts := strings.Split(repository, "/")
	if len(parts) != 2 {
		return "", ErrCodeWorkspaceCapability
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || len(part) > 256 {
			return "", ErrCodeWorkspaceCapability
		}
		for _, b := range []byte(part) {
			if (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (b < '0' || b > '9') && !strings.ContainsRune("._-", rune(b)) {
				return "", ErrCodeWorkspaceCapability
			}
		}
	}
	return repository, nil
}

func (g *CodeWorkspaceGitHub) Preflight(ctx context.Context, settings map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	base, _, _, err := githubWorkspaceConfig(settings)
	if err != nil {
		return err
	}
	if g == nil || g.allowed == nil || !g.allowed.Configured() || !g.allowed.Allows(base.String()) {
		return ErrCodeWorkspaceCapability
	}
	var selected []string
	switch value := settings["selected_tools"].(type) {
	case []any:
		for _, entry := range value {
			tool, ok := entry.(string)
			if !ok {
				return ErrCodeWorkspaceCapability
			}
			selected = append(selected, tool)
		}
	case []string:
		selected = value
	default:
		return ErrCodeWorkspaceCapability
	}
	if len(selected) == 0 || len(selected) > 1024 {
		return ErrCodeWorkspaceCapability
	}
	found := false
	for _, tool := range selected {
		if tool == "" || len(tool) > 1024 || strings.ContainsAny(tool, "\x00\r\n") {
			return ErrCodeWorkspaceCapability
		}
		found = found || tool == "read_file"
	}
	if found {
		return nil
	}
	return ErrCodeWorkspaceCapability
}

func githubWorkspaceAuthorization(configuration map[string]any, request *http.Request) error {
	text := func(key string) (string, error) {
		value, present := configuration[key]
		if !present || value == nil {
			return "", nil
		}
		v, ok := value.(string)
		if !ok || len(v) > 64<<10 || strings.ContainsAny(v, "\r\n\x00") {
			return "", ErrCodeWorkspaceCapability
		}
		return v, nil
	}
	token, err := text("access_token")
	if err != nil {
		return err
	}
	if token != "" {
		request.Header.Set("Authorization", "token "+token)
		return nil
	}
	username, err := text("username")
	if err != nil {
		return err
	}
	password, err := text("password")
	if err != nil {
		return err
	}
	if (username == "") != (password == "") {
		return ErrCodeWorkspaceCapability
	}
	if username != "" {
		request.SetBasicAuth(username, password)
		return nil
	}
	// Do not create a new App credential flow under a workspace request.
	if configuration["app_id"] != nil || configuration["app_private_key"] != nil {
		return ErrCodeWorkspaceCapability
	}
	return nil
}

func (g *CodeWorkspaceGitHub) get(ctx context.Context, base *url.URL, configuration map[string]any, path string, limit uint64, output any) (err error) {
	endpoint := *base
	endpoint.Path = strings.TrimSuffix(base.Path, "/") + "/repos/" + path
	if strings.Contains(path, "?") {
		parts := strings.SplitN(endpoint.Path, "?", 2)
		endpoint.Path, endpoint.RawQuery = parts[0], parts[1]
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return ErrCodeWorkspaceUnavailable
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "elitea-code-workspace")
	if err := githubWorkspaceAuthorization(configuration, request); err != nil {
		return err
	}
	response, err := g.client.Do(request)
	if err != nil {
		return ErrCodeWorkspaceUnavailable
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil && err == nil {
			err = ErrCodeWorkspaceUnavailable
		}
	}()
	if response.StatusCode != http.StatusOK || response.ContentLength > int64(limit) {
		return ErrCodeWorkspaceUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil || uint64(len(body)) > limit || json.Unmarshal(body, output) != nil {
		return ErrCodeWorkspaceUnavailable
	}
	return ctx.Err()
}

func (g *CodeWorkspaceGitHub) Acquire(ctx context.Context, settings map[string]any, selection CodeWorkspaceSelection, policy CodeWorkspacePolicy) ([]CodeRepositoryFile, error) {
	if err := selection.Validate(policy); err != nil {
		return nil, err
	}
	if err := g.Preflight(ctx, settings); err != nil {
		return nil, err
	}
	base, repository, configuration, err := githubWorkspaceConfig(settings)
	if err != nil {
		return nil, err
	}
	var identity struct {
		ID       json.Number `json:"id"`
		FullName string      `json:"full_name"`
	}
	if err := g.get(ctx, base, configuration, repository, 64<<10, &identity); err != nil {
		return nil, err
	}
	if identity.ID.String() != selection.RepositoryID || !strings.EqualFold(identity.FullName, repository) {
		return nil, ErrCodeWorkspaceInvalid
	}
	var commit struct {
		SHA  string `json:"sha"`
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := g.get(ctx, base, configuration, repository+"/git/commits/"+selection.Commit, 1<<20, &commit); err != nil {
		return nil, err
	}
	if commit.SHA != selection.Commit || !workspaceHex(commit.Tree.SHA, 40, 64) {
		return nil, ErrCodeWorkspaceInvalid
	}
	var tree struct {
		SHA       string `json:"sha"`
		Truncated bool   `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
			Size uint64 `json:"size"`
		} `json:"tree"`
	}
	if err := g.get(ctx, base, configuration, repository+"/git/trees/"+commit.Tree.SHA+"?recursive=1", 16<<20, &tree); err != nil {
		return nil, err
	}
	if tree.SHA != commit.Tree.SHA || tree.Truncated || len(tree.Tree) > 100000 {
		return nil, ErrCodeWorkspaceInvalid
	}
	files := make([]CodeRepositoryFile, 0)
	var total uint64
	for _, item := range tree.Tree {
		if !selection.contains(item.Path) {
			continue
		}
		if err := workspacePath(item.Path, policy); err != nil {
			return nil, err
		}
		if item.Type == "tree" && item.Mode == "040000" {
			continue
		}
		if item.Type != "blob" || item.Mode != "100644" && item.Mode != "100755" || !workspaceHex(item.SHA, 40, 64) {
			return nil, ErrCodeWorkspaceInvalid
		}
		if uint64(len(files)) >= uint64(policy.MaxFiles) {
			return nil, &CodeWorkspaceBoundsError{"max_files", uint64(policy.MaxFiles)}
		}
		if item.Size > policy.MaxFileBytes {
			return nil, &CodeWorkspaceBoundsError{"max_file_bytes", policy.MaxFileBytes}
		}
		if item.Size > policy.MaxTotalBytes-total {
			return nil, &CodeWorkspaceBoundsError{"max_total_bytes", policy.MaxTotalBytes}
		}
		total += item.Size
		var blob struct {
			SHA      string `json:"sha"`
			Size     uint64 `json:"size"`
			Encoding string `json:"encoding"`
			Content  string `json:"content"`
		}
		if err := g.get(ctx, base, configuration, repository+"/git/blobs/"+item.SHA, policy.MaxFileBytes*2+(64<<10), &blob); err != nil {
			return nil, err
		}
		if blob.SHA != item.SHA || blob.Size != item.Size || blob.Encoding != "base64" {
			return nil, ErrCodeWorkspaceInvalid
		}
		content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
		if err != nil || uint64(len(content)) != item.Size {
			return nil, ErrCodeWorkspaceInvalid
		}
		gitBytes := append([]byte("blob "+strconv.FormatUint(item.Size, 10)+"\x00"), content...)
		var gitDigest string
		if len(item.SHA) == 40 {
			h := sha1.Sum(gitBytes)
			gitDigest = hex.EncodeToString(h[:])
		} else {
			h := sha256.Sum256(gitBytes)
			gitDigest = hex.EncodeToString(h[:])
		}
		if gitDigest != item.SHA {
			return nil, ErrCodeWorkspaceInvalid
		}
		hash := sha256.Sum256(content)
		files = append(files, CodeRepositoryFile{CodeWorkspaceFile{item.Path, item.Size, hex.EncodeToString(hash[:]), item.Mode == "100755"}, content})
	}
	return files, nil
}
