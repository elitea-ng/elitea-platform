#!/usr/bin/env node
// The sample-brand output scan (ADR-0052 decision 11, W4): a client built with
// ELITEA_BRAND=sample must not show ELITEA. The test that keeps white labelling
// easy: a new "Elitea" literal fails CI.
//
//   node scripts/ci/brand-output-scan.mjs --lane <web|mobile|desktop> <path>...
//
// Each <path> is a file or a folder of built output: the web dist folders, the
// mobile `expo export` folder and resolved `expo config`, the desktop brand
// overlay and host binary. Text files are scanned as text; a binary file (Hermes
// bytecode, a native executable) is scanned through its printable strings, as
// `strings` would show them.
//
// WHAT FAILS. A WORD a person reads: `Elitea`, `ELITEA`, `EPAM` or `Epam` on
// its own (not inside a longer identifier such as `EliteaAssistant`, `eliteaFetch`
// or `X-Elitea-Trigger-Token`, which name code, not the product). And, in any
// form, `epam` (case-insensitive) and every ELITEA identifier read from
// brand/elitea/brand.json (bundle ids, app group, schemes, domains, the support
// address), so the snapshot moves when that brand does.
//
// WHAT DOES NOT. Lower-case `elitea` (API paths `/elitea_core/...`, CSS classes,
// storage keys: code, not text), the NOTICE and licence files ([SKIPPED_FILES]),
// and the reviewed ALLOWED entries below.
//
// Node's standard library only.

import { readdirSync, readFileSync, statSync } from 'node:fs';
import { basename, join, relative, resolve } from 'node:path';
import process from 'node:process';

import { isEntrypoint } from './py-compat.mjs';

/** Folders whose files are never scanned: third-party licence texts. */
export const SKIPPED_FILES = [/(^|\/)NOTICE(\.[a-z]+)?$/i, /(^|\/)(LICEN[CS]E|COPYING)[^/]*$/i, /\.LICENSE\.txt$/i, /(^|\/)THIRD[-_]PARTY[^/]*$/i];

/**
 * The reviewed allow-list: brand text that may stay in sample output, each with
 * the reason. An entry is `{ lane, text, why }`: `text` is a substring of the
 * match's context (the 60 characters around it); `lane` limits it to one lane
 * or `*`. Keep it short: every entry is a place white labelling still leaks.
 */
export const ALLOWED = Object.freeze([
  {
    lane: '*',
    text: '`Elitea`,`Epam`',
    why: 'Publish categories (the contract enum `PublishRequest.categories`, v2.yaml): stored values Main validates, shown as category names; renaming them is a data migration.',
  },
  {
    lane: 'desktop',
    text: 'Elitea-',
    why: 'Git commit trailers the handoff and checkpoint code writes and parses (Elitea-Handoff, Elitea-Base, Elitea-Skipped): a wire format, not text.',
  },
]);

/** The ELITEA identifiers brand/elitea/brand.json names, matched in any form. */
export function eliteaIdentifiers(manifest) {
  const ids = new Set([
    manifest.mobile?.bundleId,
    manifest.mobile?.appGroup,
    manifest.mobile?.widgetBundleId,
    manifest.mobile?.oauthRedirectScheme,
    manifest.desktop?.identifier,
    manifest.linkDomain,
    manifest.supportEmail,
    ...(manifest.mobile?.connectSchemes ?? []).map((s) => `${s}://`),
    ...[manifest.webUrl, manifest.privacyUrl, manifest.supportUrl].filter(Boolean).map((u) => new URL(u).host),
  ]);
  ids.delete(undefined);
  ids.delete('');
  return [...ids].sort((a, b) => b.length - a.length);
}

const escape = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

/**
 * The matchers: product words (whole words only), `epam` anywhere, and each
 * identifier anywhere (case-insensitive).
 */
export function matchers(identifiers) {
  return [
    { kind: 'word', re: /(?<![A-Za-z0-9_$-])(?:Elitea|ELITEA|EPAM|Epam)(?![A-Za-z0-9_$])/g },
    // A `-` before the word makes it part of a name (`X-Elitea-Trigger-Token`, a
    // header); a `-` after it does not (`Elitea-Handoff:` is a commit trailer a
    // person reads in `git log`), so that stays a finding unless ALLOWED.
    { kind: 'epam', re: /epam/gi },
    // No word boundary: Hermes bytecode and native string tables pack strings
    // with no separator, so `acme.elitea.ai` can run straight into the next one.
    ...identifiers.map((id) => ({ kind: 'id', re: new RegExp(escape(id), 'gi') })),
  ];
}

