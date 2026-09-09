package admin

// The admin surface of the BRAND PACK's database layer (ADR-0024, decision 5).
//
// Two routes, `GET` and `PUT /admin/branding/administration`, read and write
// the `branding` section of `centry.platform_config` — the same rows the
// generic Configuration page reaches through `plugin_config_values`, with the
// same schema (config_schemas.go's brandingSection) and the same validation.
// What the typed routes add is what the generic form cannot show: which LAYERS
// contribute to the pack a user actually sees (the mounted file, the database
// rows), the EFFECTIVE pack after the merge, and its ETag — the facts the
// admin Branding page needs to render a preview and a "file versus database"
// diff. `POST /admin/branding/assets/{kind}` stores the logo, mark, favicon,
// login artwork, e-mail logo and font files the section's asset fields
// reference (v2branding.AssetStore), and every save collects the objects the
// section no longer names.
//
// # Validation is one function, reached from both paths
//
// The generic writer type-checks against the schema; validateBrandingValues
// adds the field rules the schema cannot express — a hue is six hex digits, a
// link is an absolute http(s) URL, an asset is a same-origin path. It runs on
// BOTH the typed PUT and the generic save (config_values.go), so an operator
// cannot store through one door what the other refuses. The rules exist for
// the stored-XSS reason config_values.go's own link check exists: an asset
// path is rendered into `<img src>` and `<link rel="icon">` on every page.
//
// # A save is visible on the next request
//
// The resolver caches the merged pack for a short TTL. The write handler
// invalidates it after the transaction commits, so on this replica the next
// bootstrap.js carries the new brand; another replica sees it within the TTL.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	v2branding "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/branding"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/brandpackage"
	appmailer "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/mailer"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

// BrandingResolver is the seam to the pack resolver: the current merged
// snapshot, and the invalidation a save triggers.
type BrandingResolver interface {
	Current(ctx context.Context) v2branding.Snapshot
	Invalidate()
}

// WithBranding supplies the resolver. Without it the typed routes still read
// and write the rows, and report no effective pack — the composition root has
// not wired the bootstrap route, so there is no pack to describe.
func WithBranding(resolver BrandingResolver) Option {
	return func(h *Handler) {
		if resolver == nil {
			return
		}
		h.branding = resolver
	}
}

// WithBrandingAssets supplies the asset store the upload route writes and
// the save path collects from. Without it uploads answer 503 and a save
// collects nothing.
func WithBrandingAssets(store *v2branding.AssetStore) Option {
	return func(h *Handler) {
		if store == nil || !store.Available() {
			return
		}
		h.brandingAssets = store
	}
}

// maxAssetUploadBytes bounds the multipart body before any kind's own cap
// applies: the largest cap plus the form overhead.
const maxAssetUploadBytes = 600 * 1024

// BrandingAssetUpload serves `POST /admin/branding/assets/{kind}` with a
// multipart `file` part. It stores the bytes and answers the asset's public
// path; the caller then places that path in the section (an upload on its
// own changes nothing a user sees, so an abandoned upload is a stray object
// the next save collects, never a stray brand).
func (h *Handler) BrandingAssetUpload(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	if !v2branding.KnownKind(kind) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown asset kind"})
		return
	}
	if h.brandingAssets == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "brand asset storage is not configured on this deployment",
		})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAssetUploadBytes)
	if err := r.ParseMultipartForm(maxAssetUploadBytes); err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"error": fmt.Sprintf("the upload must be a multipart form under %d KiB", maxAssetUploadBytes/1024),
		})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a multipart part named \"file\" is required"})
		return
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, v2branding.MaxAssetBytes(kind)+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "could not read the uploaded file"})
		return
	}

	asset, err := h.brandingAssets.Put(r.Context(), kind, header.Filename, data)
	if err != nil {
		var refusal *v2branding.AssetError
		if errors.As(err, &refusal) {
			writeJSON(w, refusal.Status, map[string]any{"error": refusal.Reason})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not store the asset"})
		return
	}
	writeJSON(w, http.StatusOK, asset)
}

