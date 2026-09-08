package brandpackage

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	v2branding "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/branding"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

// memoryStore is the in-memory storage.ObjectStore the branding package's
// own tests use, repeated here because it lives in that package's test files.
type memoryStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	stamps  map[string]time.Time
	clock   time.Time
}

func newMemoryStore() *memoryStore {
	return &memoryStore{objects: map[string][]byte{}, stamps: map[string]time.Time{}, clock: time.Unix(1_700_000_000, 0)}
}

func (m *memoryStore) Put(_ context.Context, ref storage.ObjectRef, body io.Reader, _ storage.PutOptions) (storage.ObjectInfo, error) {
	data, _ := io.ReadAll(body)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clock = m.clock.Add(time.Second)
	m.objects[ref.StorageKey("")] = data
	m.stamps[ref.StorageKey("")] = m.clock
	return storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}
func (m *memoryStore) Get(_ context.Context, ref storage.ObjectRef, _ *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[ref.StorageKey("")]
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}
func (m *memoryStore) Delete(_ context.Context, ref storage.ObjectRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, ref.StorageKey(""))
	return nil
}
func (m *memoryStore) List(_ context.Context, q storage.ListQuery) (storage.ListPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var page storage.ListPage
	for key, data := range m.objects {
		if rest, ok := strings.CutPrefix(key, q.Bucket.BucketPrefix("")); ok {
			page.Objects = append(page.Objects, storage.ObjectInfo{Key: rest, Size: int64(len(data)), LastModified: m.stamps[key]})
		}
	}
	return page, nil
}
func (m *memoryStore) DeleteBatch(context.Context, []storage.ObjectRef) (storage.BatchResult, error) {
	return storage.BatchResult{}, storage.ErrNotSupported
}
func (m *memoryStore) Stat(context.Context, storage.ObjectRef) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}
func (m *memoryStore) PresignGet(context.Context, storage.ObjectRef, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}
func (m *memoryStore) PresignPut(context.Context, storage.ObjectRef, time.Duration, storage.PutOptions) (string, error) {
	return "", storage.ErrNotSupported
}
func (m *memoryStore) StartMultipart(context.Context, storage.ObjectRef, storage.PutOptions) (storage.UploadID, error) {
	return "", storage.ErrNotSupported
}
func (m *memoryStore) PresignPart(context.Context, storage.ObjectRef, storage.UploadID, int32, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}
func (m *memoryStore) CompleteMultipart(context.Context, storage.ObjectRef, storage.UploadID, []storage.Part) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}
func (m *memoryStore) AbortMultipart(context.Context, storage.ObjectRef, storage.UploadID) error {
	return storage.ErrNotSupported
}
func (m *memoryStore) Capabilities() storage.Capabilities { return storage.Capabilities{} }

const cleanSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10" fill="currentColor"/></svg>`

var woff2Bytes = append([]byte("wOF2"), bytes.Repeat([]byte{1}, 16)...)

func ptr(s string) *string { return &s }

func newService(t *testing.T) (*Service, *v2branding.AssetStore) {
	t.Helper()
	assets := v2branding.NewAssetStore(newMemoryStore())
	svc := New(assets, Previews{
		Login: func(p *v2branding.Pack) ([]byte, error) {
			return []byte("<html>login " + p.Product.Name + "</html>"), nil
		},
		Email: func(p *v2branding.Pack, kind string) (string, error) {
			return "<html>" + kind + " " + p.Product.Name + "</html>", nil
		},
		App: []byte("<html><head><title>preview</title></head><body></body></html>"),
	}, "test-deployment")
	svc.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	return svc, assets
}

func entries(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range r.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		out[f.Name] = b
	}
	return out
}

func buildZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		_, _ = f.Write(content)
	}
	_ = w.Close()
	return buf.Bytes()
}

