package branding

// Brand ASSETS — the uploaded logo, mark, favicon, login artwork, e-mail logo
// and font files a brand pack references (ADR-0024, decision 3).
//
// # Why uploaded blobs on this origin, and nothing else
//
// Three facts decide the shape. The web app's theme gate forbids external
// font and image origins, so a CDN URL is not an option the UI would accept.
// An inline data: URI collides with the 64 KiB per-field cap of the admin
// config writer and with the repository's no-binaries gate. And the login
// page's CSP is `default-src 'none'`, which only a same-origin path satisfies
// cleanly. So an asset is a blob in object storage under the PLATFORM scope
// (storage.PlatformScopeID — it belongs to no project), served anonymously at
//
//	/api/v2/branding/assets/{kind}/{digest}.{ext}
//
// The digest in the path is the SHA-256 of the bytes: the URL is a content
// address, so it is cached for a year as immutable and an upload of the same
// bytes twice is one object. It sits under /api/v2 like bootstrap.js so every
// deployed edge already forwards it and the edge-auth public rule is the
// only edge change.
//
// # What is refused, and why the download route hardens anyway
//
// Every kind has an extension allowlist and a byte cap, and the CONTENT is
// sniffed: a PNG must start with the PNG signature, a WOFF2 with `wOF2`, an
// SVG must parse as XML whose root element is `svg`. An SVG that carries a
// script, a foreignObject, an event-handler attribute, an external or
// javascript: reference, a stylesheet with url()/@import, or a DOCTYPE is
// REFUSED with the reason named — not stripped. Rewriting XML is where
// sanitisers go wrong (Go's encoder reorders namespaces), and an operator
// with a logo that fails this check can fix the file in seconds; an operator
// whose logo was silently altered cannot tell what changed.
//
// The download route then applies the icon route's hardening regardless:
// nosniff, `Content-Security-Policy: default-src 'none'; sandbox` and
// `Content-Disposition: attachment`. None of the three affects the way the
// product consumes an asset — `<img src>`, `<link rel="icon">`, `@font-face
// src` are subresource loads — and all three make a direct navigation to an
// asset inert even for an object stored before a check existed.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// AssetBucket is the platform-scoped bucket every brand asset lives in.
const AssetBucket = "branding"

// AssetPathPrefix is the public URL prefix of the download route.
const AssetPathPrefix = "/api/v2/branding/assets/"

// Asset kinds. The string is the URL segment and the storage key prefix.
const (
	KindLogoFull  = "logo-full"
	KindLogoMark  = "logo-mark"
	KindLogoEmail = "logo-email"
	KindFavicon   = "favicon"
	KindLoginArt  = "login-art"
	KindFont      = "font"
	// KindPackage stores an imported branding package (a zip) so a previous
	// brand can be restored (ADR-0024 decision 9). Never served by the
	// download route: ParseAssetPath knows it, Download refuses it.
	KindPackage = "package"
)

// kindSpec is one kind's rules: which extensions it accepts and how large
// its bytes may be. The caps are generous against any real asset and tight
// enough that an upload cannot become a per-page-load cost: a logo is drawn
// on every screen, a font is fetched on every cold load.
type kindSpec struct {
	extensions map[string]string // extension -> content type
	maxBytes   int64
}

const (
	maxImageBytes   = 512 * 1024
	maxFaviconBytes = 64 * 1024
	maxFontBytes    = 300 * 1024
	// MaxPackageBytes bounds a branding package: every asset at its cap plus
	// the previews and the pack is well under it.
	MaxPackageBytes = 4 * 1024 * 1024
)

var (
	imageExtensions = map[string]string{
		"svg":  "image/svg+xml",
		"png":  "image/png",
		"webp": "image/webp",
	}
	faviconExtensions = map[string]string{
		"svg": "image/svg+xml",
		"png": "image/png",
		"ico": "image/x-icon",
	}
	rasterExtensions = map[string]string{
		"png":  "image/png",
		"webp": "image/webp",
	}
	fontExtensions = map[string]string{
		"woff2": "font/woff2",
	}
	packageExtensions = map[string]string{
		"zip": "application/zip",
	}
)

