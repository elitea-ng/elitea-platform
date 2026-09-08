// Package brandpackage builds and reads the BRANDING PACKAGE (ADR-0024,
// decision 9): the zip an administrator downloads from the Branding page,
// restyles offline, and uploads back.
//
// # Shape
//
//	manifest.json                 format, export time, deployment, pack digest
//	brand-pack.json               the pack; asset fields point at assets/…
//	assets/<kind>.<ext>           logo-full, logo-mark, favicon, login-art, logo-email
//	assets/fonts/<name>.woff2     the pack's fontFaces
//	schema/brand-pack.schema.json JSON Schema for an editor
//	preview/app.html              the offline previewer with the pack inlined
//	preview/login.html            the login page under this brand
//	preview/email-*.html          the e-mails under this brand
//	README.md                     every field, format and cap
//
// # Export is exact, import is strict
//
// Export serialises the EFFECTIVE pack the bootstrap route serves and the
// bytes the asset store holds, so the package is what users see. Import
// reads only the entries above under the single-upload rules — a zip-slip
// entry, an oversized or mis-typed asset, an SVG with a script, a pack the
// schema rejects — and reports every problem with the entry named before
// anything is stored. The dry run is the same code with the store call
// skipped, so what the report says is what the apply does.
//
// # What the database layer receives
//
// A package is a whole pack; the database layer is per-key. ValuesFromPack
// maps every scalar and asset field to its section key. Scheme tokens are
// carried over ONLY when the package states any and they differ from the
// product default's — an untouched export re-imports as "nothing hand-tuned"
// rather than pinning 406 tokens and switching hue derivation off.
package brandpackage

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	texttemplate "text/template"
	"time"

	v2branding "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/branding"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

//go:embed templates/README.md templates/brand-pack.schema.json
var templateFS embed.FS

// Format is the package format version manifest.json carries.
const Format = 1

// Manifest is manifest.json.
type Manifest struct {
	Format     int       `json:"format"`
	ExportedAt time.Time `json:"exported_at"`
	Deployment string    `json:"deployment,omitempty"`
	Product    string    `json:"product"`
	PackDigest string    `json:"pack_digest"`
	Generator  string    `json:"generator"`
}

// Previews are the renderers the export uses; any may be nil, in which case
// the corresponding preview is omitted and README says so.
type Previews struct {
	// Login renders the login page under a pack.
	Login func(pack *v2branding.Pack) ([]byte, error)
	// Email renders one e-mail kind ("invitation", "moderation") under a pack.
	Email func(pack *v2branding.Pack, kind string) (html string, err error)
	// App is the self-contained previewer HTML; the pack is inlined into it.
	App []byte
}

// Service builds and reads packages over an asset store.
type Service struct {
	assets     *v2branding.AssetStore
	previews   Previews
	deployment string
	now        func() time.Time
}

// New wires a Service. A nil or unavailable asset store still exports the
// pack (with no asset bytes) and refuses to import.
func New(assets *v2branding.AssetStore, previews Previews, deployment string) *Service {
	return &Service{assets: assets, previews: previews, deployment: deployment, now: time.Now}
}

// ErrAssetsUnavailable is returned by Apply when no asset store is wired.
var ErrAssetsUnavailable = errors.New("branding package: asset storage is not configured on this deployment")

// --- export --------------------------------------------------------------------

// assetField names one asset slot of the pack.
type assetField struct {
	name string // "logoFull" …
	kind string // asset kind
	key  string // section key
}

var assetFields = []assetField{
	{"logoFull", v2branding.KindLogoFull, platformconfig.KeyBrandingLogoFull},
	{"logoMark", v2branding.KindLogoMark, platformconfig.KeyBrandingLogoMark},
	{"favicon", v2branding.KindFavicon, platformconfig.KeyBrandingFavicon},
	{"loginArt", v2branding.KindLoginArt, platformconfig.KeyBrandingLoginArt},
	{"logoEmail", v2branding.KindLogoEmail, platformconfig.KeyBrandingLogoEmail},
}

