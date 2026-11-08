// The well-known link files a brand's `linkDomain` must serve (ADR-0052 decision 11):
// `/.well-known/apple-app-site-association` naming `<Apple team id>.<bundle id>` for
// the connect path, and `/.well-known/assetlinks.json` naming the package with the
// SHA-256 fingerprint of the certificate the store signs it with. Pure checks here;
// check-link-files.mjs fetches. Node's standard library only.

/** The app config's link path (app.config.ts LINK_PATH_PREFIX). */
export const LINK_PATH = '/connect';

const SHA256 = /^([0-9A-F]{2}:){31}[0-9A-F]{2}$/;

/** `AB:CD:...` (32 bytes) from any case, with or without colons; null when malformed. */
export function normalizeFingerprint(value) {
  const hex = String(value).trim().replace(/:/g, '').toUpperCase();
  if (!/^[0-9A-F]{64}$/.test(hex)) return null;
  const out = hex.match(/../g).join(':');
  return SHA256.test(out) ? out : null;
}

/** Does an AASA `components` or `paths` entry cover `LINK_PATH`? */
function coversConnect(detail) {
  const matches = (pattern) => {
    if (typeof pattern !== 'string' || pattern.startsWith('NOT ')) return false;
    const re = new RegExp(`^${pattern.replace(/[.+^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '.*').replace(/\?/g, '.')}$`);
    return re.test(LINK_PATH);
  };
  if (Array.isArray(detail.components)) return detail.components.some((c) => c && !c.exclude && matches(c['/'] ?? '*'));
  if (Array.isArray(detail.paths)) return detail.paths.some(matches);
  return false;
}

/** Problems with an apple-app-site-association document for `appId` (`TEAMID.bundle.id`). */
export function aasaProblems(doc, appId) {
  const details = doc?.applinks?.details;
  if (!Array.isArray(details)) return ['apple-app-site-association: no applinks.details array'];
  const mine = details.filter((d) => (Array.isArray(d.appIDs) ? d.appIDs : [d.appID]).includes(appId));
  if (mine.length === 0) return [`apple-app-site-association: no applinks entry names ${appId}`];
  if (!mine.some(coversConnect)) return [`apple-app-site-association: the entry for ${appId} does not cover ${LINK_PATH}`];
  return [];
}

/** Problems with an assetlinks.json document for `packageName` signed with any of `fingerprints`. */
export function assetlinksProblems(doc, packageName, fingerprints) {
  if (!Array.isArray(doc)) return ['assetlinks.json: not a JSON array'];
  const mine = doc.filter(
    (s) =>
      Array.isArray(s?.relation) &&
      s.relation.includes('delegate_permission/common.handle_all_urls') &&
      s.target?.namespace === 'android_app' &&
      s.target?.package_name === packageName,
  );
  if (mine.length === 0) return [`assetlinks.json: no handle_all_urls statement for ${packageName}`];
  const served = new Set(mine.flatMap((s) => (s.target.sha256_cert_fingerprints ?? []).map(normalizeFingerprint)));
  const missing = fingerprints.filter((f) => !served.has(f));
  return missing.length ? [`assetlinks.json: ${packageName} is not linked to certificate ${missing.join(', ')}`] : [];
}