// kinds is the closed set. An unknown kind is a 404 on both routes.
var kinds = map[string]kindSpec{
	KindLogoFull:  {extensions: imageExtensions, maxBytes: maxImageBytes},
	KindLogoMark:  {extensions: imageExtensions, maxBytes: maxImageBytes},
	KindLoginArt:  {extensions: imageExtensions, maxBytes: maxImageBytes},
	KindFavicon:   {extensions: faviconExtensions, maxBytes: maxFaviconBytes},
	KindLogoEmail: {extensions: rasterExtensions, maxBytes: maxImageBytes},
	KindFont:      {extensions: fontExtensions, maxBytes: maxFontBytes},
	KindPackage:   {extensions: packageExtensions, maxBytes: MaxPackageBytes},
}

// AssetInfo describes one stored object for listings.
type AssetInfo struct {
	Path         string
	Kind         string
	Size         int64
	LastModified time.Time
}

// KnownKind reports whether kind is one the routes serve.
func KnownKind(kind string) bool {
	_, ok := kinds[kind]
	return ok
}

// MaxAssetBytes returns the byte cap of a kind, 0 for an unknown one — the
// upload handler uses it to bound the request body before reading it.
func MaxAssetBytes(kind string) int64 {
	return kinds[kind].maxBytes
}