func getAsset(p *v2branding.Pack, name string) string {
	switch name {
	case "logoFull":
		return p.Assets.LogoFull
	case "logoMark":
		return p.Assets.LogoMark
	case "favicon":
		return p.Assets.Favicon
	case "loginArt":
		return deref(p.Assets.LoginArt)
	case "logoEmail":
		return deref(p.Assets.LogoEmail)
	}
	return ""
}

func setAsset(p *v2branding.Pack, name, value string) {
	switch name {
	case "logoFull":
		p.Assets.LogoFull = value
	case "logoMark":
		p.Assets.LogoMark = value
	case "favicon":
		p.Assets.Favicon = value
	case "loginArt":
		p.Assets.LoginArt = optional(value)
	case "logoEmail":
		p.Assets.LogoEmail = optional(value)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func optional(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// Export renders the package for pack (nil means the product default). The
// returned name is the suggested download filename.
func (s *Service) Export(ctx context.Context, pack *v2branding.Pack) (data []byte, name string, err error) {
	if pack == nil {
		pack = v2branding.ProductDefault()
	}
	copied := clonePack(pack)

	var out bytes.Buffer
	archive := zip.NewWriter(&out)
	add := func(entry string, content []byte) error {
		w, err := archive.CreateHeader(&zip.FileHeader{Name: entry, Method: zip.Deflate, Modified: s.now()})
		if err != nil {
			return err
		}
		_, err = w.Write(content)
		return err
	}
	var notes []string

	// Assets: fetch the bytes the store holds and rewrite the reference to
	// the package entry. A reference that is not a stored asset (the
	// product's compiled placeholder, a file-layer path) stays as it is.
	for _, field := range assetFields {
		value := getAsset(copied, field.name)
		if value == "" {
			continue
		}
		kind, _, extension, ok := v2branding.ParseAssetPath(value)
		if !ok || kind != field.kind {
			notes = append(notes, fmt.Sprintf("`assets.%s` (%s) is a path on the deployment, not a file in this package", field.name, value))
			continue
		}
		content, _, err := s.assets.Get(ctx, value)
		if err != nil {
			notes = append(notes, fmt.Sprintf("`assets.%s` could not be read from storage and stays a deployment path", field.name))
			continue
		}
		entry := "assets/" + field.kind + "." + extension
		if err := add(entry, content); err != nil {
			return nil, "", err
		}
		setAsset(copied, field.name, entry)
	}
	for i := range copied.Typography.FontFaces {
		face := &copied.Typography.FontFaces[i]
		kind, _, extension, ok := v2branding.ParseAssetPath(face.URL)
		if !ok || kind != v2branding.KindFont {
			notes = append(notes, fmt.Sprintf("`typography.fontFaces[%d].url` is a path on the deployment, not a file in this package", i))
			continue
		}
		content, _, err := s.assets.Get(ctx, face.URL)
		if err != nil {
			notes = append(notes, fmt.Sprintf("`typography.fontFaces[%d]` could not be read from storage", i))
			continue
		}
		entry := fmt.Sprintf("assets/fonts/%s-%d.%s", slug(face.Family), i+1, extension)
		if err := add(entry, content); err != nil {
			return nil, "", err
		}
		face.URL = entry
	}

	packJSON, err := json.MarshalIndent(copied, "", "  ")
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(packJSON)
	manifest := Manifest{
		Format:     Format,
		ExportedAt: s.now().UTC(),
		Deployment: s.deployment,
		Product:    copied.Product.Name,
		PackDigest: hex.EncodeToString(digest[:]),
		Generator:  "elitea-main",
	}
	manifestJSON, _ := json.MarshalIndent(manifest, "", "  ")
	if err := add("manifest.json", manifestJSON); err != nil {
		return nil, "", err
	}
	if err := add("brand-pack.json", append(packJSON, '\n')); err != nil {
		return nil, "", err
	}
	schema, _ := templateFS.ReadFile("templates/brand-pack.schema.json")
	if err := add("schema/brand-pack.schema.json", schema); err != nil {
		return nil, "", err
	}

	// Previews render under the ORIGINAL pack (deployment paths resolve in a
	// browser only when served); the offline previewer gets the package
	// copy so its relative asset references resolve from disk.
	if s.previews.Login != nil {
		if page, err := s.previews.Login(pack); err == nil {
			if err := add("preview/login.html", page); err != nil {
				return nil, "", err
			}
		} else {
			notes = append(notes, "the login preview could not be rendered: "+err.Error())
		}
	} else {
		notes = append(notes, "no login preview on this deployment")
	}
	if s.previews.Email != nil {
		for _, kind := range []string{"invitation", "moderation"} {
			if page, err := s.previews.Email(pack, kind); err == nil {
				if err := add("preview/email-"+kind+".html", []byte(page)); err != nil {
					return nil, "", err
				}
			} else {
				notes = append(notes, "the "+kind+" e-mail preview could not be rendered: "+err.Error())
			}
		}
	} else {
		notes = append(notes, "no e-mail previews on this deployment")
	}
	if len(s.previews.App) > 0 {
		if err := add("preview/app.html", InlinePack(s.previews.App, packJSON)); err != nil {
			return nil, "", err
		}
	} else {
		notes = append(notes, "no offline previewer (preview/app.html) on this deployment: the image was built without the brand-preview entry")
	}

	readme, err := renderREADME(manifest, notes)
	if err != nil {
		return nil, "", err
	}
	if err := add("README.md", readme); err != nil {
		return nil, "", err
	}
	if err := archive.Close(); err != nil {
		return nil, "", err
	}
	return out.Bytes(), slug(copied.Product.Name) + "-branding.zip", nil
}

// InlinePack embeds pack JSON into the previewer HTML as
// `<script type="application/json" id="brand-pack">…</script>` before
// `</head>`, escaping the one sequence that could end the element early.
func InlinePack(previewer, packJSON []byte) []byte {
	safe := bytes.ReplaceAll(packJSON, []byte("</"), []byte(`<\/`))
	element := append([]byte(`<script type="application/json" id="brand-pack">`), safe...)
	element = append(element, []byte("</script>\n")...)
	if i := bytes.Index(previewer, []byte("</head>")); i >= 0 {
		return append(append(append([]byte{}, previewer[:i]...), element...), previewer[i:]...)
	}
	return append(element, previewer...)
}

func renderREADME(manifest Manifest, notes []string) ([]byte, error) {
	source, _ := templateFS.ReadFile("templates/README.md")
	tmpl, err := texttemplate.New("README.md").Parse(string(source))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, map[string]any{
		"ProductName": manifest.Product,
		"ExportedAt":  manifest.ExportedAt.Format(time.RFC3339),
		"Deployment":  orDefault(manifest.Deployment, "this deployment"),
		"Format":      manifest.Format,
	}); err != nil {
		return nil, err
	}
	if len(notes) > 0 {
		out.WriteString("\n## Notes from the export\n\n")
		for _, note := range notes {
			out.WriteString("- " + note + "\n")
		}
	}
	return out.Bytes(), nil
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

var slugPattern = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(slugPattern.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		return "brand"
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func clonePack(p *v2branding.Pack) *v2branding.Pack {
	data, _ := json.Marshal(p)
	var copied v2branding.Pack
	_ = json.Unmarshal(data, &copied)
	// Optional pointers survive Marshal/Unmarshal; the string records are
	// fresh maps. FontFaces is a fresh slice.
	return &copied
}

// --- import --------------------------------------------------------------------

// Problem is one refusal, with the entry it concerns.
type Problem struct {
	Entry  string `json:"entry"`
	Reason string `json:"reason"`
}

// AssetFile is one asset the package supplies for a field.
type AssetFile struct {
	Field    string // "logoFull" … or "fontFaces[0]"
	Kind     string
	Filename string
	Data     []byte
}

// Imported is a parsed package.
type Imported struct {
	Manifest *Manifest
	Pack     *v2branding.Pack
	Assets   []AssetFile
	// Warnings are non-fatal observations (extra entries ignored, …).
	Warnings []string
}

const (
	maxEntryBytes    = 1024 * 1024 // README, schema, previews, manifest, pack
	maxEntries       = 64
	maxPackJSONBytes = 512 * 1024
)

var allowedEntry = regexp.MustCompile(`^(manifest\.json|brand-pack\.json|README\.md|schema/[a-z0-9.-]+\.json|preview/[a-z0-9-]+\.html|assets/[a-z0-9-]+\.[a-z0-9]{1,8}|assets/fonts/[A-Za-z0-9._-]+\.woff2)$`)

// Parse reads a package strictly. It returns the parsed content and every
// problem found; a non-empty problem list means the package must not be
// applied. Nothing is stored here.
func (s *Service) Parse(data []byte) (*Imported, []Problem) {
	var problems []Problem
	fail := func(entry, format string, args ...any) {
		problems = append(problems, Problem{Entry: entry, Reason: fmt.Sprintf(format, args...)})
	}
	if int64(len(data)) > v2branding.MaxPackageBytes {
		fail("(package)", "the package is %d KiB; the limit is %d KiB", len(data)/1024, v2branding.MaxPackageBytes/1024)
		return nil, problems
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		fail("(package)", "not a zip archive: %v", err)
		return nil, problems
	}
	if len(reader.File) > maxEntries {
		fail("(package)", "too many entries (%d; the limit is %d)", len(reader.File), maxEntries)
		return nil, problems
	}

	files := map[string][]byte{}
	var warnings []string
	for _, entry := range reader.File {
		name := entry.Name
		// A directory entry (`assets/`) is what Finder, Explorer, JSZip and
		// most archivers write before the files under it. It carries nothing
		// and is skipped — but its shape is still checked first, because
		// `../x/` is a zip-slip attempt whatever its type. The trailing slash
		// is the directory marker, not part of the path being judged; before
		// this was split out, `path.Clean("assets/") != "assets/"` refused
		// every package a customer had re-zipped with ordinary tools.
		isDir := entry.FileInfo().IsDir() || strings.HasSuffix(name, "/")
		shape := strings.TrimSuffix(name, "/")
		// Zip-slip and shape: forward slashes only, no absolute path, no
		// parent segment, and only the names the format defines.
		if shape == "" || strings.Contains(shape, "\\") || strings.HasPrefix(shape, "/") || strings.Contains(shape, "..") || path.Clean(shape) != shape {
			fail(name, "entry name is not a plain relative path")
			continue
		}
		if isDir {
			continue
		}
		if !allowedEntry.MatchString(name) {
			warnings = append(warnings, "ignored entry "+name)
			continue
		}
		if entry.Mode()&0o170000 != 0 && !entry.Mode().IsRegular() {
			fail(name, "entry is not a regular file")
			continue
		}
		cap := int64(maxEntryBytes)
		if strings.HasPrefix(name, "assets/") {
			cap = v2branding.MaxPackageBytes
		}
		if int64(entry.UncompressedSize64) > cap {
			fail(name, "entry is %d KiB; the limit is %d KiB", entry.UncompressedSize64/1024, cap/1024)
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			fail(name, "could not be read: %v", err)
			continue
		}
		content, err := io.ReadAll(io.LimitReader(rc, cap+1))
		_ = rc.Close()
		if err != nil || int64(len(content)) > cap {
			fail(name, "could not be read within its size limit")
			continue
		}
		files[name] = content
	}

	imported := &Imported{Warnings: warnings}
	if raw, ok := files["manifest.json"]; ok {
		var manifest Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			fail("manifest.json", "not valid JSON: %v", err)
		} else if manifest.Format != Format {
			fail("manifest.json", "format %d is not supported (this platform reads format %d)", manifest.Format, Format)
		} else {
			imported.Manifest = &manifest
		}
	} else {
		warnings = append(warnings, "no manifest.json; the package is read as format 1")
	}

	packJSON, ok := files["brand-pack.json"]
	if !ok {
		fail("brand-pack.json", "missing")
		return imported, problems
	}
	if len(packJSON) > maxPackJSONBytes {
		fail("brand-pack.json", "larger than %d KiB", maxPackJSONBytes/1024)
		return imported, problems
	}
	pack, err := v2branding.ParsePack(packJSON)
	if err != nil {
		fail("brand-pack.json", "%v", err)
		return imported, problems
	}
	imported.Pack = pack
	// The package is NAMED by the pack it carries, not by the manifest an
	// export wrote. A customer restyles brand-pack.json offline and never
	// touches manifest.json, so the manifest still says the product it was
	// exported from; the import dialog and the versions list would then
	// label an "Acme Corp" package "Elitea". The manifest's other fields
	// (export time, deployment, digest) remain the record of where it came
	// from, which is what they are for.
	if imported.Manifest == nil {
		imported.Manifest = &Manifest{Format: Format}
	}
	if name := strings.TrimSpace(pack.Product.Name); name != "" {
		imported.Manifest.Product = name
	}

	// Assets: a reference into the package must exist and pass the kind's
	// rules; a deployment path is kept; anything else is refused.
	for _, field := range assetFields {
		value := getAsset(pack, field.name)
		if value == "" {
			continue
		}
		if strings.HasPrefix(value, "assets/") {
			content, present := files[value]
			if !present {
				fail(value, "referenced by assets.%s but not in the package", field.name)
				continue
			}
			if _, _, err := v2branding.ValidateAsset(field.kind, value, content); err != nil {
				fail(value, "%v", err)
				continue
			}
			imported.Assets = append(imported.Assets, AssetFile{Field: field.name, Kind: field.kind, Filename: value, Data: content})
			continue
		}
		if !isDeploymentPath(value) && !isPlaceholderPath(value) {
			fail("brand-pack.json", "assets.%s must be a file in the package (assets/…), a path on the deployment (/…) or the product placeholder (./brand/…), got %q", field.name, value)
		}
	}
	if len(pack.Typography.FontFaces) > 2 {
		fail("brand-pack.json", "typography.fontFaces may hold at most 2 faces")
	}
	for i, face := range pack.Typography.FontFaces {
		label := fmt.Sprintf("fontFaces[%d]", i)
		if strings.HasPrefix(face.URL, "assets/") {
			content, present := files[face.URL]
			if !present {
				fail(face.URL, "referenced by typography.%s but not in the package", label)
				continue
			}
			if _, _, err := v2branding.ValidateAsset(v2branding.KindFont, face.URL, content); err != nil {
				fail(face.URL, "%v", err)
				continue
			}
			imported.Assets = append(imported.Assets, AssetFile{Field: label, Kind: v2branding.KindFont, Filename: face.URL, Data: content})
			continue
		}
		if kind, _, _, ok := v2branding.ParseAssetPath(face.URL); !ok || kind != v2branding.KindFont {
			fail("brand-pack.json", "typography.%s.url must be a font file in the package (assets/fonts/…) or an uploaded font asset path", label)
		}
	}
	imported.Warnings = warnings
	return imported, problems
}

// isPlaceholderPath admits the product default's document-relative
// placeholders (`./brand/logo-full.svg`): they mean "the compiled artwork"
// and import as "inherit".
func isPlaceholderPath(value string) bool {
	return strings.HasPrefix(value, "./") && !strings.Contains(value, "..") && isDeploymentPath("/"+strings.TrimPrefix(value, "./"))
}

func isDeploymentPath(value string) bool {
	if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || len(value) > 512 {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(`"'()\<>`, r) {
			return false
		}
	}
	return true
}

// Apply stores the package's assets and returns the section values the
// database layer should hold for this pack. It stores nothing when the
// parse reported problems.
func (s *Service) Apply(ctx context.Context, imported *Imported) (map[string]any, error) {
	if imported == nil || imported.Pack == nil {
		return nil, errors.New("branding package: nothing to apply")
	}
	pack := clonePack(imported.Pack)
	if len(imported.Assets) > 0 && (s.assets == nil || !s.assets.Available()) {
		return nil, ErrAssetsUnavailable
	}
	for _, file := range imported.Assets {
		stored, err := s.assets.Put(ctx, file.Kind, file.Filename, file.Data)
		if err != nil {
			return nil, fmt.Errorf("storing %s: %w", file.Filename, err)
		}
		if strings.HasPrefix(file.Field, "fontFaces[") {
			var index int
			_, _ = fmt.Sscanf(file.Field, "fontFaces[%d]", &index)
			if index < len(pack.Typography.FontFaces) {
				pack.Typography.FontFaces[index].URL = stored.Path
			}
			continue
		}
		setAsset(pack, file.Field, stored.Path)
	}
	return ValuesFromPack(pack, v2branding.ProductDefault()), nil
}

// ValuesFromPack maps a whole pack onto the branding section's keys. Every
// scalar and asset is set; scheme tokens only when they differ from base's.
func ValuesFromPack(pack, base *v2branding.Pack) map[string]any {
	values := map[string]any{
		platformconfig.KeyBrandingProductName:      pack.Product.Name,
		platformconfig.KeyBrandingProductShortName: pack.Product.ShortName,
		platformconfig.KeyBrandingProductTagline:   deref(pack.Product.Tagline),
		platformconfig.KeyBrandingDocsURL:          deref(pack.Product.DocsURL),
		platformconfig.KeyBrandingSupportURL:       deref(pack.Product.SupportURL),
		platformconfig.KeyBrandingSenderName:       deref(pack.Product.SenderName),
		platformconfig.KeyBrandingSupportEmail:     deref(pack.Product.SupportEmail),
		platformconfig.KeyBrandingHue:              pack.Brand.Hue,
		platformconfig.KeyBrandingOnBrand:          deref(pack.Brand.OnBrand),
		platformconfig.KeyBrandingFontFamily:       pack.Typography.FontFamily,
		platformconfig.KeyBrandingFontFamilyMono:   pack.Typography.FontFamilyMono,
		platformconfig.KeyBrandingBaseSize:         pack.Typography.BaseSize,
		platformconfig.KeyBrandingScale:            pack.Typography.Scale,
		platformconfig.KeyBrandingRadiusSm:         pack.Shape.RadiusSm,
		platformconfig.KeyBrandingRadiusMd:         pack.Shape.RadiusMd,
		platformconfig.KeyBrandingRadiusLg:         pack.Shape.RadiusLg,
		platformconfig.KeyBrandingRadiusPill:       pack.Shape.RadiusPill,
		platformconfig.KeyBrandingDensity:          pack.Shape.Density,
		platformconfig.KeyBrandingLogoFull:         assetValue(pack.Assets.LogoFull),
		platformconfig.KeyBrandingLogoMark:         assetValue(pack.Assets.LogoMark),
		platformconfig.KeyBrandingFavicon:          assetValue(pack.Assets.Favicon),
		platformconfig.KeyBrandingLoginArt:         assetValue(deref(pack.Assets.LoginArt)),
		platformconfig.KeyBrandingLogoEmail:        assetValue(deref(pack.Assets.LogoEmail)),
	}
	faces := make([]any, 0, len(pack.Typography.FontFaces))
	for _, face := range pack.Typography.FontFaces {
		entry := map[string]any{"family": face.Family, "url": face.URL}
		if face.Weight != nil {
			entry["weight"] = *face.Weight
		}
		if face.Style != nil {
			entry["style"] = *face.Style
		}
		faces = append(faces, entry)
	}
	values[platformconfig.KeyBrandingFontFaces] = faces

	tokens := map[string]any{}
	if base == nil || !sameRecord(pack.Schemes.Light, base.Schemes.Light) || !sameRecord(pack.Schemes.Dark, base.Schemes.Dark) {
		if len(pack.Schemes.Light) > 0 {
			tokens["light"] = toAnyMap(pack.Schemes.Light)
		}
		if len(pack.Schemes.Dark) > 0 {
			tokens["dark"] = toAnyMap(pack.Schemes.Dark)
		}
		if len(pack.Schemes.HC) > 0 {
			tokens["hc"] = toAnyMap(pack.Schemes.HC)
		}
	}
	values[platformconfig.KeyBrandingSchemeTokens] = tokens
	return values
}

// assetValue keeps only values the section accepts: a root-relative
// deployment path. The product's compiled `./brand/…` placeholders and an
// unresolved `assets/…` reference become "inherit".
func assetValue(v string) string {
	if isDeploymentPath(v) {
		return v
	}
	return ""
}

func sameRecord(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Change is one key whose value an import would alter.
type Change struct {
	Key      string `json:"key"`
	Current  any    `json:"current"`
	Incoming any    `json:"incoming"`
}

// Diff lists the keys whose incoming value differs from the current one, in
// key order. Values are compared by their JSON encoding.
func Diff(current, incoming map[string]any) []Change {
	keys := make([]string, 0, len(incoming))
	for k := range incoming {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var changes []Change
	for _, k := range keys {
		before, _ := json.Marshal(current[k])
		after, _ := json.Marshal(incoming[k])
		if !bytes.Equal(before, after) {
			changes = append(changes, Change{Key: k, Current: current[k], Incoming: incoming[k]})
		}
	}
	return changes
}

// --- rollback storage ------------------------------------------------------------

// KeepVersions is how many imported packages are kept for restore.
const KeepVersions = 5

// Version is one stored package.
type Version struct {
	Digest     string    `json:"digest"`
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	StoredAt   time.Time `json:"stored_at"`
	Product    string    `json:"product,omitempty"`
	ExportedAt string    `json:"exported_at,omitempty"`
}

// Store keeps a package for rollback and prunes to KeepVersions.
func (s *Service) Store(ctx context.Context, data []byte) (Version, error) {
	if s.assets == nil || !s.assets.Available() {
		return Version{}, ErrAssetsUnavailable
	}
	stored, err := s.assets.Put(ctx, v2branding.KindPackage, "package.zip", data)
	if err != nil {
		return Version{}, err
	}
	versions, err := s.Versions(ctx)
	if err == nil {
		for i, v := range versions {
			if i >= KeepVersions && v.Digest != stored.Digest {
				_ = s.assets.Delete(ctx, v.Path)
			}
		}
	}
	return Version{Digest: stored.Digest, Path: stored.Path, Size: stored.Size, StoredAt: s.now()}, nil
}

// Versions lists stored packages, newest first.
func (s *Service) Versions(ctx context.Context) ([]Version, error) {
	if s.assets == nil || !s.assets.Available() {
		return nil, ErrAssetsUnavailable
	}
	infos, err := s.assets.ListInfos(ctx)
	if err != nil {
		return nil, err
	}
	var versions []Version
	for _, info := range infos {
		if info.Kind != v2branding.KindPackage {
			continue
		}
		_, digest, _, _ := v2branding.ParseAssetPath(info.Path)
		version := Version{Digest: digest, Path: info.Path, Size: info.Size, StoredAt: info.LastModified}
		if data, _, err := s.assets.Get(ctx, info.Path); err == nil {
			if imported, problems := s.Parse(data); len(problems) == 0 && imported.Manifest != nil {
				version.Product = imported.Manifest.Product
				version.ExportedAt = imported.Manifest.ExportedAt.Format(time.RFC3339)
			}
		}
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].StoredAt.After(versions[j].StoredAt) })
	return versions, nil
}

// Load reads a stored package by digest.
func (s *Service) Load(ctx context.Context, digest string) ([]byte, error) {
	if s.assets == nil || !s.assets.Available() {
		return nil, ErrAssetsUnavailable
	}
	data, _, err := s.assets.Get(ctx, v2branding.AssetPath(v2branding.KindPackage, digest, "zip"))
	return data, err
}
