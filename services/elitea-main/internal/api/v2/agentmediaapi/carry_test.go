package agentmediaapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image/color"
	"strconv"
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

	mapping := CarryMedia(ctx, store, "9", PackURLs(pack), nil, anyProject)
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
	if got := CarryMedia(ctx, store, "9", []string{missing}, nil, anyProject); len(got) != 0 {
		t.Fatalf("a missing file was mapped: %v", got)
	}
	// No store: the copy keeps every URL.
	if got := CarryMedia(ctx, nil, "9", PackURLs(pack), nil, anyProject); len(got) != 0 {
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
	mapping := CarryMedia(ctx, target, "9", []string{still}, files, nil)
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

func anyProject(string) bool { return true }

// A file already in the target is written again, so its modification time
// moves: an old orphan a fork claims must not be swept by its age.
func TestCarryMediaRefreshesAnExistingObject(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	still := storeFile(t, store, "7", pngOf(t, 32, color.White))
	_, file, _, _ := agentmedia.ParseURL(still)
	if got := CarryMedia(ctx, store, "9", []string{still}, nil, anyProject); len(got) != 1 {
		t.Fatalf("first carry = %v", got)
	}
	ref, _ := storage.NewObjectRef("9", Bucket, file)
	before := store.putCount(ref)
	if got := CarryMedia(ctx, store, "9", []string{still}, nil, anyProject); len(got) != 1 {
		t.Fatalf("second carry = %v", got)
	}
	if store.putCount(ref) != before+1 {
		t.Fatalf("an existing object was not written again (puts %d -> %d)", before, store.putCount(ref))
	}
}

// A URL may name any project: only a readable one is copied from.
func TestCarryMediaDoesNotReadAnUnreadableProject(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	secret := storeFile(t, store, "7", pngOf(t, 32, color.White))
	_, file, _, _ := agentmedia.ParseURL(secret)
	ref, _ := storage.NewObjectRef("9", Bucket, file)

	for name, readable := range map[string]func(string) bool{"nil": nil, "denied": func(string) bool { return false }} {
		if got := CarryMedia(ctx, store, "9", []string{secret}, nil, readable); len(got) != 0 {
			t.Fatalf("%s: an unreadable project's file was mapped: %v", name, got)
		}
		if _, err := store.Stat(ctx, ref); err == nil {
			t.Fatalf("%s: the file was copied out of an unreadable project", name)
		}
	}
	if got := CarryMedia(ctx, store, "9", []string{secret}, nil, func(owner string) bool { return owner == "7" }); len(got) != 1 {
		t.Fatalf("a readable project was not copied: %v", got)
	}
}

func TestDecodeEmbeddedMediaCapsEntriesAndTotalBytes(t *testing.T) {
	tooMany := map[string]any{}
	for i := 0; i <= MaxEmbeddedFiles; i++ {
		tooMany[strings.Repeat("a", 60)+strconv.Itoa(1000+i)+".png"] = ""
	}
	if _, err := DecodeEmbeddedMedia(tooMany); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("entry count: %v", err)
	}
	// Each file is under the per-file cap; together they pass the total cap.
	big := strings.Repeat("A", agentmedia.MaxBytes/3*4)
	heavy := map[string]any{}
	for i := 0; i < MaxEmbeddedBytes/agentmedia.MaxBytes+1; i++ {
		heavy[strings.Repeat("b", 60)+strconv.Itoa(1000+i)+".png"] = big
	}
	if _, err := DecodeEmbeddedMedia(heavy); err == nil || !strings.Contains(err.Error(), "in all") {
		t.Fatalf("total bytes: %v", err)
	}
}