// Asset describes one stored asset.
type Asset struct {
	Kind        string `json:"kind"`
	Digest      string `json:"digest"`
	Extension   string `json:"extension"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	// Path is the public same-origin URL — the value the branding section's
	// asset fields and font_faces[].url take.
	Path string `json:"path"`
}

// AssetError is a refusal the operator is shown. Status is 400 for a bad
// file, 413 for an oversized one, 415 for an extension or content the kind
// does not accept.
type AssetError struct {
	Status int
	Reason string
}

func (e *AssetError) Error() string { return e.Reason }

func refuse(status int, format string, args ...any) error {
	return &AssetError{Status: status, Reason: fmt.Sprintf(format, args...)}
}

var assetFilePattern = regexp.MustCompile(`^([0-9a-f]{64})\.([a-z0-9]{1,8})$`)

// AssetPath renders the public path of an asset.
func AssetPath(kind, digest, extension string) string {
	return AssetPathPrefix + kind + "/" + digest + "." + extension
}

// ParseAssetPath validates a value the branding section stores in an asset
// field and returns its parts. It accepts ONLY the shape this package hands
// out: a known kind, a 64-hex digest and an extension the kind allows.
func ParseAssetPath(path string) (kind, digest, extension string, ok bool) {
	rest, found := strings.CutPrefix(path, AssetPathPrefix)
	if !found {
		return "", "", "", false
	}
	kind, file, found := strings.Cut(rest, "/")
	if !found {
		return "", "", "", false
	}
	spec, known := kinds[kind]
	if !known {
		return "", "", "", false
	}
	m := assetFilePattern.FindStringSubmatch(file)
	if m == nil {
		return "", "", "", false
	}
	if _, allowed := spec.extensions[m[2]]; !allowed {
		return "", "", "", false
	}
	return kind, m[1], m[2], true
}

// AssetStore stores and serves brand assets over an object store.
type AssetStore struct {
	store storage.ObjectStore
}

// NewAssetStore wraps store; a nil store yields a store whose every call
// answers ErrAssetStorageUnavailable, so the routes can say so.
func NewAssetStore(store storage.ObjectStore) *AssetStore {
	return &AssetStore{store: store}
}

// ErrAssetStorageUnavailable is returned when no object store is configured.
var ErrAssetStorageUnavailable = errors.New("branding asset storage is not configured")

// Available reports whether uploads and downloads can be served.
func (s *AssetStore) Available() bool { return s != nil && s.store != nil }

func assetRef(kind, digest, extension string) (storage.ObjectRef, error) {
	return storage.NewPlatformObjectRef(AssetBucket, kind+"/"+digest+"."+extension)
}

// Put validates and stores one asset. filename supplies the extension; the
// bytes decide everything else. The same bytes stored twice are one object.
func (s *AssetStore) Put(ctx context.Context, kind, filename string, data []byte) (Asset, error) {
	if !s.Available() {
		return Asset{}, ErrAssetStorageUnavailable
	}
	extension, contentType, err := ValidateAsset(kind, filename, data)
	if err != nil {
		return Asset{}, err
	}

	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	ref, err := assetRef(kind, digest, extension)
	if err != nil {
		return Asset{}, err
	}
	if _, err := s.store.Put(ctx, ref, bytes.NewReader(data), storage.PutOptions{
		ContentType:   contentType,
		ContentLength: int64(len(data)),
	}); err != nil {
		return Asset{}, fmt.Errorf("storing %s: %w", kind, err)
	}
	return Asset{
		Kind:        kind,
		Digest:      digest,
		Extension:   extension,
		ContentType: contentType,
		Size:        int64(len(data)),
		Path:        AssetPath(kind, digest, extension),
	}, nil
}

// ValidateAsset applies a kind's rules to a file without storing it: the
// extension allowlist, the byte cap, and the content sniff. The package
// importer runs it over every entry before it stores anything.
func ValidateAsset(kind, filename string, data []byte) (extension, contentType string, err error) {
	spec, known := kinds[kind]
	if !known {
		return "", "", refuse(http.StatusNotFound, "unknown asset kind %q", kind)
	}
	extension = strings.ToLower(strings.TrimPrefix(extensionOf(filename), "."))
	contentType, allowed := spec.extensions[extension]
	if !allowed {
		return "", "", refuse(http.StatusUnsupportedMediaType,
			"%s accepts %s, not %q", kind, extensionList(spec), extensionOrNone(extension))
	}
	if int64(len(data)) > spec.maxBytes {
		return "", "", refuse(http.StatusRequestEntityTooLarge,
			"%s must be at most %d KiB, got %d KiB", kind, spec.maxBytes/1024, (int64(len(data))+1023)/1024)
	}
	if len(data) == 0 {
		return "", "", refuse(http.StatusBadRequest, "the file is empty")
	}
	if err := checkContent(extension, data); err != nil {
		return "", "", err
	}
	return extension, contentType, nil
}

// Get reads one asset's bytes by its public path.
func (s *AssetStore) Get(ctx context.Context, path string) ([]byte, string, error) {
	if !s.Available() {
		return nil, "", ErrAssetStorageUnavailable
	}
	kind, digest, extension, ok := ParseAssetPath(path)
	if !ok {
		return nil, "", storage.ErrNotFound
	}
	ref, err := assetRef(kind, digest, extension)
	if err != nil {
		return nil, "", err
	}
	body, _, err := s.store.Get(ctx, ref, nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, kinds[kind].maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	return data, kinds[kind].extensions[extension], nil
}

// ListInfos returns every stored asset with its size and modification time.
func (s *AssetStore) ListInfos(ctx context.Context) ([]AssetInfo, error) {
	if !s.Available() {
		return nil, ErrAssetStorageUnavailable
	}
	bucket, err := storage.NewPlatformBucketRef(AssetBucket)
	if err != nil {
		return nil, err
	}
	var infos []AssetInfo
	token := ""
	for {
		page, err := s.store.List(ctx, storage.ListQuery{Bucket: bucket, ContinuationToken: token})
		if err != nil {
			return nil, err
		}
		for _, object := range page.Objects {
			kind, file, found := strings.Cut(object.Key, "/")
			if !found {
				continue
			}
			path := AssetPathPrefix + kind + "/" + file
			if _, _, _, ok := ParseAssetPath(path); ok {
				infos = append(infos, AssetInfo{Path: path, Kind: kind, Size: object.Size, LastModified: object.LastModified})
			}
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return infos, nil
		}
		token = page.NextContinuationToken
	}
}

// Delete removes one asset by its public path. Unknown paths are ignored.
func (s *AssetStore) Delete(ctx context.Context, path string) error {
	if !s.Available() {
		return ErrAssetStorageUnavailable
	}
	kind, digest, extension, ok := ParseAssetPath(path)
	if !ok {
		return nil
	}
	ref, err := assetRef(kind, digest, extension)
	if err != nil {
		return err
	}
	return s.store.Delete(ctx, ref)
}

// List returns the public paths of every stored asset. A backend without
// listing returns storage.ErrNotSupported and the caller skips collection.
func (s *AssetStore) List(ctx context.Context) ([]string, error) {
	if !s.Available() {
		return nil, ErrAssetStorageUnavailable
	}
	bucket, err := storage.NewPlatformBucketRef(AssetBucket)
	if err != nil {
		return nil, err
	}
	var paths []string
	token := ""
	for {
		page, err := s.store.List(ctx, storage.ListQuery{Bucket: bucket, ContinuationToken: token})
		if err != nil {
			return nil, err
		}
		for _, object := range page.Objects {
			kind, file, found := strings.Cut(object.Key, "/")
			if !found {
				continue
			}
			if _, _, _, ok := ParseAssetPath(AssetPathPrefix + kind + "/" + file); ok {
				paths = append(paths, AssetPathPrefix+kind+"/"+file)
			}
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return paths, nil
		}
		token = page.NextContinuationToken
	}
}

// Download serves GET/HEAD /api/v2/branding/assets/{kind}/{file}. It is
// mounted OUTSIDE the authenticated group: a browser's <img>, <link
// rel="icon"> and @font-face fetches carry no credential.
func (s *AssetStore) Download(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	file := chi.URLParam(r, "file")
	kind, digest, extension, ok := ParseAssetPath(AssetPathPrefix + kind + "/" + file)
	if !ok || kind == KindPackage {
		// A stored package is an admin-only artefact (the restore route reads
		// it); it is never a browser subresource.
		http.NotFound(w, r)
		return
	}
	if !s.Available() {
		http.NotFound(w, r)
		return
	}
	ref, err := assetRef(kind, digest, extension)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	body, info, err := s.store.Get(r.Context(), ref, nil)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = body.Close() }()

	// Content type from the allowlisted extension, never from the backend:
	// the extension is what ParseAssetPath admitted, so it is the only
	// attacker-independent fact about the object.
	w.Header().Set("Content-Type", kinds[kind].extensions[extension])
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Content-Disposition", "attachment")
	// Content-addressed: the bytes behind this URL can never change.
	w.Header().Set("Cache-Control", cacheImmutable)
	w.Header().Set("ETag", `"`+digest+`"`)
	if info.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, body)
}

// --- content checks ----------------------------------------------------------

func extensionOf(filename string) string {
	if i := strings.LastIndexAny(filename, "/\\"); i >= 0 {
		filename = filename[i+1:]
	}
	if i := strings.LastIndexByte(filename, '.'); i > 0 && i < len(filename)-1 {
		return filename[i:]
	}
	return ""
}

func extensionOrNone(extension string) string {
	if extension == "" {
		return "a file without an extension"
	}
	return "." + extension
}

func extensionList(spec kindSpec) string {
	names := make([]string, 0, len(spec.extensions))
	for ext := range spec.extensions {
		names = append(names, "."+ext)
	}
	sortStrings(names)
	return strings.Join(names, ", ")
}

func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// checkContent verifies the bytes are what the extension claims.
func checkContent(extension string, data []byte) error {
	switch extension {
	case "png":
		if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
			return refuse(http.StatusUnsupportedMediaType, "the file is not a PNG image")
		}
	case "webp":
		if len(data) < 12 || !bytes.HasPrefix(data, []byte("RIFF")) || string(data[8:12]) != "WEBP" {
			return refuse(http.StatusUnsupportedMediaType, "the file is not a WebP image")
		}
	case "ico":
		if !bytes.HasPrefix(data, []byte{0, 0, 1, 0}) {
			return refuse(http.StatusUnsupportedMediaType, "the file is not an ICO image")
		}
	case "woff2":
		if !bytes.HasPrefix(data, []byte("wOF2")) {
			return refuse(http.StatusUnsupportedMediaType, "the file is not a WOFF2 font")
		}
	case "zip":
		if !bytes.HasPrefix(data, []byte("PK\x03\x04")) {
			return refuse(http.StatusUnsupportedMediaType, "the file is not a zip archive")
		}
	case "svg":
		return checkSVG(data)
	default:
		return refuse(http.StatusUnsupportedMediaType, "unsupported extension %q", extension)
	}
	return nil
}

// Elements an SVG rendered in this origin must not carry. `style` is checked
// by content rather than listed here.
var forbiddenSVGElements = map[string]bool{
	"script": true, "foreignobject": true, "iframe": true, "embed": true,
	"object": true, "link": true, "meta": true, "html": true, "body": true,
	"handler": true, "listener": true,
}

// checkSVG parses the document and refuses anything that could execute or
// fetch in the app's origin. See the file doc for why it refuses rather than
// rewrites.
func checkSVG(data []byte) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	// Entities other than the five predefined ones are a DOCTYPE feature and
	// DOCTYPEs are refused below, so strict mode is right: an undeclared
	// entity is a malformed file, not a file to guess at.
	decoder.Strict = true
	root := ""
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return refuse(http.StatusUnsupportedMediaType, "the file is not well-formed SVG: %v", err)
		}
		switch t := token.(type) {
		case xml.Directive:
			return refuse(http.StatusUnsupportedMediaType, "the SVG carries a DOCTYPE or other directive, which is refused")
		case xml.ProcInst:
			if strings.EqualFold(t.Target, "xml-stylesheet") {
				return refuse(http.StatusUnsupportedMediaType, "the SVG references an external stylesheet, which is refused")
			}
		case xml.StartElement:
			local := strings.ToLower(t.Name.Local)
			if root == "" {
				if local != "svg" {
					return refuse(http.StatusUnsupportedMediaType, "the file's root element is <%s>, not <svg>", t.Name.Local)
				}
				root = local
			}
			if forbiddenSVGElements[local] {
				return refuse(http.StatusUnsupportedMediaType, "the SVG contains a <%s> element, which is refused", t.Name.Local)
			}
			for _, attr := range t.Attr {
				name := strings.ToLower(attr.Name.Local)
				if strings.HasPrefix(name, "on") {
					return refuse(http.StatusUnsupportedMediaType, "the SVG carries an event handler attribute (%s), which is refused", attr.Name.Local)
				}
				if name == "href" || name == "xlink:href" || attr.Name.Space == "http://www.w3.org/1999/xlink" && name == "href" {
					if !safeSVGReference(attr.Value) {
						return refuse(http.StatusUnsupportedMediaType, "the SVG references %q; only #fragment and data:image references are allowed", attr.Value)
					}
				}
				if name == "style" && unsafeCSS(attr.Value) {
					return refuse(http.StatusUnsupportedMediaType, "the SVG's inline style loads an external resource, which is refused")
				}
			}
		case xml.CharData:
			// A <style> body: its text arrives as CharData under the element.
			// Cheaper to scan every text run for the two dangerous tokens
			// than to track the element stack; text outside <style> that
			// happens to contain "url(" is rare and refusing it is harmless.
			if unsafeCSS(string(t)) {
				return refuse(http.StatusUnsupportedMediaType, "the SVG's stylesheet loads an external resource (url() or @import), which is refused")
			}
		}
	}
	if root == "" {
		return refuse(http.StatusUnsupportedMediaType, "the file is not an SVG document")
	}
	return nil
}

func safeSVGReference(value string) bool {
	v := strings.TrimSpace(value)
	if v == "" || strings.HasPrefix(v, "#") {
		return true
	}
	lower := strings.ToLower(v)
	return strings.HasPrefix(lower, "data:image/") && !strings.Contains(lower, "svg")
}

func unsafeCSS(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "url(") || strings.Contains(lower, "@import") || strings.Contains(lower, "expression(")
}
