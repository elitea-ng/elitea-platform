package applications_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmedia"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
)

func storedPack(t *testing.T, still string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"schema": agentmedia.PackSchema,
		"slots":  map[string]any{"still": map[string]any{"url": still, "media_type": "image/png"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func packBody(still string) map[string]any {
	return map[string]any{"schema": agentmedia.PackSchema, "slots": map[string]any{"still": map[string]any{"url": still}}}
}

// A version whose pack was copied from another project (publish, the
// catalogue mirror, the MCP copy) keeps saving: the editor forwards the
// stored pack, and a url the version already holds is accepted.
func TestUpdateVersion_AvatarPackForwardedFromAnotherProjectIsAccepted(t *testing.T) {
	foreign := packURL("9", "png")
	repo := &recordingRepo{mockRepo: mockRepo{versions: []applications.Version{{
		ID: "3", AvatarPack: storedPack(t, foreign),
		Meta: map[string]any{"icon_meta": map[string]any{"url": foreign}},
	}}}}
	rec := putVersion(t, repo, map[string]any{"instructions": "x", "avatar_pack": packBody(foreign)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	// A url it does not already hold is still refused.
	rec = putVersion(t, repo, map[string]any{"avatar_pack": packBody(packURL("8", "png"))})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a new foreign url: %d", rec.Code)
	}
}

// An unchanged pack does not overwrite an icon picked on its own; a new
// still does.
func TestUpdateVersion_UnchangedPackKeepsTheChosenIcon(t *testing.T) {
	still := packURL("1", "png")
	repo := &recordingRepo{mockRepo: mockRepo{versions: []applications.Version{{
		ID: "3", AvatarPack: storedPack(t, still),
		Meta: map[string]any{"icon_meta": map[string]any{"url": "/icons/1/picked.png"}},
	}}}}
	rec := putVersion(t, repo, map[string]any{
		"meta":        map[string]any{"icon_meta": map[string]any{"url": "/icons/1/picked.png"}},
		"avatar_pack": packBody(still),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if icon, _ := repo.lastUpdate.Meta["icon_meta"].(map[string]any); icon["url"] != "/icons/1/picked.png" {
		t.Fatalf("an unchanged pack overwrote the icon: %v", repo.lastUpdate.Meta["icon_meta"])
	}
	changed := packURL("1", "webp")
	if rec := putVersion(t, repo, map[string]any{"avatar_pack": packBody(changed)}); rec.Code != http.StatusCreated {
		t.Fatalf("status %d", rec.Code)
	}
	if icon, _ := repo.lastUpdate.Meta["icon_meta"].(map[string]any); icon["url"] != changed {
		t.Fatalf("a new still did not reach icon_meta: %v", repo.lastUpdate.Meta["icon_meta"])
	}
}

// Clearing the pack clears icon_meta when it shows the removed still, and
// leaves an icon chosen on its own.
func TestUpdateVersion_ClearingThePackClearsItsMirroredIcon(t *testing.T) {
	still := packURL("1", "png")
	repo := &recordingRepo{mockRepo: mockRepo{versions: []applications.Version{{
		ID: "3", AvatarPack: storedPack(t, still),
		Meta: map[string]any{"icon_meta": map[string]any{"url": still}},
	}}}}
	if rec := putVersion(t, repo, map[string]any{"avatar_pack": nil}); rec.Code != http.StatusCreated {
		t.Fatalf("status %d", rec.Code)
	}
	if icon, ok := repo.lastUpdate.Meta["icon_meta"].(map[string]any); !ok || len(icon) != 0 {
		t.Fatalf("the removed still stayed the icon: %v", repo.lastUpdate.Meta["icon_meta"])
	}

	repo.versions[0].Meta = map[string]any{"icon_meta": map[string]any{"url": "/icons/1/picked.png"}}
	if rec := putVersion(t, repo, map[string]any{"avatar_pack": nil}); rec.Code != http.StatusCreated {
		t.Fatalf("status %d", rec.Code)
	}
	if _, touched := repo.lastUpdate.Meta["icon_meta"]; touched {
		t.Fatalf("a chosen icon was cleared: %v", repo.lastUpdate.Meta["icon_meta"])
	}
}
