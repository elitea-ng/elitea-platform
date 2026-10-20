/**
 * Version ordering for the server's `min_client_version` (ADR-0025 decision
 * 5): `MAJOR.MINOR.PATCH`, an optional `-prerelease` and an optional `+build`
 * (ignored when ordering), SemVer 2.0.0 precedence. Like the server, a leading
 * `v` and leading zeros in numeric parts are tolerated.
 */

export interface ParsedVersion {
  core: [number, number, number];
  pre: string[];
}

const MAX_LENGTH = 64;
const IDENTIFIER = /^[0-9A-Za-z-]+$/;
const DIGITS = /^\d+$/;

export function parseVersion(text: string): ParsedVersion | null {
  if (typeof text !== 'string' || text === '' || text.length > MAX_LENGTH) return null;
  let rest = text.startsWith('v') ? text.slice(1) : text;
  const plus = rest.indexOf('+');
  if (plus >= 0) {
    if (!rest.slice(plus + 1).split('.').every((id) => IDENTIFIER.test(id))) return null;
    rest = rest.slice(0, plus);
  }
  let pre: string[] = [];
  const dash = rest.indexOf('-');
  if (dash >= 0) {
    pre = rest.slice(dash + 1).split('.');
    if (!pre.every((id) => IDENTIFIER.test(id))) return null;
    rest = rest.slice(0, dash);
  }
  const parts = rest.split('.');
  if (parts.length !== 3) return null;
  const core: number[] = [];
  for (const part of parts) {
    if (!DIGITS.test(part) || part.length > 9) return null;
    core.push(Number(part));
  }
  return { core: core as [number, number, number], pre };
}

function compareIdentifiers(a: string, b: string): number {
  const aNum = DIGITS.test(a);
  const bNum = DIGITS.test(b);
  if (aNum && bNum) return Math.sign(Number(a) - Number(b));
  if (aNum) return -1; // numeric identifiers sort before alphanumeric ones
  if (bNum) return 1;
  return a < b ? -1 : a > b ? 1 : 0;
}

/** -1, 0 or 1. */
export function compareVersions(a: ParsedVersion, b: ParsedVersion): number {
  for (let i = 0; i < 3; i++) {
    if (a.core[i] !== b.core[i]) return a.core[i] < b.core[i] ? -1 : 1;
  }
  if (a.pre.length === 0 || b.pre.length === 0) {
    return a.pre.length === b.pre.length ? 0 : a.pre.length === 0 ? 1 : -1;
  }
  for (let i = 0; i < Math.min(a.pre.length, b.pre.length); i++) {
    const c = compareIdentifiers(a.pre[i], b.pre[i]);
    if (c !== 0) return c;
  }
  return Math.sign(a.pre.length - b.pre.length);
}

/**
 * True when `version` orders strictly before `minimum`. An empty or
 * unparsable minimum never refuses (the server does the same); an unparsable
 * version against a real minimum is treated as too old (fail safe).
 */
export function isBelowMinimum(version: string, minimum: string | null | undefined): boolean {
  if (!minimum) return false;
  const floor = parseVersion(minimum);
  if (floor === null) return false;
  const got = parseVersion(version);
  if (got === null) return true;
  return compareVersions(got, floor) < 0;
}