func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc, assets := newService(t)

	// A branded deployment: uploaded logo and font, custom name and hue.
	logo, err := assets.Put(ctx, v2branding.KindLogoFull, "logo.svg", []byte(cleanSVG))
	if err != nil {
		t.Fatal(err)
	}
	font, err := assets.Put(ctx, v2branding.KindFont, "Inter.woff2", woff2Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pack := v2branding.ProductDefault()
	pack.Product.Name = "Acme AI"
	pack.Brand.Hue = "#FF6600"
	pack.Assets.LogoFull = logo.Path
	pack.Typography.FontFaces = []v2branding.FontFace{{Family: "Inter", URL: font.Path, Weight: ptr("400")}}

	data, name, err := svc.Export(ctx, pack)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if name != "acme-ai-branding.zip" {
		t.Errorf("name = %q", name)
	}
	got := entries(t, data)
	for _, want := range []string{"manifest.json", "brand-pack.json", "README.md", "schema/brand-pack.schema.json",
		"assets/logo-full.svg", "assets/fonts/inter-1.woff2", "preview/app.html", "preview/login.html",
		"preview/email-invitation.html", "preview/email-moderation.html"} {
		if _, ok := got[want]; !ok {
			t.Errorf("package lacks %s (has %d entries)", want, len(got))
		}
	}
	if !bytes.Equal(got["assets/logo-full.svg"], []byte(cleanSVG)) {
		t.Errorf("logo bytes differ")
	}
	if !strings.Contains(string(got["brand-pack.json"]), `"logoFull": "assets/logo-full.svg"`) ||
		!strings.Contains(string(got["brand-pack.json"]), `"url": "assets/fonts/inter-1.woff2"`) {
		t.Errorf("pack references not rewritten:\n%s", got["brand-pack.json"])
	}
	if !strings.Contains(string(got["preview/app.html"]), `<script type="application/json" id="brand-pack">`) ||
		!strings.Contains(string(got["preview/app.html"]), `"Acme AI"`) {
		t.Errorf("previewer lacks the inlined pack")
	}
	if !strings.Contains(string(got["preview/login.html"]), "login Acme AI") {
		t.Errorf("login preview not rendered under the pack")
	}
	if !strings.Contains(string(got["README.md"]), "# Acme AI branding package") {
		t.Errorf("README not rendered")
	}

	// Import into a fresh store: the assets are stored again and the values
	// carry their new paths; nothing hand-tuned means empty scheme tokens.
	fresh, freshAssets := newService(t)
	imported, problems := fresh.Parse(data)
	if len(problems) != 0 {
		t.Fatalf("problems: %+v", problems)
	}
	if imported.Manifest == nil || imported.Manifest.Product != "Acme AI" || len(imported.Assets) != 2 {
		t.Fatalf("imported = %+v", imported)
	}
	values, err := fresh.Apply(ctx, imported)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if values[platformconfig.KeyBrandingProductName] != "Acme AI" || values[platformconfig.KeyBrandingHue] != "#FF6600" {
		t.Errorf("values = %v", values)
	}
	logoPath, _ := values[platformconfig.KeyBrandingLogoFull].(string)
	if kind, _, _, ok := v2branding.ParseAssetPath(logoPath); !ok || kind != v2branding.KindLogoFull {
		t.Errorf("logo not stored as an asset path: %q", logoPath)
	}
	if stored, _, err := freshAssets.Get(ctx, logoPath); err != nil || !bytes.Equal(stored, []byte(cleanSVG)) {
		t.Errorf("stored logo bytes differ: %v", err)
	}
	faces, _ := values[platformconfig.KeyBrandingFontFaces].([]any)
	if len(faces) != 1 {
		t.Fatalf("font faces = %v", faces)
	}
	if url, _ := faces[0].(map[string]any)["url"].(string); !strings.HasPrefix(url, v2branding.AssetPathPrefix+"font/") {
		t.Errorf("font url = %q", url)
	}
	tokens, _ := values[platformconfig.KeyBrandingSchemeTokens].(map[string]any)
	if len(tokens) != 0 {
		t.Errorf("an untouched export must not pin scheme tokens, got %d schemes", len(tokens))
	}
}