/** Printable ASCII runs of four or more characters, as `strings -n 4`. */
export function printableStrings(buffer) {
  const runs = [];
  let start = -1;
  for (let i = 0; i <= buffer.length; i++) {
    const b = i < buffer.length ? buffer[i] : 0;
    const printable = (b >= 0x20 && b < 0x7f) || b === 0x09;
    if (printable && start < 0) start = i;
    if (!printable && start >= 0) {
      if (i - start >= 4) runs.push(buffer.toString('latin1', start, i));
      start = -1;
    }
  }
  return runs.join('\n');
}

/** Whether `buffer` is binary: a NUL byte in its first 8 KiB, as git decides. */
export const isBinary = (buffer) => buffer.subarray(0, 8192).includes(0);

/** The findings in `text` (from `file`) for `lane`, after the allow-list. */
export function scanText(text, { file, lane, identifiers, allowed = ALLOWED }) {
  const findings = [];
  for (const { kind, re } of matchers(identifiers)) {
    re.lastIndex = 0;
    for (const m of text.matchAll(re)) {
      const context = text.slice(Math.max(0, m.index - 30), m.index + m[0].length + 30).replace(/\s+/g, ' ');
      const allow = allowed.find((a) => (a.lane === '*' || a.lane === lane) && context.includes(a.text));
      if (!allow) findings.push({ file, kind, match: m[0], context });
    }
  }
  return findings;
}

function* files(path) {
  const st = statSync(path);
  if (st.isFile()) {
    yield path;
    return;
  }
  for (const entry of readdirSync(path, { withFileTypes: true })) {
    if (entry.name === 'node_modules' || entry.name.startsWith('.')) continue;
    yield* files(join(path, entry.name));
  }
}

/** Scan every file under `paths`. Returns `{ findings, scanned }`. */
export function scan(paths, { lane, identifiers, allowed = ALLOWED, cwd = process.cwd() }) {
  const findings = [];
  let scanned = 0;
  for (const root of paths) {
    for (const file of files(resolve(cwd, root))) {
      const shown = relative(cwd, file) || basename(file);
      if (SKIPPED_FILES.some((re) => re.test(shown))) continue;
      const buffer = readFileSync(file);
      const text = isBinary(buffer) ? printableStrings(buffer) : buffer.toString('utf8');
      scanned++;
      findings.push(...scanText(text, { file: shown, lane, identifiers, allowed }));
    }
  }
  return { findings, scanned };
}

/** One line per distinct finding, sorted, so a CI log reads as a list. */
export function report(findings) {
  const seen = new Map();
  for (const f of findings) {
    const key = `${f.file}\t${f.match}\t${f.context}`;
    seen.set(key, (seen.get(key) ?? 0) + 1);
  }
  return [...seen.keys()].sort().map((key) => {
    const [file, match, context] = key.split('\t');
    return `${file}: "${match}" in …${context}…`;
  });
}

if (isEntrypoint(import.meta.url)) {
  const args = process.argv.slice(2);
  const laneAt = args.indexOf('--lane');
  const lane = laneAt >= 0 ? args[laneAt + 1] : undefined;
  const paths = args.filter((_, i) => i !== laneAt && i !== laneAt + 1);
  if (!['web', 'mobile', 'desktop'].includes(lane) || paths.length === 0) {
    console.log('::error::usage: brand-output-scan.mjs --lane <web|mobile|desktop> <path>...');
    process.exit(2);
  }
  if ((process.env.ELITEA_BRAND ?? '').replace(/^.*\//, '') !== 'sample') {
    console.log('::error::build and scan with ELITEA_BRAND=sample: the scan proves a sample build shows no ELITEA');
    process.exit(2);
  }
  const repo = resolve(new URL('../..', import.meta.url).pathname);
  const manifest = JSON.parse(readFileSync(join(repo, 'brand/elitea/brand.json'), 'utf8'));
  const { findings, scanned } = scan(paths, { lane, identifiers: eliteaIdentifiers(manifest) });
  // Nothing scanned is a broken build path, not a clean build.
  if (scanned === 0) {
    console.log(`::error::brand-output-scan (${lane}): no file under ${paths.join(', ')}`);
    process.exit(1);
  }
  const lines = report(findings);
  for (const line of lines) console.log(`::error::${line}`);
  if (lines.length) {
    console.log(
      `${lines.length} ELITEA literal(s) in the ${lane} sample build. Read the brand from the manifest (scripts/brand/load.mjs, the web brand pack), or add a reviewed ALLOWED entry in scripts/ci/brand-output-scan.mjs.`,
    );
    process.exit(1);
  }
  console.log(`brand-output-scan (${lane}): ${scanned} file(s), no ELITEA literal`);
}
