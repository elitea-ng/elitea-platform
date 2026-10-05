package branding

// Three-layer pack resolution (ADR-0024, decisions 1 and 4).
//
// The served pack is `product default ← BRAND_PACK_PATH file ← platform_config
// section "branding"`, merged field by field:
//
//   - The PRODUCT DEFAULT is the pack compiled into the web app
//     (`apps/elitea-web/src/shared/brand/tokens/default.pack.json`), embedded
//     here as product_default.pack.json and pinned byte-for-byte by a test. It
//     is NOT DefaultPack(): that one is the schema reference with a placeholder
//     hue, and serving it repainted the whole app (see noPackBody). The product
//     default is what the UI renders when channel C is absent, so using it as
//     the base under a database overlay changes nothing the operator did not
//     ask for.
//   - The FILE layer is the pack an operator mounts; it is read once at
//     construction, as before — a mounted file changes with a redeploy.
//   - The DATABASE layer is what the admin Branding section writes. It is read
//     on demand and cached for a short TTL, so a save takes effect on the next
//     page load without a restart.
//
// # When nothing is served
//
// With no file and an empty database layer the resolver publishes NO pack and
// the handler serves the inert body: an unset `window.elitea_brand` is the
// documented "channel C absent" path and the UI renders its compiled default.
// The product default is a base for an overlay, never a pack served on its own.
//
// # Failure is permissive, and the last good answer wins
//
// A database error keeps the previous snapshot for another TTL when there is
// one, and otherwise serves the file-or-inert layers. A database overlay that
// produces a pack the schema rejects (an operator typed a hue the UI would not
// parse) is logged and the layers below it are served — the UI's channelC.ts
// would otherwise reject the whole pack and fall back to its default, which is
// the same outcome minus the log line naming the field.

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/httpcache"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

//go:embed product_default.pack.json
var productDefaultJSON []byte

// DefaultCacheTTL bounds how stale a served pack can be after an admin save
// on ANOTHER replica (a save on this replica invalidates immediately). Fifteen
// seconds is the maintenance gate's order of magnitude and well under the
// time it takes an operator to open a second window to check.
const DefaultCacheTTL = 15 * time.Second

// loadTimeout bounds the database read so a slow store degrades to the cached
// or lower layers rather than holding the bootstrap script — and with it every
// page load — open.
const loadTimeout = 3 * time.Second

// Layers reports which of the two operator-controlled layers contributed.
type Layers struct {
	// File is true when BRAND_PACK_PATH named a pack that parsed.
	File bool `json:"file"`
	// Database is true when the branding section holds at least one value.
	Database bool `json:"database"`
}

// Snapshot is one resolved answer: the pack (nil when nothing is served), the
// rendered bootstrap body and its strong ETag.
type Snapshot struct {
	Pack      *Pack
	Body      []byte
	ETag      string // strong quoted form: "<hex>"
	ETagValue string // unquoted hex — the ?v= token
	Layers    Layers
}

// ResolverConfig wires a Resolver.
type ResolverConfig struct {
	// PackPath is the file layer (BRAND_PACK_PATH). Empty means no file layer.
	PackPath string
	// Pool reads the database layer. Nil means no database layer — the
	// resolver then behaves exactly as the file-only handler did.
	Pool *pgxpool.Pool
	// TTL overrides DefaultCacheTTL; zero keeps the default.
	TTL time.Duration

	// loadOverlay and now are injectable so the layering and the cache are
	// testable without a database or a clock. Nil selects the real ones.
	loadOverlay func(context.Context) (platformconfig.BrandingOverlay, error)
	now         func() time.Time
}

// Resolver produces the current Snapshot.
type Resolver struct {
	filePack    *Pack
	loadOverlay func(context.Context) (platformconfig.BrandingOverlay, error)
	ttl         time.Duration
	now         func() time.Time

	mu        sync.Mutex
	current   *Snapshot
	expiresAt time.Time
}

