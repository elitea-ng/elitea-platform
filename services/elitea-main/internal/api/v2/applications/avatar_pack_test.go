package applications_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmedia"
)

func packURL(project, ext string) string {
	return agentmedia.URLPrefix + project + "/" + strings.Repeat("cd", 32) + "." + ext
}

func putVersion(t *testing.T, repo *recordingRepo, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("PUT", "/version/prompt_lib/1/2/3", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	setupVersionRouter(repo).ServeHTTP(rec, req)
	return rec
}

// ADR-0050 decision 6: a pack is stored typed, and its still is mirrored into
// meta.icon_meta so a client that reads only the old field shows it.
func TestUpdateVersion_AvatarPackStoredAndStillMirroredToIconMeta(t *testing.T) {
	repo := &recordingRepo{}
	rec := putVersion(t, repo, map[string]any{
		"meta": map[string]any{"step_limit": 25},
		"avatar_pack": map[string]any{
			"schema": agentmedia.PackSchema,
			"slots": map[string]any{
				"still":    map[string]any{"url": packURL("1", "png")},
				"thinking": map[string]any{"url": packURL("1", "webm"), "poster_url": packURL("1", "png")},
			},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !repo.lastUpdate.Present.AvatarPack || len(repo.lastUpdate.AvatarPack) == 0 {
		t.Fatalf("pack not forwarded: %+v", repo.lastUpdate)
	}
	icon, _ := repo.lastUpdate.Meta["icon_meta"].(map[string]any)
	if icon["url"] != packURL("1", "png") {
		t.Fatalf("icon_meta = %v", repo.lastUpdate.Meta["icon_meta"])
	}
	if repo.lastUpdate.Meta["step_limit"] != float64(25) {
		t.Fatalf("the body's own meta was lost: %v", repo.lastUpdate.Meta)
	}
}

func TestUpdateVersion_AvatarPackRefusals(t *testing.T) {
	cases := map[string]any{
		"another project's media": map[string]any{"schema": agentmedia.PackSchema, "slots": map[string]any{"still": map[string]any{"url": packURL("9", "png")}}},
		"no still":                map[string]any{"schema": agentmedia.PackSchema, "slots": map[string]any{"idle": map[string]any{"url": packURL("1", "webp")}}},
		"video without poster":    map[string]any{"schema": agentmedia.PackSchema, "slots": map[string]any{"still": map[string]any{"url": packURL("1", "png")}, "idle": map[string]any{"url": packURL("1", "mp4")}}},
		"icon path":               map[string]any{"schema": agentmedia.PackSchema, "slots": map[string]any{"still": map[string]any{"url": "/icons/1/a.png"}}},
	}
	for name, pack := range cases {
		t.Run(name, func(t *testing.T) {
			repo := &recordingRepo{}
			rec := putVersion(t, repo, map[string]any{"avatar_pack": pack})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			var body struct{ Code string }
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if body.Code != agentmedia.PackCode {
				t.Fatalf("code %q", body.Code)
			}
			if repo.lastUpdate.Present.AvatarPack {
				t.Fatal("a refused pack reached the repository")
			}
		})
	}
}

func TestUpdateVersion_AvatarPackNullClearsAndAbsentLeavesAlone(t *testing.T) {
	repo := &recordingRepo{}
	if rec := putVersion(t, repo, map[string]any{"avatar_pack": nil}); rec.Code != http.StatusCreated {
		t.Fatalf("null: %d %s", rec.Code, rec.Body)
	}
	if !repo.lastUpdate.Present.AvatarPack || repo.lastUpdate.AvatarPack != nil {
		t.Fatalf("null did not clear: %+v", repo.lastUpdate)
	}
	repo = &recordingRepo{}
	if rec := putVersion(t, repo, map[string]any{"instructions": "x"}); rec.Code != http.StatusCreated {
		t.Fatalf("absent: %d", rec.Code)
	}
	if repo.lastUpdate.Present.AvatarPack {
		t.Fatal("an absent key touched the pack")
	}
}