func TestValuesFromPack_HandTunedTokensAreKept(t *testing.T) {
	base := v2branding.ProductDefault()
	pack := v2branding.ProductDefault()
	pack.Schemes.Light["primary.main"] = "#123456"
	values := ValuesFromPack(pack, base)
	tokens := values[platformconfig.KeyBrandingSchemeTokens].(map[string]any)
	light, _ := tokens["light"].(map[string]any)
	if light["primary.main"] != "#123456" {
		t.Fatalf("tuned token lost: %v", tokens)
	}
	// Placeholders that are not deployment paths become "inherit".
	if values[platformconfig.KeyBrandingLogoFull] != "" {
		t.Errorf("compiled placeholder leaked into the section: %v", values[platformconfig.KeyBrandingLogoFull])
	}
}

func TestParse_Refusals(t *testing.T) {
	svc, _ := newService(t)
	good, _, _ := svc.Export(context.Background(), nil)
	goodEntries := entries(t, good)
	packJSON := goodEntries["brand-pack.json"]

	cases := map[string]struct {
		files map[string][]byte
		want  string
	}{
		"zip slip": {map[string][]byte{"brand-pack.json": packJSON, "../evil.txt": []byte("x")}, "plain relative path"},
		"absolute": {map[string][]byte{"brand-pack.json": packJSON, "/etc/passwd": []byte("x")}, "plain relative path"},
		"no pack":  {map[string][]byte{"README.md": []byte("x")}, "missing"},
		"bad pack": {map[string][]byte{"brand-pack.json": []byte(`{"id":`)}, "brand-pack.json"},
		"scripted svg": {map[string][]byte{
			"brand-pack.json":      bytes.Replace(packJSON, []byte(`"logoFull": "./brand/logo-full.svg"`), []byte(`"logoFull": "assets/logo-full.svg"`), 1),
			"assets/logo-full.svg": []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>1</script></svg>`),
		}, "<script>"},
		"missing referenced asset": {map[string][]byte{
			"brand-pack.json": bytes.Replace(packJSON, []byte(`"logoFull": "./brand/logo-full.svg"`), []byte(`"logoFull": "assets/logo-full.svg"`), 1),
		}, "not in the package"},
		"external asset reference": {map[string][]byte{
			"brand-pack.json": bytes.Replace(packJSON, []byte(`"logoFull": "./brand/logo-full.svg"`), []byte(`"logoFull": "https://cdn.example/l.svg"`), 1),
		}, "path on the deployment"},
		"wrong format": {map[string][]byte{"brand-pack.json": packJSON, "manifest.json": []byte(`{"format": 2}`)}, "format 2"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, problems := svc.Parse(buildZip(t, tc.files))
			if len(problems) == 0 {
				t.Fatal("accepted")
			}
			found := false
			for _, p := range problems {
				if strings.Contains(p.Reason, tc.want) || strings.Contains(p.Entry, tc.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("problems %+v do not mention %q", problems, tc.want)
			}
		})
	}

	t.Run("not a zip", func(t *testing.T) {
		if _, problems := svc.Parse([]byte("hello")); len(problems) == 0 {
			t.Fatal("accepted")
		}
	})
	t.Run("directory entries are skipped, not refused", func(t *testing.T) {
		// Finder, Explorer, JSZip and most archivers write `assets/` before
		// the files under it. archive/zip's Create makes one for a name that
		// ends in "/". Before the fix every such package was refused with
		// "entry name is not a plain relative path" on the directory itself.
		imported, problems := svc.Parse(buildZip(t, map[string][]byte{
			"assets/":              nil,
			"assets/fonts/":        nil,
			"brand-pack.json":      bytes.Replace(packJSON, []byte(`"logoFull": "./brand/logo-full.svg"`), []byte(`"logoFull": "assets/logo-full.svg"`), 1),
			"assets/logo-full.svg": []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1"/></svg>`),
		}))
		if len(problems) != 0 {
			t.Fatalf("problems %+v", problems)
		}
		for _, w := range imported.Warnings {
			if strings.Contains(w, "assets/") {
				t.Fatalf("directory entries must be silent, got warning %q", w)
			}
		}
	})
	t.Run("the package is named by its pack, not by the exported manifest", func(t *testing.T) {
		// A customer edits brand-pack.json and leaves manifest.json as the
		// export wrote it. The dialog and the versions list must say the
		// customer's product, not the one the package was exported from.
		restyled := bytes.Replace(packJSON, []byte(`"name": "Elitea"`), []byte(`"name": "Acme Corp"`), 1)
		if bytes.Equal(restyled, packJSON) {
			t.Fatalf("fixture pack does not carry the default product name: %s", packJSON[:200])
		}
		imported, problems := svc.Parse(buildZip(t, map[string][]byte{
			"manifest.json":   goodEntries["manifest.json"],
			"brand-pack.json": restyled,
		}))
		if len(problems) != 0 {
			t.Fatalf("problems %+v", problems)
		}
		if imported.Manifest == nil || imported.Manifest.Product != "Acme Corp" {
			t.Fatalf("manifest product = %+v, want the pack's", imported.Manifest)
		}
		if imported.Manifest.ExportedAt.IsZero() {
			t.Fatal("the manifest's own export time must be kept")
		}
	})
	t.Run("a directory entry that climbs out is still refused", func(t *testing.T) {
		_, problems := svc.Parse(buildZip(t, map[string][]byte{"brand-pack.json": packJSON, "../evil/": nil}))
		if len(problems) == 0 {
			t.Fatal("accepted")
		}
	})
	t.Run("extra entries are ignored with a warning", func(t *testing.T) {
		imported, problems := svc.Parse(buildZip(t, map[string][]byte{"brand-pack.json": packJSON, "notes.txt": []byte("hi")}))
		if len(problems) != 0 || len(imported.Warnings) == 0 {
			t.Fatalf("problems %v warnings %v", problems, imported.Warnings)
		}
	})
}