// NewResolver loads the file layer once and prepares the database layer. It
// never fails: every degradation is logged and lands on a lower layer.
func NewResolver(cfg ResolverConfig) *Resolver {
	r := &Resolver{ttl: cfg.TTL, now: cfg.now, loadOverlay: cfg.loadOverlay}
	if r.ttl <= 0 {
		r.ttl = DefaultCacheTTL
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.loadOverlay == nil && cfg.Pool != nil {
		pool := cfg.Pool
		r.loadOverlay = func(ctx context.Context) (platformconfig.BrandingOverlay, error) {
			return platformconfig.LoadBranding(ctx, pool)
		}
	}

	if cfg.PackPath == "" {
		slog.Info("branding: BRAND_PACK_PATH not set; no file layer")
	} else if loaded, err := LoadPack(cfg.PackPath); err != nil {
		slog.Warn("branding: ignoring the file layer",
			"path", cfg.PackPath, "reason", err.Error())
	} else {
		r.filePack = loaded
		slog.Info("branding: file layer loaded",
			"path", cfg.PackPath, "pack_id", loaded.ID, "pack_version", loaded.Version)
	}
	if r.loadOverlay == nil {
		slog.Info("branding: no database layer; the pack can only change with a restart")
	}
	return r
}

// Current returns the snapshot, refreshing the database layer when the cache
// has expired.
func (r *Resolver) Current(ctx context.Context) Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current != nil && r.now().Before(r.expiresAt) {
		return *r.current
	}
	next := r.resolve(ctx)
	r.current = &next
	r.expiresAt = r.now().Add(r.ttl)
	return next
}

// Invalidate drops the cache so the next Current re-reads the database layer.
// The admin save path calls it, which is what makes a save visible on THIS
// replica immediately rather than within a TTL.
func (r *Resolver) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expiresAt = time.Time{}
}

// resolve computes a fresh snapshot. Called under the lock.
func (r *Resolver) resolve(ctx context.Context) Snapshot {
	overlay, overlayErr := r.readOverlay(ctx)
	if overlayErr != nil {
		if r.current != nil {
			slog.Warn("branding: database layer unreadable; keeping the last resolved pack",
				"reason", overlayErr.Error())
			return *r.current
		}
		slog.Warn("branding: database layer unreadable; serving the layers below it",
			"reason", overlayErr.Error())
	}

	layers := Layers{File: r.filePack != nil, Database: !overlay.IsZero()}
	base := r.filePack
	if base == nil && layers.Database {
		base = productDefaultPack()
	}

	var pack *Pack
	if base != nil {
		pack = base
		if layers.Database {
			merged, err := applyOverlay(base, overlay)
			if err != nil {
				slog.Warn("branding: database layer produces an invalid pack; serving the layers below it",
					"reason", err.Error())
				layers.Database = false
				if r.filePack == nil {
					pack = nil
				}
			} else {
				pack = merged
			}
		}
	}

	return newSnapshot(pack, layers)
}