// collectBrandingAssets deletes every stored asset the SAVED section no
// longer references. It runs after a successful write and never fails the
// request: a stray object is a cost, not a defect, and the next save tries
// again. A backend that cannot list (storage.ErrNotSupported) is skipped.
func (h *Handler) collectBrandingAssets(ctx context.Context, stored map[string]any) {
	if h.brandingAssets == nil {
		return
	}
	referenced := referencedAssetPaths(stored)
	paths, err := h.brandingAssets.List(ctx)
	if err != nil {
		slog.Debug("branding: asset collection skipped", "reason", err.Error())
		return
	}
	for _, path := range paths {
		if referenced[path] {
			continue
		}
		// Kept packages (rollback, decision 9) are referenced by nothing in
		// the section and are pruned by their own rule, never here.
		if kind, _, _, ok := v2branding.ParseAssetPath(path); ok && kind == v2branding.KindPackage {
			continue
		}
		if err := h.brandingAssets.Delete(ctx, path); err != nil {
			slog.Warn("branding: could not collect an unreferenced asset", "path", path, "reason", err.Error())
		}
	}
}

// referencedAssetPaths lists every asset path the stored rows name: the four
// image fields and every font face's url.
func referencedAssetPaths(stored map[string]any) map[string]bool {
	referenced := map[string]bool{}
	for _, key := range []string{
		platformconfig.KeyBrandingLogoFull, platformconfig.KeyBrandingLogoMark,
		platformconfig.KeyBrandingFavicon, platformconfig.KeyBrandingLoginArt,
		platformconfig.KeyBrandingLogoEmail,
	} {
		if text, ok := stored[key].(string); ok && text != "" {
			referenced[strings.TrimSpace(text)] = true
		}
	}
	if faces, ok := stored[platformconfig.KeyBrandingFontFaces].([]any); ok {
		for _, entry := range faces {
			if object, ok := entry.(map[string]any); ok {
				if text, ok := object["url"].(string); ok && text != "" {
					referenced[strings.TrimSpace(text)] = true
				}
			}
		}
	}
	return referenced
}

// Mailer is the seam to outbound e-mail (internal/application/mailer).
//
// Configured takes a context because the mail configuration is resolved from
// the database over the environment on every call (gap G7), so a relay an
// administrator has just saved is configured on the next request rather than
// after the next restart.
type Mailer interface {
	Configured(ctx context.Context) bool
	SendInvitation(ctx context.Context, invitation appmailer.Invitation) error
	SendTest(ctx context.Context, to string) error
}

// WithMailer supplies the composer the invite and test routes send through.
func WithMailer(m Mailer) Option {
	return func(h *Handler) {
		if m == nil {
			return
		}
		h.mailer = m
	}
}

// brandingTestEmailBody is the test route's request.
type brandingTestEmailBody struct {
	To string `json:"to"`
}

// BrandingTestEmail serves `POST /admin/branding/test_email/administration`:
// one branded test message to the address in the body, so an administrator
// can see the e-mail brand without inviting anyone. 503 when no relay is
// configured (or on a shadow deployment), 400 for a bad address, 502 when
// the relay refused — the reason is in the body, since the operator is the
// one who can act on it.
//
// The body is `email.go`'s `sendTestEmail`, shared with the E-mail page's own
// test route. The two routes exist for their two PERMISSIONS, not for two
// behaviours, and the message must be identical either way: this button
// verifies the RESOLVED configuration (gap G7), so pressing it after a save
// tells the operator whether the settings they just typed work.
func (h *Handler) BrandingTestEmail(w http.ResponseWriter, r *http.Request) {
	h.sendTestEmail(w, r)
}

// brandingResponse is the wire shape of both routes.
type brandingResponse struct {
	// Values is the section's declared keys with the stored values overlaid on
	// the schema defaults — the generic form's shape, so the two editors agree.
	Values map[string]any `json:"values"`
	// Layers says which operator-controlled layers contribute to the served
	// pack: the mounted file, the database rows.
	Layers v2branding.Layers `json:"layers"`
	// Effective is the merged pack the bootstrap route serves right now, or
	// null when nothing is served (no file, no rows) — the UI then renders its
	// compiled default.
	Effective *v2branding.Pack `json:"effective"`
	// ETag is the unquoted content hash of the served bootstrap body; a page
	// that has just saved can wait for it to change.
	ETag  string `json:"etag,omitempty"`
	Saved bool   `json:"saved,omitempty"`
}

// BrandingRead serves `GET /admin/branding/administration`.
func (h *Handler) BrandingRead(w http.ResponseWriter, r *http.Request) {
	section, ok := findConfigSection(platformconfig.SectionBranding)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "branding section missing"})
		return
	}
	h.writeBrandingState(w, r, section, false)
}

