// The brand manifest format (ADR-0052 decision 2): `brand/<id>/brand.json`.
//
// This module IS the schema. It holds the field table, the asset table and
// the validator; the loader (./load.mjs) and the CLI (../brand-check.mjs)
// both use it, and the contributor docs page
// (apps/elitea-docs/src/content/docs/development/brands.mdx) describes it.
// Add a field here first, then to brand/elitea and brand/sample, then to the
// docs page.
//
// Node's standard library only, no I/O: the validator takes parsed JSON.

/** The version of the format this module reads. A manifest names it in `formatVersion`. */
export const FORMAT_VERSION = 1;

/** A brand id: lowercase, starts with a letter, 2 to 32 characters. */
export const BRAND_ID = /^[a-z][a-z0-9-]{1,31}$/;

/**
 * Folder names under `brand/` that belong to the brand guide site
 * (brand/AGENTS.md), never to a brand.
 */
export const RESERVED_IDS = Object.freeze(['assets', 'scripts', 'test', 'dist']);

/**
 * Fields that are permanent once the brand uploads its first build: the
 * stores key the app on them, and the desktop identifier names the folder
 * that holds every user's data. brand-check prints a marker next to each.
 */
export const PERMANENT_FIELDS = Object.freeze({
  'mobile.bundleId': 'iOS bundle id and Android package name; the stores key the app on it',
  'mobile.appGroup': 'iOS App Group; the widget extension and the app share data through it',
  'desktop.identifier': 'desktop app identifier; names the config, data, log and credentials folders',
});

/**
 * Every asset a brand folder holds, relative to `assets/`. `kind` is the file
 * type; `size` is the PNG edge in pixels (all are square); `sizes` lists the
 * edges an .ico or .icns must contain; `alpha` is true when the image must
 * have an alpha channel, false when it must be opaque, and absent when
 * either is fine. Optional assets are checked only when present.
 */
export const ASSETS = Object.freeze([
  { path: 'ios/icon-1024.png', kind: 'png', size: 1024, alpha: false, use: 'iOS app icon and App Store listing (Apple rejects alpha)' },
  { path: 'ios/app.icon/icon.json', kind: 'json', optional: true, use: 'iOS 26 Icon Composer document (with its Assets/ folder)' },
  { path: 'android/icon-512.png', kind: 'png', size: 512, alpha: false, use: 'Google Play listing icon' },
  { path: 'android/adaptive-foreground.png', kind: 'png', size: 1024, alpha: true, use: 'Android adaptive icon foreground (108 dp canvas)' },
  { path: 'android/adaptive-background.png', kind: 'png', size: 1024, alpha: false, use: 'Android adaptive icon background' },
  { path: 'android/adaptive-monochrome.png', kind: 'png', size: 1024, alpha: true, use: 'Android 13+ themed (monochrome) icon' },
  { path: 'macos/icon.icns', kind: 'icns', sizes: [16, 32, 64, 128, 256, 512, 1024], use: 'macOS app icon' },
  { path: 'windows/icon.ico', kind: 'ico', sizes: [16, 24, 32, 48, 64, 128, 256], use: 'Windows app and installer icon' },
  { path: 'desktop/32x32.png', kind: 'png', size: 32, use: 'desktop bundle icon' },
  { path: 'desktop/128x128.png', kind: 'png', size: 128, use: 'desktop bundle icon' },
  { path: 'desktop/128x128@2x.png', kind: 'png', size: 256, use: 'desktop bundle icon' },
  { path: 'desktop/icon.png', kind: 'png', size: 512, use: 'desktop window and Linux icon' },
  { path: 'desktop/tray-template.png', kind: 'png', size: 44, alpha: true, use: 'macOS menu bar template image (22 pt @2x)' },
  { path: 'splash/splash-icon.png', kind: 'png', size: 1024, use: 'mobile splash screen image' },
  { path: 'web/favicon.svg', kind: 'svg', use: 'web favicon' },
  { path: 'web/favicon-48.png', kind: 'png', size: 48, use: 'mobile web favicon' },
  { path: 'logo/logo-full.svg', kind: 'svg', use: 'lockup: mark and product name' },
  { path: 'logo/logo-mark.svg', kind: 'svg', use: 'the mark alone' },
]);

