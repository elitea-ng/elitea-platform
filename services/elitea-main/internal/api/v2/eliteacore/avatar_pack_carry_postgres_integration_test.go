package eliteacore_test

// Real-PostgreSQL coverage for #1853: fork, export and import carry a
// version's avatar pack, and the copy's media live in the TARGET project's
// agent-media bucket with the pack and meta.icon_meta rewritten to them.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentmediaapi"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmedia"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// mediaStore is an in-memory object store: Put, Get and Stat.
type mediaStore struct {
	storage.ObjectStore
	mu      sync.Mutex
	objects map[string][]byte
}

func newMediaStore() *mediaStore { return &mediaStore{objects: map[string][]byte{}} }

func mediaKey(ref storage.ObjectRef) string {
	return ref.ProjectID() + "/" + ref.Bucket() + "/" + ref.Key()
}

func (m *mediaStore) Put(_ context.Context, ref storage.ObjectRef, body io.Reader, _ storage.PutOptions) (storage.ObjectInfo, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[mediaKey(ref)] = data
	return storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}

func (m *mediaStore) Get(_ context.Context, ref storage.ObjectRef, _ *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[mediaKey(ref)]
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}

func (m *mediaStore) Stat(_ context.Context, ref storage.ObjectRef) (storage.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[mediaKey(ref)]
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	return storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}

func (m *mediaStore) has(project, file string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[project+"/"+agentmediaapi.Bucket+"/"+file]
	return ok
}

// putMedia stores a small PNG of colour c in project's bucket and answers its URL.
func putMedia(t *testing.T, store *mediaStore, project string, c color.Color) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	file := hex.EncodeToString(sum[:]) + ".png"
	ref, err := storage.NewObjectRef(project, agentmediaapi.Bucket, file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), ref, bytes.NewReader(buf.Bytes()), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	return agentmedia.URLOf(project, file)
}

func avatarPack(still, thinking string) map[string]any {
	return map[string]any{
		"schema": agentmedia.PackSchema,
		"slots": map[string]any{
			"still":    map[string]any{"url": still, "media_type": "image/png"},
			"thinking": map[string]any{"url": thinking, "media_type": "image/png"},
		},
	}
}

// storedPackAndIcon reads the one version the copy wrote.
func storedPackAndIcon(t *testing.T, pool *pgxpool.Pool, where string) (agentmedia.Pack, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var packText *string
	var icon string
	if err := pool.QueryRow(ctx, `
SELECT avatar_pack::text, COALESCE(meta->'icon_meta'->>'url', '')
FROM p_1.application_versions `+where).Scan(&packText, &icon); err != nil {
		t.Fatalf("read the copied version: %v", err)
	}
	if packText == nil {
		t.Fatal("the copy has no avatar_pack")
	}
	var pack agentmedia.Pack
	if err := json.Unmarshal([]byte(*packText), &pack); err != nil {
		t.Fatal(err)
	}
	return pack, icon
}

func fileOf(url string) string {
	_, file, _, _ := agentmedia.ParseURL(url)
	return file
}

