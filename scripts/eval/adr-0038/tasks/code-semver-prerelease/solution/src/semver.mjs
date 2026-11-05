// Semantic Versioning 2.0.0 helpers.

const NUM = '0|[1-9]\\d*';
const PRE_ID = `(?:${NUM}|\\d*[A-Za-z-][0-9A-Za-z-]*)`;
const BUILD_ID = '[0-9A-Za-z-]+';
const RE = new RegExp(
  `^(${NUM})\\.(${NUM})\\.(${NUM})` +
    `(?:-(${PRE_ID}(?:\\.${PRE_ID})*))?` +
    `(?:\\+(${BUILD_ID}(?:\\.${BUILD_ID})*))?$`,
);

/**
 * @param {string} version
 * @returns {{major:number, minor:number, patch:number, prerelease:string[], build:string[]}}
 */
export function parse(version) {
  const m = RE.exec(String(version).trim());
  if (!m) throw new Error(`invalid version: ${version}`);
  return {
    major: Number(m[1]),
    minor: Number(m[2]),
    patch: Number(m[3]),
    prerelease: m[4] ? m[4].split('.') : [],
    build: m[5] ? m[5].split('.') : [],
  };
}

const toParsed = (v) => (typeof v === 'string' ? parse(v) : v);
const isNumeric = (id) => /^\d+$/.test(id);

function compareIdentifiers(a, b) {
  const an = isNumeric(a);
  const bn = isNumeric(b);
  if (an && bn) {
    // Compare as integers without precision loss for long digit strings.
    if (a.length !== b.length) return a.length < b.length ? -1 : 1;
    return a === b ? 0 : a < b ? -1 : 1;
  }
  if (an) return -1; // numeric identifiers have lower precedence
  if (bn) return 1;
  return a === b ? 0 : a < b ? -1 : 1; // ASCII order
}

/**
 * @returns {-1|0|1}
 */
export function compare(a, b) {
  const x = toParsed(a);
  const y = toParsed(b);
  for (const k of ['major', 'minor', 'patch']) {
    if (x[k] !== y[k]) return x[k] < y[k] ? -1 : 1;
  }
  const px = x.prerelease;
  const py = y.prerelease;
  // A release has higher precedence than any prerelease of the same version.
  if (px.length === 0 || py.length === 0) {
    if (px.length === py.length) return 0;
    return px.length === 0 ? 1 : -1;
  }
  const n = Math.min(px.length, py.length);
  for (let i = 0; i < n; i++) {
    const c = compareIdentifiers(px[i], py[i]);
    if (c !== 0) return c;
  }
  if (px.length === py.length) return 0;
  return px.length < py.length ? -1 : 1;
}

export function sort(versions) {
  return [...versions].sort(compare);
}