// BrandingSave serves `PUT /admin/branding/administration`. The body is the
// generic save's `{"values": {...}}` so a form can move between the two
// editors without a translation layer.
func (h *Handler) BrandingSave(w http.ResponseWriter, r *http.Request) {
	section, ok := findConfigSection(platformconfig.SectionBranding)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "branding section missing"})
		return
	}

	var body configSaveBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	if body.Values == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "values is required"})
		return
	}
	if reason := validateSectionValues(section, body.Values); reason != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": reason})
		return
	}
	if reason := validateBrandingValues(body.Values); reason != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": reason})
		return
	}

	principal, _ := auth.UserFromContext(r.Context())
	author := principal.Email
	if author == "" {
		author = principal.ID
	}
	if err := h.storeSectionValues(r, section.id, body.Values, author); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "could not write the platform configuration store",
		})
		return
	}
	h.invalidateBranding()
	if stored, err := h.loadSectionValues(r, section.id); err == nil {
		h.collectBrandingAssets(r.Context(), stored)
	}
	h.writeBrandingState(w, r, section, true)
}

// invalidateBranding drops the resolver's cache after a write to the branding
// section, from either editor.
func (h *Handler) invalidateBranding() {
	if h.branding != nil {
		h.branding.Invalidate()
	}
}

// writeBrandingState re-READS the rows (never echoes the request — a write
// that silently failed must look different from one that landed) and
// describes the resolved pack beside them.
func (h *Handler) writeBrandingState(w http.ResponseWriter, r *http.Request, section configSection, saved bool) {
	stored, err := h.loadSectionValues(r, section.id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "could not read the platform configuration store",
		})
		return
	}
	response := brandingResponse{
		Values: mergeSectionValues(section, stored),
		Saved:  saved,
	}
	if h.branding != nil {
		snap := h.branding.Current(r.Context())
		response.Layers = snap.Layers
		response.Effective = snap.Pack
		response.ETag = snap.ETagValue
	}
	writeJSON(w, http.StatusOK, response)
}

// ---------------------------------------------------------------------------
// field rules
// ---------------------------------------------------------------------------

var hexColourPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Bounds. The typography bounds are the brand-pack schema's own
// (branding/pack.go); the text bounds are generous against any real product
// name and tight enough that a paste cannot turn the document title or the
// login heading into a paragraph.
const (
	maxBrandingNameRunes    = 80
	maxBrandingTaglineRunes = 200
	maxBrandingFontRunes    = 200
	maxBrandingPathRunes    = 512
	minBrandingBaseSize     = 12
	maxBrandingBaseSize     = 18
	minBrandingScale        = 1.05
	maxBrandingScale        = 1.5
	maxBrandingRadius       = 9999
)

// validateBrandingValues returns "" when the values may be stored, or the
// sentence the operator is shown. An empty string or a zero number is always
// accepted: it means "inherit from the layer below" (platformconfig/branding.go).
// Keys are visited in sorted order so the first refusal is deterministic.
func validateBrandingValues(values map[string]any) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		var reason string
		switch key {
		case platformconfig.KeyBrandingHue, platformconfig.KeyBrandingOnBrand:
			reason = validateHexColour(key, values[key])
		case platformconfig.KeyBrandingDocsURL:
			reason = validateDocsURL(key, values[key])
		case platformconfig.KeyBrandingSupportURL:
			reason = validateAbsoluteHTTPURL(key, values[key])
		case platformconfig.KeyBrandingLogoFull, platformconfig.KeyBrandingLogoMark,
			platformconfig.KeyBrandingFavicon, platformconfig.KeyBrandingLoginArt,
			platformconfig.KeyBrandingLogoEmail:
			reason = validateSameOriginPath(key, values[key])
		case platformconfig.KeyBrandingSupportEmail:
			reason = validateEmailAddress(key, values[key])
		case platformconfig.KeyBrandingSenderName:
			reason = validateRuneBound(key, values[key], maxBrandingNameRunes)
		case platformconfig.KeyBrandingProductName, platformconfig.KeyBrandingProductShortName:
			reason = validateRuneBound(key, values[key], maxBrandingNameRunes)
		case platformconfig.KeyBrandingProductTagline:
			reason = validateRuneBound(key, values[key], maxBrandingTaglineRunes)
		case platformconfig.KeyBrandingFontFamily, platformconfig.KeyBrandingFontFamilyMono:
			reason = validateRuneBound(key, values[key], maxBrandingFontRunes)
		case platformconfig.KeyBrandingBaseSize:
			reason = validateNumberInRange(key, values[key], minBrandingBaseSize, maxBrandingBaseSize)
		case platformconfig.KeyBrandingScale:
			reason = validateNumberInRange(key, values[key], minBrandingScale, maxBrandingScale)
		case platformconfig.KeyBrandingRadiusSm, platformconfig.KeyBrandingRadiusMd,
			platformconfig.KeyBrandingRadiusLg, platformconfig.KeyBrandingRadiusPill:
			reason = validateNumberInRange(key, values[key], 0, maxBrandingRadius)
		case platformconfig.KeyBrandingDensity:
			reason = validateDensity(key, values[key])
		case platformconfig.KeyBrandingFontFaces:
			reason = validateFontFaces(key, values[key])
		case platformconfig.KeyBrandingSchemeTokens:
			reason = validateSchemeTokens(key, values[key])
		}
		if reason != "" {
			return reason
		}
	}
	return ""
}