func TestForkCarriesTheAvatarPackIntoTheTargetBucket(t *testing.T) {
	pool := newImportLinkPool(t)
	store := newMediaStore()
	still := putMedia(t, store, "9", color.White)
	thinking := putMedia(t, store, "9", color.Black)

	body := forkBody("api_token")
	version := body["applications"].([]any)[0].(map[string]any)["versions"].([]any)[0].(map[string]any)
	version["avatar_pack"] = avatarPack(still, thinking)
	version["meta"] = map[string]any{"icon_meta": map[string]any{"url": still, "name": strings.TrimPrefix(still, agentmedia.URLPrefix)}}

	recorder := forkDo(t, forkRouter(eliteacore.NewHandler(pool, eliteacore.WithObjectStore(store)), true), body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("fork status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	pack, icon := storedPackAndIcon(t, pool, "")
	if pack.Slots["still"].URL != agentmedia.URLOf("1", fileOf(still)) || pack.Slots["thinking"].URL != agentmedia.URLOf("1", fileOf(thinking)) {
		t.Fatalf("the fork's pack still names the source project: %+v", pack.Slots)
	}
	if icon != agentmedia.URLOf("1", fileOf(still)) {
		t.Fatalf("meta.icon_meta.url = %q, want the target's copy", icon)
	}
	if !store.has("1", fileOf(still)) || !store.has("1", fileOf(thinking)) {
		t.Fatal("the target bucket has no copy of the media")
	}
}

func TestForkReportsAnInvalidAvatarPack(t *testing.T) {
	pool := newImportLinkPool(t)
	body := forkBody("api_token")
	version := body["applications"].([]any)[0].(map[string]any)["versions"].([]any)[0].(map[string]any)
	version["avatar_pack"] = map[string]any{"schema": "something/else"}

	recorder := forkDo(t, forkRouter(eliteacore.NewHandler(pool, eliteacore.WithObjectStore(newMediaStore())), true), body)
	if !strings.Contains(recorder.Body.String(), agentmedia.PackCode) {
		t.Fatalf("the refused pack was not reported: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestExportEmbedsTheMediaAndImportRestoresThem(t *testing.T) {
	pool := newImportLinkPool(t)
	source := newMediaStore()
	seeded := seedRoundTripAgent(t, pool)
	still := putMedia(t, source, "1", color.White)
	// A file of another project is not the exporter's to embed; the import
	// copies it when that project is on the same installation.
	foreign := putMedia(t, source, "9", color.Black)
	packJSON, err := json.Marshal(avatarPack(still, foreign))
	if err != nil {
		t.Fatal(err)
	}
	roundTripExec(t, pool, `
UPDATE p_1.application_versions
   SET avatar_pack = $2::jsonb,
       meta = jsonb_build_object('icon_meta', jsonb_build_object('url', $3::text))
 WHERE id = $1`, seeded.versionID, string(packJSON), still)

	document := exportRoundTrip(t, eliteacore.NewHandler(pool, eliteacore.WithObjectStore(source)), seeded.applicationID, http.StatusOK)
	exported := document["applications"].([]any)[0].(map[string]any)["versions"].([]any)[0].(map[string]any)
	if _, ok := exported["avatar_pack"].(map[string]any); !ok {
		t.Fatalf("the export has no avatar_pack: %v", exported)
	}
	media, _ := exported[agentmediaapi.EmbeddedMediaKey].(map[string]any)
	if len(media) != 1 || media[fileOf(still)] == nil {
		t.Fatalf("avatar_media = %v, want only the exporting project's still", media)
	}

	// Another installation: an empty bucket, the source project 9 present.
	target := newMediaStore()
	putMedia(t, target, "9", color.Black)
	recorder := importLinkDo(t, importLinkRouter(eliteacore.NewHandler(pool, eliteacore.WithObjectStore(target))), roundTripImportBody(t, document))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if answer := decodeImportLink(t, recorder); len(answer.Errors.Agents) != 0 {
		t.Fatalf("the import reported errors: %s", recorder.Body.String())
	}
	pack, icon := storedPackAndIcon(t, pool, "WHERE id <> "+strconv.Itoa(seeded.versionID))
	if pack.Slots["still"].URL != still || icon != still {
		t.Fatalf("still = %q, icon = %q, want %q", pack.Slots["still"].URL, icon, still)
	}
	if pack.Slots["thinking"].URL != agentmedia.URLOf("1", fileOf(foreign)) {
		t.Fatalf("thinking = %q, want the copy in project 1", pack.Slots["thinking"].URL)
	}
	if !target.has("1", fileOf(still)) || !target.has("1", fileOf(foreign)) {
		t.Fatal("the importing installation's bucket has no copy of the media")
	}
}

func TestImportRefusesEmbeddedMediaThatIsNotItsName(t *testing.T) {
	pool := newImportLinkPool(t)
	store := newMediaStore()
	still := putMedia(t, store, "1", color.White)

	// "hello" under the still's name: not its digest, not an image.
	body := []any{map[string]any{
		"entity": "agents", "name": "bad media", "description": "x",
		"versions": []any{map[string]any{
			"name": "latest", "agent_type": "openai",
			"avatar_pack":                  avatarPack(still, still),
			agentmediaapi.EmbeddedMediaKey: map[string]any{fileOf(still): "aGVsbG8="},
		}},
	}}
	recorder := importLinkDo(t, importLinkRouter(eliteacore.NewHandler(pool, eliteacore.WithObjectStore(store))), body)
	if !strings.Contains(recorder.Body.String(), agentmedia.Code) {
		t.Fatalf("the mismatched file was not reported: %d %s", recorder.Code, recorder.Body.String())
	}
}
