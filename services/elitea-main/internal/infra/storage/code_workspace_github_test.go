package storage

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib"
)

type workspaceGithubTransport struct {
	reply func(*http.Request) (any, int)
	paths []string
}

func (r *workspaceGithubTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	r.paths = append(r.paths, request.URL.Path)
	value, status := r.reply(request)
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), ContentLength: int64(len(body)), Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
}
func workspaceGithubFixture(t *testing.T) (*CodeWorkspaceGitHub, map[string]any, *workspaceGithubTransport) {
	t.Helper()
	allowed, err := egresslib.Parse([]string{"github.example"})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewCodeWorkspaceGitHub(allowed)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("hello")
	h := sha1.Sum(append([]byte("blob 5\x00"), content...))
	blob := hex.EncodeToString(h[:])
	transport := &workspaceGithubTransport{reply: func(request *http.Request) (any, int) {
		if request.URL.Host != "github.example" || request.Header.Get("Authorization") != "token synthetic-secret" {
			t.Fatal("acquisition changed saved origin or auth")
		}
		switch request.URL.Path {
		case "/repos/owner/repository":
			return map[string]any{"id": 123, "full_name": "owner/repository"}, 200
		case "/repos/owner/repository/git/commits/" + strings.Repeat("2", 40):
			return map[string]any{"sha": strings.Repeat("2", 40), "tree": map[string]any{"sha": strings.Repeat("3", 40)}}, 200
		case "/repos/owner/repository/git/trees/" + strings.Repeat("3", 40):
			if request.URL.RawQuery != "recursive=1" {
				t.Fatal("tree is not complete")
			}
			return map[string]any{"sha": strings.Repeat("3", 40), "truncated": false, "tree": []any{map[string]any{"path": "src/greeting.txt", "type": "blob", "mode": "100644", "sha": blob, "size": 5}}}, 200
		case "/repos/owner/repository/git/blobs/" + blob:
			return map[string]any{"sha": blob, "size": 5, "encoding": "base64", "content": base64.StdEncoding.EncodeToString(content)}, 200
		default:
			return map[string]any{}, 404
		}
	}}
	adapter.client.Transport = transport
	settings := map[string]any{"repository": "owner/repository", "selected_tools": []any{"read_file"}, "github_configuration": map[string]any{"base_url": "https://github.example", "access_token": "synthetic-secret"}}
	return adapter, settings, transport
}

func TestCodeWorkspaceGitHubAcquiresExactSavedRepositoryCommitAndVerifiedRegularBlob(t *testing.T) {
	adapter, settings, transport := workspaceGithubFixture(t)
	files, err := adapter.Acquire(context.Background(), settings, workspaceSelectionFixture(), DefaultCodeWorkspacePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].File.Path != "src/greeting.txt" || string(files[0].Content) != "hello" || files[0].File.SHA256 != workspaceFilesFixture()[0].SHA256 || len(transport.paths) != 4 {
		t.Fatal("repository snapshot does not match the exact commit")
	}
}
func TestCodeWorkspaceGitHubResourceCommitSymlinkSubmoduleAndBoundsRefuseBeforeBlobFetch(t *testing.T) {
	for _, kind := range []string{"repository", "commit", "symlink", "submodule", "bounds", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			adapter, settings, transport := workspaceGithubFixture(t)
			original := transport.reply
			transport.reply = func(request *http.Request) (any, int) {
				value, status := original(request)
				m := value.(map[string]any)
				switch {
				case kind == "repository" && request.URL.Path == "/repos/owner/repository":
					m["id"] = 124
				case kind == "commit" && strings.Contains(request.URL.Path, "/git/commits/"):
					m["sha"] = strings.Repeat("f", 40)
				case strings.Contains(request.URL.Path, "/git/trees/"):
					entry := m["tree"].([]any)[0].(map[string]any)
					switch kind {
					case "symlink":
						entry["mode"] = "120000"
					case "submodule":
						entry["mode"] = "160000"
						entry["type"] = "commit"
					case "bounds":
						entry["size"] = uint64(2 << 20)
					case "truncated":
						m["truncated"] = true
					}
				}
				return m, status
			}
			if _, err := adapter.Acquire(context.Background(), settings, workspaceSelectionFixture(), DefaultCodeWorkspacePolicy()); err == nil {
				t.Fatal("accepted invalid repository acquisition")
			}
			for _, path := range transport.paths {
				if strings.Contains(path, "/git/blobs/") {
					t.Fatal("failure read repository file bytes")
				}
			}
		})
	}
}
func TestCodeWorkspaceGitHubSelectedReadAndOriginDenyBeforeAnyProviderRequest(t *testing.T) {
	for _, kind := range []string{"selection", "origin", "url-user", "repository-path"} {
		t.Run(kind, func(t *testing.T) {
			adapter, settings, transport := workspaceGithubFixture(t)
			switch kind {
			case "selection":
				settings["selected_tools"] = []string{"create_file"}
			case "origin":
				settings["github_configuration"].(map[string]any)["base_url"] = "https://other.example"
			case "url-user":
				settings["github_configuration"].(map[string]any)["base_url"] = "https://synthetic-secret@github.example"
			case "repository-path":
				settings["repository"] = "owner/../repository"
			}
			if _, err := adapter.Acquire(context.Background(), settings, workspaceSelectionFixture(), DefaultCodeWorkspacePolicy()); err == nil {
				t.Fatal("accepted unauthorized read capability")
			}
			if len(transport.paths) != 0 {
				t.Fatal("failure crossed provider boundary")
			}
		})
	}
}
func TestCodeWorkspaceGitHubSafeErrorsDoNotExposeCredentialOrProviderCause(t *testing.T) {
	adapter, settings, transport := workspaceGithubFixture(t)
	transport.reply = func(*http.Request) (any, int) { return map[string]any{"secret": "synthetic-secret"}, 403 }
	_, err := adapter.Acquire(context.Background(), settings, workspaceSelectionFixture(), DefaultCodeWorkspacePolicy())
	if err == nil || strings.Contains(err.Error(), "synthetic-secret") || strings.Contains(err.Error(), "github.example") {
		t.Fatal("unsafe acquisition diagnostic")
	}
}

func TestCodeWorkspaceSavedGitHubRepositoryFormsRetainExistingBusinessIdentity(t *testing.T) {
	for _, saved := range []string{"owner/repository", "owner/repository.git", "https://github.example/owner/repository.git", "git@github.example:owner/repository.git"} {
		actual, err := githubWorkspaceRepository(saved)
		if err != nil || actual != "owner/repository" {
			t.Fatalf("saved form lost repository identity: %q", saved)
		}
	}
	for _, saved := range []string{"", "https://user:secret@github.example/owner/repository", "https://github.example/owner/repository?branch=main", "owner/repository/extra", "owner/../escape", "git@:owner/repository"} {
		if _, err := githubWorkspaceRepository(saved); err == nil {
			t.Fatal("unsafe saved repository form accepted")
		}
	}
}