func stringValue(value any) (string, bool) {
	text, ok := value.(string)
	return strings.TrimSpace(text), ok
}

func validateHexColour(key string, value any) string {
	text, ok := stringValue(value)
	if !ok || text == "" {
		return ""
	}
	if !hexColourPattern.MatchString(text) {
		return fmt.Sprintf("%q must be a six-digit hex colour such as #1A73E8", key)
	}
	return ""
}

// validateEmailAddress admits one plain addr-spec (no display name), the
// same rule the invite handlers apply.
func validateEmailAddress(key string, value any) string {
	text, ok := stringValue(value)
	if !ok || text == "" {
		return ""
	}
	parsed, err := mail.ParseAddress(text)
	if err != nil || parsed.Address != text || !strings.Contains(text[strings.LastIndex(text, "@")+1:], ".") {
		return fmt.Sprintf("%q must be a plain e-mail address such as support@example.com", key)
	}
	return ""
}

func validateAbsoluteHTTPURL(key string, value any) string {
	text, ok := stringValue(value)
	if !ok || text == "" {
		return ""
	}
	parsed, err := url.Parse(text)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Sprintf("%q must be an absolute http or https URL", key)
	}
	return ""
}

// validateDocsURL admits either an absolute http(s) URL (a tenant's
// externally hosted documentation) or a root-relative same-origin path (the
// platform's own embedded docs SPA, issue W1b) — unlike supportUrl, which
// must always be absolute. `ProductDefault()` states docs_url as exactly the
// latter shape, and a branding package export round-trips that value back
// through this same validator on import, so both shapes must be accepted
// here, not only in the brand-pack schema (schema.ts / pack.go's
// optionalRelativeOrAbsoluteURL, which this mirrors).
func validateDocsURL(key string, value any) string {
	if validateSameOriginPath(key, value) == "" {
		return ""
	}
	return validateAbsoluteHTTPURL(key, value)
}

// validateSameOriginPath admits only a root-relative path on this origin:
// one leading slash, no scheme, no authority, no whitespace or control
// characters. That excludes `javascript:`, `data:` and protocol-relative
// `//host` forms by construction rather than by denylist. The uploaded-asset
// route of a later unit hands out exactly this shape.
func validateSameOriginPath(key string, value any) string {
	text, ok := stringValue(value)
	if !ok || text == "" {
		return ""
	}
	if utf8.RuneCountInString(text) > maxBrandingPathRunes {
		return fmt.Sprintf("%q is too long", key)
	}
	if !strings.HasPrefix(text, "/") || strings.HasPrefix(text, "//") || strings.HasPrefix(text, "/\\") {
		return fmt.Sprintf("%q must be a path on this origin, starting with a single /", key)
	}
	for _, r := range text {
		if r <= ' ' || r == 0x7f {
			return fmt.Sprintf("%q must not contain whitespace or control characters", key)
		}
	}
	if parsed, err := url.Parse(text); err != nil || parsed.Scheme != "" || parsed.Host != "" {
		return fmt.Sprintf("%q must be a path on this origin, starting with a single /", key)
	}
	return ""
}

func validateRuneBound(key string, value any, max int) string {
	text, ok := stringValue(value)
	if !ok {
		return ""
	}
	if utf8.RuneCountInString(text) > max {
		return fmt.Sprintf("%q must be at most %d characters", key, max)
	}
	return ""
}

