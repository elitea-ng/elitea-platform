#!/usr/bin/env node
/**
 * Release check (docs/release.md): the selected brand's `linkDomain` serves
 * well-known link files that name THIS brand's app and signing certificate, so
 * universal links and verified App Links open it (ADR-0052 decision 11).
 *
 *   ELITEA_BRAND=<id> APPLE_TEAM_ID=<team> ANDROID_CERT_SHA256=<fp>[,<fp>] \
 *     npm run check:link-files
 *
 * The team id and the fingerprint (Play Console, App integrity, "App signing key
 * certificate") belong to the brand owner's accounts, so they are not in the
 * manifest. A brand with no `linkDomain` passes: it declares no links.
 * Exit 1 when a file is missing or wrong, 2 on a usage error.
 */
import process from 'node:process';

import { loadBrand } from '../../../scripts/brand/load.mjs';
import { aasaProblems, assetlinksProblems, normalizeFingerprint } from './link-files.mjs';

const brand = loadBrand();
const { linkDomain, mobile } = brand.manifest;
if (!linkDomain) {
  console.log(`link files [${brand.id}]: no linkDomain, no universal links or App Links declared`);
  process.exit(0);
}
const team = process.env.APPLE_TEAM_ID?.trim() ?? '';
const fingerprints = (process.env.ANDROID_CERT_SHA256 ?? '').split(',').filter((s) => s.trim()).map(normalizeFingerprint);
if (!/^[A-Z0-9]{10}$/.test(team) || fingerprints.length === 0 || fingerprints.includes(null)) {
  console.error('usage: APPLE_TEAM_ID=<10-character team id> ANDROID_CERT_SHA256=<sha-256 fingerprint>[,...] npm run check:link-files');
  process.exit(2);
}

async function fetchJson(path) {
  const url = `https://${linkDomain}${path}`;
  const res = await fetch(url, { redirect: 'error', signal: AbortSignal.timeout(15_000) });
  if (!res.ok) throw new Error(`${url}: HTTP ${res.status}`);
  return res.json();
}

const problems = [];
for (const [path, check] of [
  ['/.well-known/apple-app-site-association', (doc) => aasaProblems(doc, `${team}.${mobile.bundleId}`)],
  ['/.well-known/assetlinks.json', (doc) => assetlinksProblems(doc, mobile.bundleId, fingerprints)],
]) {
  try {
    problems.push(...check(await fetchJson(path)));
  } catch (err) {
    problems.push(`${path}: ${err.message}`);
  }
}
if (problems.length) {
  for (const p of problems) console.error(`link files [${brand.id}]: ${p}`);
  process.exit(1);
}
console.log(`link files [${brand.id}]: ${linkDomain} names ${team}.${mobile.bundleId} and ${mobile.bundleId} with its certificate`);