func TestInlinePack_EscapesClosingTag(t *testing.T) {
	out := InlinePack([]byte("<html><head></head><body></body></html>"), []byte(`{"name":"</script><script>alert(1)"}`))
	if strings.Count(string(out), "</script>") != 1 {
		t.Fatalf("closing tag not escaped:\n%s", out)
	}
	if !strings.Contains(string(out), `id="brand-pack"`) || strings.Index(string(out), "</head>") < strings.Index(string(out), `id="brand-pack"`) {
		t.Fatalf("pack not inlined before </head>:\n%s", out)
	}
}

func TestDiff(t *testing.T) {
	changes := Diff(map[string]any{"a": "1", "b": float64(2), "c": []any{}}, map[string]any{"a": "1", "b": float64(3), "c": []any{}, "d": "new"})
	if len(changes) != 2 || changes[0].Key != "b" || changes[1].Key != "d" {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestStoreVersionsAndPrune(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	for i := 0; i < KeepVersions+2; i++ {
		pack := v2branding.ProductDefault()
		pack.Product.Name = "Brand " + strings.Repeat("x", i+1)
		data, _, _ := svc.Export(ctx, pack)
		if _, err := svc.Store(ctx, data); err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
	}
	versions, err := svc.Versions(ctx)
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(versions) != KeepVersions {
		t.Fatalf("kept %d versions, want %d", len(versions), KeepVersions)
	}
	if versions[0].Product != "Brand "+strings.Repeat("x", KeepVersions+2) {
		t.Errorf("newest first: got %q", versions[0].Product)
	}
	data, err := svc.Load(ctx, versions[0].Digest)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if imported, problems := svc.Parse(data); len(problems) != 0 || imported.Pack.Product.Name != versions[0].Product {
		t.Fatalf("restored package differs: %v %+v", problems, imported)
	}
}