func validateNumberInRange(key string, value any, min, max float64) string {
	number, ok := value.(float64)
	if !ok || number == 0 {
		return ""
	}
	if number < min || number > max {
		return fmt.Sprintf("%q must be between %v and %v, or 0 to inherit", key, min, max)
	}
	return ""
}

// maxBrandingFontFaces bounds the self-hosted faces: every one is a fetch on
// every cold load, and two (a text face and a monospace face, or a regular
// and a bold) cover every corporate identity this feature is for.
const maxBrandingFontFaces = 2

// validateFontFaces checks the font_faces array: at most two objects, each
// with a non-empty family, a url that is an uploaded font asset's path, an
// optional weight and an optional style of normal or italic.
func validateFontFaces(key string, value any) string {
	entries, ok := value.([]any)
	if !ok {
		return fmt.Sprintf("%q must be an array of font faces", key)
	}
	if len(entries) > maxBrandingFontFaces {
		return fmt.Sprintf("%q may hold at most %d faces", key, maxBrandingFontFaces)
	}
	for i, entry := range entries {
		object, ok := entry.(map[string]any)
		if !ok {
			return fmt.Sprintf("%q[%d] must be an object with family and url", key, i)
		}
		family, _ := stringValue(object["family"])
		if family == "" || utf8.RuneCountInString(family) > maxBrandingFontRunes {
			return fmt.Sprintf("%q[%d].family must be a non-empty string of at most %d characters", key, i, maxBrandingFontRunes)
		}
		address, _ := stringValue(object["url"])
		if kind, _, _, ok := v2branding.ParseAssetPath(address); !ok || kind != v2branding.KindFont {
			return fmt.Sprintf("%q[%d].url must be the path of an uploaded font asset", key, i)
		}
		if weight, present := object["weight"]; present {
			if text, ok := stringValue(weight); !ok || utf8.RuneCountInString(text) > 16 {
				return fmt.Sprintf("%q[%d].weight must be a short string such as \"400\" or \"100 900\"", key, i)
			}
		}
		if style, present := object["style"]; present {
			if text, ok := stringValue(style); !ok || (text != "" && text != "normal" && text != "italic") {
				return fmt.Sprintf("%q[%d].style must be \"normal\" or \"italic\"", key, i)
			}
		}
		for name := range object {
			switch name {
			case "family", "url", "weight", "style":
			default:
				return fmt.Sprintf("%q[%d] has an unknown key %q", key, i, name)
			}
		}
	}
	return ""
}

// maxSchemeTokens bounds one scheme's record: the product default states 406
// ids, and a package that hand-tunes every one of them is still under this.
const maxSchemeTokens = 512

// validateSchemeTokens checks {light|dark|hc: {id: "#rrggbb"}}. Ids are the
// web app's token vocabulary and are not enumerated here — an unknown id is
// ignored by the UI's unflatten step — but every value must be a six-digit
// hex colour, so a stored record can never smuggle a CSS expression into a
// custom property.
func validateSchemeTokens(key string, value any) string {
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Sprintf("%q must be an object of schemes", key)
	}
	for scheme, entries := range object {
		if scheme != "light" && scheme != "dark" && scheme != "hc" {
			return fmt.Sprintf("%q has an unknown scheme %q (light, dark or hc)", key, scheme)
		}
		record, ok := entries.(map[string]any)
		if !ok {
			return fmt.Sprintf("%q[%q] must be an object of token ids", key, scheme)
		}
		if len(record) > maxSchemeTokens {
			return fmt.Sprintf("%q[%q] has too many tokens (limit %d)", key, scheme, maxSchemeTokens)
		}
		for id, colour := range record {
			if utf8.RuneCountInString(id) > 64 || strings.TrimSpace(id) == "" {
				return fmt.Sprintf("%q[%q] has an invalid token id %q", key, scheme, id)
			}
			text, _ := stringValue(colour)
			if text == "" || !hexColourPattern.MatchString(text) {
				return fmt.Sprintf("%q[%q][%q] must be a six-digit hex colour", key, scheme, id)
			}
		}
	}
	return ""
}

func validateDensity(key string, value any) string {
	text, ok := stringValue(value)
	if !ok || text == "" || text == "comfortable" || text == "compact" {
		return ""
	}
	return fmt.Sprintf("%q must be \"comfortable\", \"compact\" or empty to inherit", key)
}