func (r *Resolver) readOverlay(ctx context.Context) (platformconfig.BrandingOverlay, error) {
	if r.loadOverlay == nil {
		return platformconfig.BrandingOverlay{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()
	return r.loadOverlay(ctx)
}

// newSnapshot renders the body and its content hash.
func newSnapshot(pack *Pack, layers Layers) Snapshot {
	body := renderBootstrapJS(pack)
	etag, value := httpcache.StrongETag(body)
	return Snapshot{
		Pack:      pack,
		Body:      body,
		ETag:      etag,
		ETagValue: value,
		Layers:    layers,
	}
}

// ProductDefault returns a fresh parse of the embedded product default pack —
// the base a branding package is compared against and exported from on a
// deployment that has customised nothing.
func ProductDefault() *Pack { return productDefaultPack() }

// productDefaultPack parses the embedded product default. The embed is pinned
// to the UI's file and covered by a parse test, so a failure here is a build
// defect; it degrades to DefaultPack() only so that the branding path can
// never panic.
func productDefaultPack() *Pack {
	pack, err := ParsePack(productDefaultJSON)
	if err != nil {
		slog.Error("branding: embedded product default pack does not parse", "reason", err.Error())
		return DefaultPack()
	}
	return pack
}

// applyOverlay lays the database values over a copy of base, field by field,
// and re-validates the result through ParsePack so the served pack is exactly
// one the UI's zod schema accepts. A zero overlay field leaves the base value.
func applyOverlay(base *Pack, o platformconfig.BrandingOverlay) (*Pack, error) {
	merged := *base
	merged.Schemes = Schemes{Light: base.Schemes.Light, Dark: base.Schemes.Dark, HC: base.Schemes.HC}
	// A NEW hue drops the base's stated tokens. The UI resolves a scheme as
	// "stated token wins, else derive from brand.hue" (toMuiPalette.ts's
	// resolveScheme), and the product default states all 406 ids per
	// scheme — so a hue laid over stated tokens would change
	// window.elitea_brand.brand.hue and nothing visible. Emptying the records
	// is the hue-only tenant-pack shape: every id derives from the new hue
	// and the whole surface repaints, which is what the operator asked for.
	if o.Hue != "" && !strings.EqualFold(strings.TrimSpace(o.Hue), strings.TrimSpace(base.Brand.Hue)) {
		merged.Schemes = Schemes{Light: map[string]string{}, Dark: map[string]string{}}
	}

	setString(&merged.Product.Name, o.ProductName)
	setString(&merged.Product.ShortName, o.ProductShortName)
	setOptional(&merged.Product.Tagline, o.ProductTagline)
	setOptional(&merged.Product.DocsURL, o.DocsURL)
	setOptional(&merged.Product.SupportURL, o.SupportURL)
	setOptional(&merged.Product.SenderName, o.SenderName)
	setOptional(&merged.Product.SupportEmail, o.SupportEmail)
	setString(&merged.Brand.Hue, o.Hue)
	setOptional(&merged.Brand.OnBrand, o.OnBrand)
	setString(&merged.Typography.FontFamily, o.FontFamily)
	setString(&merged.Typography.FontFamilyMono, o.FontFamilyMono)
	setNumber(&merged.Typography.BaseSize, o.BaseSize)
	setNumber(&merged.Typography.Scale, o.Scale)
	setNumber(&merged.Shape.RadiusSm, o.RadiusSm)
	setNumber(&merged.Shape.RadiusMd, o.RadiusMd)
	setNumber(&merged.Shape.RadiusLg, o.RadiusLg)
	setNumber(&merged.Shape.RadiusPill, o.RadiusPill)
	setString(&merged.Shape.Density, o.Density)
	setString(&merged.Assets.LogoFull, o.LogoFull)
	setString(&merged.Assets.LogoMark, o.LogoMark)
	setString(&merged.Assets.Favicon, o.Favicon)
	setOptional(&merged.Assets.LoginArt, o.LoginArt)
	setOptional(&merged.Assets.LogoEmail, o.LogoEmail)
	// Hand-tuned tokens (a branding package, decision 9) are STATED tokens:
	// they replace the base's records for the schemes they name, so the ids
	// they carry win over derivation and every other id still derives from
	// the hue. They are applied after the hue rule above on purpose.
	if len(o.SchemeTokens) > 0 {
		merged.Schemes = Schemes{Light: map[string]string{}, Dark: map[string]string{}}
		for scheme, tokens := range o.SchemeTokens {
			copied := make(map[string]string, len(tokens))
			for id, value := range tokens {
				copied[id] = value
			}
			switch scheme {
			case "light":
				merged.Schemes.Light = copied
			case "dark":
				merged.Schemes.Dark = copied
			case "hc":
				merged.Schemes.HC = copied
			}
		}
	}
	if len(o.FontFaces) > 0 {
		faces := make([]FontFace, 0, len(o.FontFaces))
		for _, f := range o.FontFaces {
			face := FontFace{Family: f.Family, URL: f.URL}
			setOptional(&face.Weight, f.Weight)
			setOptional(&face.Style, f.Style)
			faces = append(faces, face)
		}
		merged.Typography.FontFaces = faces
	}

	// Round-trip through the validator: the overlay came from rows, and the
	// only contract that matters is "parses under the UI's schema".
	data, err := json.Marshal(&merged)
	if err != nil {
		return nil, fmt.Errorf("encoding merged pack: %w", err)
	}
	return ParsePack(data)
}

func setString(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

func setOptional(dst **string, v string) {
	if v != "" {
		value := v
		*dst = &value
	}
}

func setNumber(dst *float64, v float64) {
	if v != 0 {
		*dst = v
	}
}
