// Semantic Versioning 2.0.0 helpers.

const RE = /^(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+([0-9A-Za-z.-]+))?$/;

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

/**
 * @returns {-1|0|1}
 */
export function compare(a, b) {
  const x = toParsed(a);
  const y = toParsed(b);
  for (const k of ['major', 'minor', 'patch']) {
    if (x[k] !== y[k]) return x[k] < y[k] ? -1 : 1;
  }
  const pa = x.prerelease.join('.');
  const pb = y.prerelease.join('.');
  if (pa !== pb) return pa < pb ? -1 : 1;
  const ba = x.build.join('.');
  const bb = y.build.join('.');
  if (ba !== bb) return ba < bb ? -1 : 1;
  return 0;
}

export function sort(versions) {
  return versions.sort(compare);
}