// --- the branding package (ADR-0024 decision 9) --------------------------------

// BrandingPackages is the seam to internal/application/brandpackage.
type BrandingPackages interface {
	Export(ctx context.Context, pack *v2branding.Pack) (data []byte, name string, err error)
	Parse(data []byte) (*brandpackage.Imported, []brandpackage.Problem)
	Apply(ctx context.Context, imported *brandpackage.Imported) (map[string]any, error)
	Store(ctx context.Context, data []byte) (brandpackage.Version, error)
	Versions(ctx context.Context) ([]brandpackage.Version, error)
	Load(ctx context.Context, digest string) ([]byte, error)
}

// WithBrandingPackages supplies the package service.
func WithBrandingPackages(p BrandingPackages) Option {
	return func(h *Handler) {
		if p != nil {
			h.packages = p
		}
	}
}

// BrandingPackageExport serves `GET /admin/branding/package/administration`:
// the current brand as a zip.
func (h *Handler) BrandingPackageExport(w http.ResponseWriter, r *http.Request) {
	if h.packages == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "branding packages are not available on this deployment"})
		return
	}
	var pack *v2branding.Pack
	if h.branding != nil {
		pack = h.branding.Current(r.Context()).Pack
	}
	data, name, err := h.packages.Export(r.Context(), pack)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not build the branding package: " + err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// brandingPackageReport is the import routes' answer.
type brandingPackageReport struct {
	OK       bool                   `json:"ok"`
	DryRun   bool                   `json:"dry_run"`
	Applied  bool                   `json:"applied"`
	Problems []brandpackage.Problem `json:"problems"`
	Warnings []string               `json:"warnings"`
	Diff     []brandpackage.Change  `json:"diff"`
	Manifest *brandpackage.Manifest `json:"manifest,omitempty"`
	Version  *brandpackage.Version  `json:"version,omitempty"`
	Error    string                 `json:"error,omitempty"`
}

// BrandingPackageImport serves `POST /admin/branding/package/administration`
// with a multipart `file` part and an optional `dry_run=true` query. The dry
// run and the apply share every check; only the store calls differ.
func (h *Handler) BrandingPackageImport(w http.ResponseWriter, r *http.Request) {
	if h.packages == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "branding packages are not available on this deployment"})
		return
	}
	dryRun := r.URL.Query().Get("dry_run") == "true" || r.URL.Query().Get("dry_run") == "1"
	r.Body = http.MaxBytesReader(w, r.Body, v2branding.MaxPackageBytes+64*1024)
	if err := r.ParseMultipartForm(v2branding.MaxPackageBytes + 64*1024); err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"error": fmt.Sprintf("the upload must be a multipart form under %d MiB", v2branding.MaxPackageBytes/1024/1024),
		})
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a multipart part named \"file\" is required"})
		return
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, v2branding.MaxPackageBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "could not read the uploaded file"})
		return
	}
	h.importPackage(w, r, data, dryRun, true)
}

