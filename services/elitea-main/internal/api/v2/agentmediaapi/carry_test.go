package agentmediaapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image/color"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmedia"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

func storeFile(t *testing.T, store *memoryStore, project string, data []byte) string {
	t.Helper()
	media, err := agentmedia.Inspect(data)
	if err != nil {
		t.Fatal(err)
	}
	object, err := (&Handler{store: store}).put(context.Background(), project, data, media)
	if err != nil {
		t.Fatal(err)
	}
	return object.URL
}

func packOf(t *testing.T, still, thinking string) json.RawMessage {
	t.Helper()
	pack, err := agentmedia.NormalizePack(map[string]any{
		"schema": agentmedia.PackSchema,
		"slots": map[string]any{
			"still":    map[string]any{"url": still},
			"thinking": map[string]any{"url": thinking},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return pack
}

func TestCarryMediaCopiesIntoTheTargetAndRewrites(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	still := storeFile(t, store, "7", pngOf(t, 32, color.White))
	thinking := storeFile(t, store, "7", pngOf(t, 32, color.Black))
	pack := packOf(t, still, thinking)

	mapping := CarryMedia(ctx, store, "9", PackURLs(pack), nil)
	if len(mapping) != 2 {
		t.Fatalf("mapping = %v", mapping)
	}
	rewritten, err := RewritePack(pack, mapping)
	if err != nil {
		t.Fatal(err)
	}
	for _, url := range PackURLs(rewritten) {
		owner, file, _, _ := agentmedia.ParseURL(url)
		if owner != "9" {
			t.Fatalf("url %s is not the target's", url)
		}
		ref, _ := storage.NewObjectRef("9", Bucket, file)
		if _, err := store.Stat(ctx, ref); err != nil {
			t.Fatalf("the target has no %s: %v", file, err)
		}
	}

	meta := map[string]any{"icon_meta": map[string]any{"url": still, "name": strings.TrimPrefix(still, agentmedia.URLPrefix)}}
	RewriteIconMeta(meta, mapping)
	icon := meta["icon_meta"].(map[string]any)
	if icon["url"] != mapping[still] || icon["name"] != strings.TrimPrefix(mapping[still], agentmedia.URLPrefix) {
		t.Fatalf("icon_meta = %v", icon)
	}

	// A file the source no longer has keeps its URL; nothing fails.
	missing := agentmedia.URLOf("7", strings.Repeat("cd", 32)+".png")
	if got := CarryMedia(ctx, store, "9", []string{missing}, nil); len(got) != 0 {
		t.Fatalf("a missing file was mapped: %v", got)
	}
	// No store: the copy keeps every URL.
	if got := CarryMedia(ctx, nil, "9", PackURLs(pack), nil); len(got) != 0 {
		t.Fatalf("no store mapped %v", got)
	}
}

func TestEmbeddedMediaRoundTripAndRefusals(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	data := pngOf(t, 32, color.White)
	still := storeFile(t, store, "7", data)
	foreign := storeFile(t, store, "8", pngOf(t, 32, color.Black))

	embedded := EmbedMedia(ctx, store, "7", []string{still, foreign})
	if len(embedded) != 1 {
		t.Fatalf("embedded = %d files, want only the exporting project's", len(embedded))
	}
	raw := map[string]any{}
	for file, text := range embedded {
		raw[file] = text
	}
	files, err := DecodeEmbeddedMedia(raw)
	if err != nil {
		t.Fatal(err)
	}

	// Into a store that has never seen the source project.
	target := newMemoryStore()
	mapping := CarryMedia(ctx, target, "9", []string{still}, files)
	_, file, _, _ := agentmedia.ParseURL(still)
	ref, _ := storage.NewObjectRef("9", Bucket, file)
	body, _, err := target.Get(ctx, ref, nil)
	if err != nil || mapping[still] != agentmedia.URLOf("9", file) {
		t.Fatalf("embedded file not stored: %v %v", err, mapping)
	}
	var got bytes.Buffer
	_, _ = got.ReadFrom(body)
	if !bytes.Equal(got.Bytes(), data) {
		t.Fatal("stored bytes differ")
	}

	sum := sha256.Sum256([]byte("<html>x</html>"))
	html := hex.EncodeToString(sum[:]) + ".png"
	other := pngOf(t, 8, color.Black)
	for name, entry := range map[string]map[string]any{
		"not base64":      {file: "%%%"},
		"wrong digest":    {file: base64.StdEncoding.EncodeToString(other)},
		"not media":       {html: base64.StdEncoding.EncodeToString([]byte("<html>x</html>"))},
		"bad name":        {"x.png": base64.StdEncoding.EncodeToString(data)},
		"not a string":    {file: 3},
		"wrong extension": {strings.TrimSuffix(file, ".png") + ".jpg": base64.StdEncoding.EncodeToString(data)},
	} {
		if _, err := DecodeEmbeddedMedia(entry); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if _, err := DecodeEmbeddedMedia([]any{}); err == nil {
		t.Fatal("a list was accepted")
	}
}