/** Optional folders and files beside `assets/` (ADR-0052 decision 2). */
export const OPTIONAL_ENTRIES = Object.freeze(['pack.json', 'firebase', 'store', 'README.md']);

/** The files `firebase/` may hold. */
export const FIREBASE_FILES = Object.freeze(['GoogleService-Info.plist', 'google-services.json']);

/** The two scheme backgrounds the hue is checked against when the manifest sets none. */
export const DEFAULT_BACKGROUNDS = Object.freeze({ light: '#FFFFFF', dark: '#0B0B18' });

/** WCAG 2.2 minimums the brand hue must meet. */
export const CONTRAST = Object.freeze({
  // 1.4.11 non-text contrast: the hue as a control fill, focus ring or icon on each background.
  hueOnBackground: 3,
  // 1.4.3 text contrast: the label colour (`brand.onBrand`) on a hue-filled button.
  onBrandOnHue: 4.5,
});

const REVERSE_DNS = /^[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*){2,}$/;
const REVERSE_DNS_LABEL_MAX = 63;
const URI_SCHEME = /^[a-z][a-z0-9+.-]*$/;
const RESERVED_SCHEMES = new Set(['http', 'https', 'file', 'ftp', 'mailto', 'tel', 'sms', 'data', 'javascript', 'about', 'blob', 'content', 'intent']);
const HOST = /^(?=.{1,253}$)([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$/;
const EMAIL = /^[^\s@<>()",;:]+@([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$/i;
const HEX = /^#([0-9a-f]{3}|[0-9a-f]{6})$/i;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const EXPO_ACCOUNT = /^[a-z0-9][a-z0-9_-]*$/i;
const RELAY_OWNER = /^[a-z][a-z0-9-]{1,31}$/;
const SCHEME_TOKEN = /^[A-Za-z][A-Za-z0-9.]*$/;

const isObject = (v) => v !== null && typeof v === 'object' && !Array.isArray(v);
const isText = (v, max = 200) => typeof v === 'string' && v.trim() === v && v.length > 0 && v.length <= max && !/[\u0000-\u001f]/.test(v);

/** True for a reverse-DNS identifier such as `ai.elitea.app` (three or more labels, lowercase). */
export function isReverseDns(value) {
  return typeof value === 'string' && value.length <= 155 && REVERSE_DNS.test(value) && value.split('.').every((l) => l.length <= REVERSE_DNS_LABEL_MAX && !l.endsWith('-'));
}

// Java keywords and literals: not allowed as a segment of an Android package name.
const JAVA_RESERVED = new Set('abstract assert boolean break byte case catch char class const continue default do double else enum extends false final finally float for goto if implements import instanceof int interface long native new null package private protected public return short static strictfp super switch synchronized this throw throws transient true try void volatile while'.split(' '));

/**
 * True for an app id that is valid as BOTH an iOS bundle id and an Android
 * package name: reverse-DNS, labels of lowercase letters and digits only
 * (Android forbids "-", iOS forbids "_"), no Java keyword as a label.
 */
export function isAppId(value) {
  return isReverseDns(value) && value.split('.').every((l) => /^[a-z][a-z0-9]*$/.test(l) && !JAVA_RESERVED.has(l));
}

/** True for a host name such as `elitea.ai` (lowercase, a public-looking TLD, no port). */
export function isHost(value) {
  return typeof value === 'string' && HOST.test(value);
}

/**
 * True for an absolute https URL with a host name, no port, credentials or
 * fragment, written in its normal form (what `new URL(value).href` prints;
 * a bare origin may leave out the trailing "/").
 */
export function isHttpsUrl(value) {
  if (typeof value !== 'string') return false;
  let url;
  try {
    url = new URL(value);
  } catch {
    return false;
  }
  if (url.protocol !== 'https:' || !isHost(url.hostname) || url.port || url.username || url.password || url.hash) return false;
  return url.href === value || (url.pathname === '/' && !url.search && url.href === `${value}/`);
}

/** True for a custom URI scheme a mobile app may register (RFC 3986, lowercase, not a well-known one). */
export function isAppScheme(value) {
  return typeof value === 'string' && value.length <= 64 && URI_SCHEME.test(value) && !RESERVED_SCHEMES.has(value);
}

/** `#rgb` or `#rrggbb` → `[r, g, b]` in 0–255, or null. */
export function parseHex(value) {
  if (typeof value !== 'string' || !HEX.test(value)) return null;
  const h = value.length === 4 ? [...value.slice(1)].map((c) => c + c).join('') : value.slice(1);
  return [0, 2, 4].map((i) => parseInt(h.slice(i, i + 2), 16));
}

/** WCAG 2.2 relative luminance of an `[r, g, b]` colour. */
export function luminance([r, g, b]) {
  const lin = (c) => {
    const v = c / 255;
    return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

/** WCAG 2.2 contrast ratio of two hex colours (1 to 21). */
export function contrastRatio(a, b) {
  const la = luminance(parseHex(a));
  const lb = luminance(parseHex(b));
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

/**
 * The colours the contrast check uses: the hue, its label colour and the two
 * scheme backgrounds (`brand.schemes.<scheme>["background.default"]` when the
 * manifest sets it as a hex colour, else Paper and Ink).
 */
export function brandColours(manifest) {
  const b = manifest.brand ?? {};
  const bg = (scheme) => {
    const v = b.schemes?.[scheme]?.['background.default'];
    return parseHex(v) ? v : DEFAULT_BACKGROUNDS[scheme];
  };
  return { hue: b.hue, onBrand: b.onBrand ?? '#FFFFFF', light: bg('light'), dark: bg('dark') };
}

/**
 * Contrast findings for the manifest's hue: an array of
 * `{ pair, ratio, min, ok }`. Empty when the hue does not parse (the
 * structural validator reports that).
 */
export function contrastFindings(manifest) {
  const c = brandColours(manifest);
  if (!parseHex(c.hue) || !parseHex(c.onBrand)) return [];
  const row = (pair, a, b, min) => {
    const ratio = contrastRatio(a, b);
    return { pair, ratio: Math.round(ratio * 100) / 100, min, ok: ratio >= min };
  };
  return [
    row(`brand.hue ${c.hue} on the light background ${c.light}`, c.hue, c.light, CONTRAST.hueOnBackground),
    row(`brand.hue ${c.hue} on the dark background ${c.dark}`, c.hue, c.dark, CONTRAST.hueOnBackground),
    row(`brand.onBrand ${c.onBrand} on brand.hue ${c.hue}`, c.onBrand, c.hue, CONTRAST.onBrandOnHue),
  ];
}

/**
 * Validate a parsed `brand.json`. Returns an array of problems, each a string
 * that starts with the field path; empty means valid. Checks structure,
 * shapes and the relations between fields; it reads no files (assets are
 * brand-check's).
 */
export function validateManifest(m) {
  const errors = [];
  const fail = (path, msg) => errors.push(`${path}: ${msg}`);
  if (!isObject(m)) return ['brand.json: must be a JSON object'];

  const allowed = (obj, path, keys) => {
    for (const k of Object.keys(obj)) if (!keys.includes(k)) fail(path ? `${path}.${k}` : k, 'unknown field');
  };
  allowed(m, '', ['$comment', 'formatVersion', 'id', 'productName', 'shortName', 'publisher', 'mobile', 'desktop', 'linkDomain', 'webUrl', 'privacyUrl', 'supportUrl', 'supportEmail', 'docsUrl', 'brand', 'eas', 'updates', 'push']);

  if (m.formatVersion !== FORMAT_VERSION) fail('formatVersion', `must be ${FORMAT_VERSION}`);

  if (typeof m.id !== 'string' || !BRAND_ID.test(m.id)) fail('id', 'must be 2 to 32 characters: a lowercase letter, then lowercase letters, digits or "-"');
  else if (RESERVED_IDS.includes(m.id)) fail('id', `"${m.id}" is reserved for the brand guide (${RESERVED_IDS.join(', ')})`);

  if (!isText(m.productName, 40)) fail('productName', 'must be 1 to 40 characters, no surrounding spaces');
  if (!isText(m.shortName, 20)) fail('shortName', 'must be 1 to 20 characters, no surrounding spaces');
  if (!isText(m.publisher, 120)) fail('publisher', 'must be the publisher\'s legal name, 1 to 120 characters');

  // mobile
  const ids = [];
  if (!isObject(m.mobile)) fail('mobile', 'must be an object');
  else {
    const mob = m.mobile;
    allowed(mob, 'mobile', ['bundleId', 'connectSchemes', 'oauthRedirectScheme', 'appGroup', 'widgetBundleId']);
    if (!isAppId(mob.bundleId)) fail('mobile.bundleId', 'must be a reverse-DNS id of three or more labels, each a lowercase letter then letters or digits (valid on iOS and Android), e.g. com.example.app');
    else ids.push(['mobile.bundleId', mob.bundleId]);
    if (!Array.isArray(mob.connectSchemes) || mob.connectSchemes.length === 0) fail('mobile.connectSchemes', 'must be a non-empty array of URI schemes');
    else {
      mob.connectSchemes.forEach((s, i) => {
        if (!isAppScheme(s)) fail(`mobile.connectSchemes[${i}]`, `"${s}" is not a lowercase custom URI scheme (or it is a well-known one)`);
      });
      if (new Set(mob.connectSchemes).size !== mob.connectSchemes.length) fail('mobile.connectSchemes', 'holds a scheme twice');
    }
    if (!isReverseDns(mob.oauthRedirectScheme)) fail('mobile.oauthRedirectScheme', 'must be a reverse-DNS private-use scheme (RFC 8252 section 7.1)');
    else {
      if (Array.isArray(mob.connectSchemes) && mob.connectSchemes.includes(mob.oauthRedirectScheme)) fail('mobile.oauthRedirectScheme', 'must not also be a connect scheme');
      if (isAppId(mob.bundleId) && mob.oauthRedirectScheme !== mob.bundleId && !mob.oauthRedirectScheme.startsWith(`${mob.bundleId}.`)) {
        fail('mobile.oauthRedirectScheme', `must be the bundle id or start with "${mob.bundleId}." (the scheme names a domain the brand controls)`);
      }
    }
    if (typeof mob.appGroup !== 'string' || !mob.appGroup.startsWith('group.') || !isReverseDns(mob.appGroup.slice(6))) fail('mobile.appGroup', 'must be "group." followed by a reverse-DNS id');
    else ids.push(['mobile.appGroup', mob.appGroup]);
    if (!isReverseDns(mob.widgetBundleId)) fail('mobile.widgetBundleId', 'must be a reverse-DNS id');
    else if (isAppId(mob.bundleId) && !mob.widgetBundleId.startsWith(`${mob.bundleId}.`)) fail('mobile.widgetBundleId', `must start with "${mob.bundleId}." (Apple requires an extension id under its app's id)`);
    else ids.push(['mobile.widgetBundleId', mob.widgetBundleId]);
  }

  // desktop
  if (!isObject(m.desktop)) fail('desktop', 'must be an object');
  else {
    allowed(m.desktop, 'desktop', ['identifier']);
    if (!isReverseDns(m.desktop.identifier)) fail('desktop.identifier', 'must be a lowercase reverse-DNS id with three or more labels');
    else if (m.desktop.identifier.endsWith('.app')) fail('desktop.identifier', 'must not end in ".app" (it clashes with the macOS bundle extension)');
    else ids.push(['desktop.identifier', m.desktop.identifier]);
  }
  for (let i = 0; i < ids.length; i++) {
    for (let j = i + 1; j < ids.length; j++) if (ids[i][1] === ids[j][1]) fail(ids[j][0], `repeats ${ids[i][0]} ("${ids[i][1]}"); every identifier must be unique`);
  }

  // links
  if (m.linkDomain !== null && !isHost(m.linkDomain)) fail('linkDomain', 'must be a lowercase host name (no scheme, port or path), or null for no universal links');
  for (const k of ['webUrl', 'privacyUrl', 'supportUrl']) if (!isHttpsUrl(m[k])) fail(k, 'must be an absolute https URL with a host name, no port, credentials or fragment');
  if (m.supportEmail !== null && !(typeof m.supportEmail === 'string' && EMAIL.test(m.supportEmail))) fail('supportEmail', 'must be an email address, or null');
  if (!(isHttpsUrl(m.docsUrl) || (typeof m.docsUrl === 'string' && /^\/[A-Za-z0-9._~/-]*$/.test(m.docsUrl) && !m.docsUrl.startsWith('//')))) fail('docsUrl', 'must be an https URL or a path on the workspace server starting with "/"');

  // brand colours
  if (!isObject(m.brand)) fail('brand', 'must be an object');
  else {
    allowed(m.brand, 'brand', ['hue', 'onBrand', 'schemes']);
    if (!parseHex(m.brand.hue)) fail('brand.hue', 'must be a hex colour, #rgb or #rrggbb');
    if (m.brand.onBrand !== undefined && !parseHex(m.brand.onBrand)) fail('brand.onBrand', 'must be a hex colour');
    if (m.brand.schemes !== undefined) {
      if (!isObject(m.brand.schemes)) fail('brand.schemes', 'must be an object with "light" and/or "dark"');
      else {
        allowed(m.brand.schemes, 'brand.schemes', ['light', 'dark']);
        for (const [s, rec] of Object.entries(m.brand.schemes)) {
          if (!isObject(rec)) {
            fail(`brand.schemes.${s}`, 'must be an object of token id to colour');
            continue;
          }
          for (const [tok, val] of Object.entries(rec)) {
            if (!SCHEME_TOKEN.test(tok)) fail(`brand.schemes.${s}`, `"${tok}" is not a token id`);
            if (!isText(val, 120)) fail(`brand.schemes.${s}.${tok}`, 'must be a CSS colour string');
          }
          if (rec['background.default'] !== undefined && !parseHex(rec['background.default'])) fail(`brand.schemes.${s}.background.default`, 'must be a hex colour (the contrast check reads it)');
        }
      }
    }
  }

  // eas
  if (!isObject(m.eas)) fail('eas', 'must be an object ({ "projectId": null, "owner": null } when not linked)');
  else {
    allowed(m.eas, 'eas', ['projectId', 'owner']);
    if (m.eas.projectId !== null && !(typeof m.eas.projectId === 'string' && UUID.test(m.eas.projectId))) fail('eas.projectId', 'must be the EAS project UUID, or null');
    if (m.eas.owner !== null && !(typeof m.eas.owner === 'string' && EXPO_ACCOUNT.test(m.eas.owner))) fail('eas.owner', 'must be an Expo account or organization name, or null');
    if ((m.eas.projectId === null) !== (m.eas.owner === null)) fail('eas', 'projectId and owner are set together or not at all');
  }

  // updates
  if (!isObject(m.updates)) fail('updates', 'must be an object, e.g. { "mode": "off" }');
  else {
    allowed(m.updates, 'updates', ['mode', 'codeSigningCertificate']);
    if (m.updates.mode !== 'on' && m.updates.mode !== 'off') fail('updates.mode', 'must be "on" or "off"');
    if (m.updates.mode === 'on') {
      if (!isObject(m.eas) || !m.eas.projectId) fail('updates.mode', '"on" needs the brand\'s own eas.projectId (an update never crosses brands)');
      if (!isRelativeFile(m.updates.codeSigningCertificate)) fail('updates.codeSigningCertificate', '"on" needs the path of the code signing certificate, relative to the brand folder');
    } else if (m.updates.codeSigningCertificate !== undefined && !isRelativeFile(m.updates.codeSigningCertificate)) {
      fail('updates.codeSigningCertificate', 'must be a path relative to the brand folder');
    }
  }

  // push
  if (!isObject(m.push)) fail('push', 'must be an object, e.g. { "relay": "none" }');
  else {
    allowed(m.push, 'push', ['relay']);
    if (typeof m.push.relay !== 'string' || !(m.push.relay === 'none' || RELAY_OWNER.test(m.push.relay))) fail('push.relay', 'must be "none" or the relay owner\'s name (lowercase letters, digits, "-")');
  }

  return errors;
}

/** A path inside the brand folder: relative, forward slashes, no "..". */
export function isRelativeFile(value) {
  return typeof value === 'string' && value.length > 0 && !value.startsWith('/') && !value.includes('\\') && value.split('/').every((p) => p !== '' && p !== '.' && p !== '..');
}

/** Read a dotted path such as `mobile.bundleId` from a manifest. */
export function fieldValue(manifest, path) {
  return path.split('.').reduce((v, k) => (v == null ? undefined : v[k]), manifest);
}