// BrandingPackageVersions serves `GET /admin/branding/package/administration/versions`.
func (h *Handler) BrandingPackageVersions(w http.ResponseWriter, r *http.Request) {
	if h.packages == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "branding packages are not available on this deployment"})
		return
	}
	versions, err := h.packages.Versions(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	if versions == nil {
		versions = []brandpackage.Version{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
}

// BrandingPackageRestore serves
// `POST /admin/branding/package/administration/versions/{digest}/restore`:
// re-imports a stored package. It is not stored again, so restoring never
// pushes an older package out of the kept set.
func (h *Handler) BrandingPackageRestore(w http.ResponseWriter, r *http.Request) {
	if h.packages == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "branding packages are not available on this deployment"})
		return
	}
	digest := chi.URLParam(r, "digest")
	if !digestPattern.MatchString(digest) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown package version"})
		return
	}
	data, err := h.packages.Load(r.Context(), digest)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown package version"})
		return
	}
	h.importPackage(w, r, data, false, false)
}

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// importPackage is the shared body of import and restore.
func (h *Handler) importPackage(w http.ResponseWriter, r *http.Request, data []byte, dryRun, keep bool) {
	section, ok := findConfigSection(platformconfig.SectionBranding)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "branding section missing"})
		return
	}
	report := brandingPackageReport{DryRun: dryRun, Problems: []brandpackage.Problem{}, Warnings: []string{}, Diff: []brandpackage.Change{}}

	imported, problems := h.packages.Parse(data)
	if imported != nil {
		report.Manifest = imported.Manifest
		if imported.Warnings != nil {
			report.Warnings = imported.Warnings
		}
	}
	if len(problems) > 0 {
		report.Problems = problems
		writeJSON(w, http.StatusBadRequest, report)
		return
	}

	// The values the section WOULD hold, computed without storing anything:
	// assets keep their package references for the diff, which is what the
	// operator can recognise.
	incoming := brandpackage.ValuesFromPack(imported.Pack, v2branding.ProductDefault())
	if reason := validateSectionValues(section, incoming); reason != "" {
		report.Problems = append(report.Problems, brandpackage.Problem{Entry: "brand-pack.json", Reason: reason})
	}
	// Package-relative asset references are not same-origin paths yet; they
	// are validated per kind by Parse and become stored paths on apply, so
	// they are blanked for the section rules and restored for the diff.
	report.Problems = append(report.Problems, packageOnlyProblems(validateBrandingValues(withoutPackageRefs(imported, incoming)))...)
	if len(report.Problems) > 0 {
		writeJSON(w, http.StatusBadRequest, report)
		return
	}

	current := map[string]any{}
	if h.pool != nil {
		stored, err := h.loadSectionValues(r, section.id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not read the platform configuration store"})
			return
		}
		current = mergeSectionValues(section, stored)
	}
	report.Diff = brandpackage.Diff(current, incoming)
	if report.Diff == nil {
		report.Diff = []brandpackage.Change{}
	}
	report.OK = true
	if dryRun {
		writeJSON(w, http.StatusOK, report)
		return
	}
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "database unavailable"})
		return
	}

	values, err := h.packages.Apply(r.Context(), imported)
	if err != nil {
		report.Error = err.Error()
		writeJSON(w, http.StatusBadGateway, report)
		return
	}
	if reason := validateBrandingValues(values); reason != "" {
		report.Problems = append(report.Problems, brandpackage.Problem{Entry: "brand-pack.json", Reason: reason})
		writeJSON(w, http.StatusBadRequest, report)
		return
	}
	principal, _ := auth.UserFromContext(r.Context())
	author := principal.Email
	if author == "" {
		author = principal.ID
	}
	if err := h.storeSectionValues(r, section.id, values, author); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not write the platform configuration store"})
		return
	}
	h.invalidateBranding()
	if keep {
		if version, err := h.packages.Store(r.Context(), data); err == nil {
			report.Version = &version
		} else {
			report.Warnings = append(report.Warnings, "the package was applied but could not be kept for rollback: "+err.Error())
		}
	}
	if stored, err := h.loadSectionValues(r, section.id); err == nil {
		h.collectBrandingAssets(r.Context(), stored)
	}
	report.Applied = true
	writeJSON(w, http.StatusOK, report)
}

// withoutPackageRefs blanks the asset fields that point into the package so
// the same-origin rule does not refuse them before Apply rewrites them.
func withoutPackageRefs(imported *brandpackage.Imported, values map[string]any) map[string]any {
	copied := make(map[string]any, len(values))
	for k, v := range values {
		copied[k] = v
	}
	for _, key := range []string{
		platformconfig.KeyBrandingLogoFull, platformconfig.KeyBrandingLogoMark, platformconfig.KeyBrandingFavicon,
		platformconfig.KeyBrandingLoginArt, platformconfig.KeyBrandingLogoEmail,
	} {
		if text, _ := copied[key].(string); strings.HasPrefix(text, "assets/") {
			copied[key] = ""
		}
	}
	if faces, ok := copied[platformconfig.KeyBrandingFontFaces].([]any); ok {
		kept := make([]any, 0, len(faces))
		for _, face := range faces {
			if object, ok := face.(map[string]any); ok {
				if url, _ := object["url"].(string); strings.HasPrefix(url, "assets/") {
					continue
				}
			}
			kept = append(kept, face)
		}
		copied[platformconfig.KeyBrandingFontFaces] = kept
	}
	_ = imported
	return copied
}

func packageOnlyProblems(reason string) []brandpackage.Problem {
	if reason == "" {
		return nil
	}
	return []brandpackage.Problem{{Entry: "brand-pack.json", Reason: reason}}
}
