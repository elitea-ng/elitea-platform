# {{.ProductName}} branding package

Exported {{.ExportedAt}} from {{.Deployment}} (brand-pack format {{.Format}}).

## What is in here

| Entry | Meaning |
| --- | --- |
| `brand-pack.json` | The brand: product names and links, the brand colour, typography, radii, density, asset references. Edit this. Validate it against `schema/brand-pack.schema.json`. |
| `assets/` | The files the pack references: `logo-full`, `logo-mark`, `favicon`, `login-art`, `logo-email`, and `fonts/*.woff2`. Replace a file and keep its name, or change the reference in the pack. |
| `preview/app.html` | An offline previewer. Open it from disk, then load your edited `brand-pack.json` and drop your assets on it to see the app shell in both colour schemes. |
| `preview/login.html`, `preview/email-*.html` | The login page and the e-mails rendered under this brand, as they were at export time. |
| `manifest.json` | Format version, export time, the deployment and a digest of the pack. Leave it alone. |

## Rules the import applies

- `brand.hue` is a six-digit hex colour, e.g. `#1A73E8`. Every accent and surface tint is derived from it unless `schemes` states a token.
- `schemes.light` / `schemes.dark` may name individual token ids with hex colours; an empty record derives everything from the hue. Only state what you tuned by hand.
- If you state a `role.*` id, give the legacy id of its row the same colour: `role.bg` and `background.default`, `role.border` and `border.lines`, `role.text` and `text.secondary`, `role.textMuted` and `text.primary`, `role.primary` and `primary.main`, `role.primaryHover` and `primary.hover`, `role.primaryPressed` and `primary.pressed`, `role.signal` and `text.link`, `role.success` and `success.main`, `role.warning` and `warning.main`, `role.danger` and `text.error`, `role.dangerFill` and `error.main`, `role.onDangerFill` and `error.contrastText`. A later release moves the reads of that legacy id to the role, and those screens then take the role's colour.
- Images: SVG, PNG or WebP up to 512 KiB (favicon: SVG, PNG or ICO up to 64 KiB; e-mail logo: PNG or WebP). An SVG must not contain scripts, event handlers, foreignObject, external references or stylesheets that load resources — such a file is refused with the reason named.
- Fonts: WOFF2 only, up to 300 KiB each, at most two faces, declared in `typography.fontFaces`.
- Links (`product.docsUrl`, `product.supportUrl`) are absolute http(s) URLs; `product.supportEmail` is a plain address.
- Nothing else is read: extra files are ignored, unknown pack keys are refused.

## Applying it

Admin → Branding → "Import branding package". The dry run reports every problem and shows a field-by-field diff before anything changes. The previous brand is kept for one-click rollback.
