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

import { BRANDS_DIR, REPO_ROOT, resolveBrandDir } from '../brand/load.mjs';
import { isEntrypoint } from './py-compat.mjs';

/** Folders whose files are never scanned: third-party licence texts. */
export const SKIPPED_FILES = [/(^|\/)NOTICE(\.[a-z]+)?$/i, /(^|\/)(LICEN[CS]E|COPYING)[^/]*$/i, /\.LICENSE\.txt$/i, /(^|\/)THIRD[-_]PARTY[^/]*$/i];

/**
 * The reviewed allow-list: brand text that may stay in sample output, each with
 * the reason. An entry is `{ lane, text, why }` or `{ lane, re, why }`:
 * `text` is a substring of the match's context (the 60 characters around it),
 * `re` a pattern tested on it; `lane` limits it to one lane or `*`. Keep it short: every entry is a place white labelling still leaks.
 */
export const ALLOWED = Object.freeze([
  {
    lane: '*',
    text: '`Elitea`,`Epam`',
    why: 'Publish categories (the contract enum `PublishRequest.categories`, v2.yaml): stored values Main validates, shown as category names; renaming them is a data migration.',
  },
  {
    lane: 'web',
    text: 'elitea.ai/schemas/brand-pack/1.json',
    why: "The brand pack's `$schema` id (v2.yaml BrandPack, Main's product_default.pack.json): a contract identifier the client validates, never shown or fetched.",
  },
  {
    lane: 'desktop',
    // Only the names the code writes: any other `Elitea-…` text still fails.
    re: /Elitea-(?:Handoff|Base|Skipped|Included|Upstream|Agent-Assertion)\b/,
    why: 'Git commit trailers and the remote-agent header the handoff, checkpoint and assertion code write and parse (Elitea-Handoff, Elitea-Base, Elitea-Skipped, Elitea-Agent-Assertion): a wire format Main reads too, not text.',
  },
  {
    lane: 'desktop',
    text: 'elitea.invalid',
    why: "The author address of the local git commits checkpoints and handoffs make (RFC 2606 `.invalid`, never delivered); 'Elitea merge', 'Elitea checkpoints', 'Elitea handoff' and 'Elitea cloud executor' are the matching author names. Renaming them would split one repository's history in two.",
  },
  {
    lane: 'desktop',
    text: 'Elitea handoff',
    why: 'The handoff commit identity and subject (local-tools handoff/snapshot.rs IDENTITY): the handoff code finds its own commits by it.',
  },
  {
    lane: 'desktop',
    text: 'Elitea cloud turn',
    why: 'The subject of a replayed cloud commit with no message of its own (handoff/apply.rs), in the same commit identity family as above.',
  },
  {
    lane: 'desktop',
    text: 'mcp_elitea_internal_',
    why: "Main's internal toolkit names (services/elitea-main/internal/mcpregistry/internal_builders.go: Elitea Applications, Elitea Chat, ...), mirrored by agent-runtime snapshot.rs: server data, renamed on the server.",
  },
  {
    lane: 'desktop',
    // The list is one string table: the id runs straight into the next
    // reverse-DNS entry (`com.`, `org.`, ...) or ends its run (the context
    // reads a run break as a space). A longer form (`ai.elitea.desktop.plist`)
    // still fails.
    re: /ai\.elitea\.desktop(?:(?:com|org|net|io|app|ai|dev)\.|\s|$)/,
    why: "ELITEA's bundle id on the computer-use platform deny list (local-tools policy.rs PLATFORM_APP_DENY, ADR-0047 decision 5). Every brand keeps it: a white-label build must not drive an installed ELITEA app, another agent host. The build's own id is refused through brand::own_apps().",
  },
  {
    lane: 'desktop',
    text: 'Generated with Elitea AI',
    why: "The disclosure marker's default text (libs/proto/contracts/tool-disclosure-v1.json default_marker_text): a cross-language contract the administrator overrides per platform.",
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

/**
 * Whether the match at `[index, index + length)` lies inside a bundler content
 * hash: the 8-character token of an asset name, `name-BCePam0p.js`, or of a
 * reference to one. Such a token is a hash of the file's bytes, so any term of
 * four letters turns up in one now and then (`BCePam0p` holds `epam`); it is
 * not text. Only a token of exactly eight URL-safe characters, set off by `-`
 * and followed by a file extension, counts: `Epam.js` or `x-EPAM.js` is still
 * a finding. The hash alphabet is base64url, so a token may hold `-` itself
 * (`railroad-ebnf-DWYD2UWJ-nePAm-w5.js`); such a token counts only when it also
 * holds a digit, so a name like `foo-bar-EPAM.js` is still a finding.
 */
export function insideContentHash(text, index, length) {
  const from = Math.max(0, index - 12);
  const window = text.slice(from, index + length + 12);
  // Every `-` may open a token (a token can hold `-`), so the match is a lookahead and the tokens may overlap.
  for (const m of window.matchAll(/-(?=([A-Za-z0-9_-]{8})\.[a-z0-9]{2,5}(?![A-Za-z0-9]))/g)) {
    const token = m[1];
    if (token.includes('-') && !/\d/.test(token)) continue;
    const start = from + m.index + 1;
    if (index >= start && index + length <= start + 8) return true;
  }
  return false;
}

/**
 * Whether the match lies inside a CSP source hash, `'sha256-<base64>'` (sha384, sha512 too). Tauri writes the
 * hash of every inline script and style into the host binary's CSP; it is base64 of a digest, so a four-letter
 * term turns up in one now and then (`…QTObaePAmaA=`), and it changes with every web build. Only the base64
 * body of a complete, quoted source expression counts.
 */
export function insideCspHash(text, index, length) {
  const from = Math.max(0, index - 100);
  const window = text.slice(from, index + length + 100);
  for (const m of window.matchAll(/'sha(?:256|384|512)-([A-Za-z0-9+/]{20,}={0,2})'/g)) {
    const start = from + m.index + m[0].indexOf('-') + 1;
    if (index >= start && index + length <= start + m[1].length) return true;
  }
  return false;
}

const printableByte = (b) => (b >= 0x20 && b < 0x7f) || b === 0x09;

/**
 * The printable runs of four or more characters in `buffer`: single-byte
 * ASCII, as `strings -n 4`, then UTF-16LE at both byte alignments, as
 * `strings -el` (Hermes stores a string with any non-ASCII character as
 * UTF-16, so `Elitea’s` is `E\0l\0i\0...`; the non-ASCII unit ends the run).
 */
export function* printableRuns(buffer) {
  let start = -1;
  for (let i = 0; i <= buffer.length; i++) {
    const printable = i < buffer.length && printableByte(buffer[i]);
    if (printable && start < 0) start = i;
    if (!printable && start >= 0) {
      if (i - start >= 4) yield buffer.toString('latin1', start, i);
      start = -1;
    }
  }
  for (const offset of [0, 1]) {
    let begin = -1;
    for (let i = offset; i + 1 <= buffer.length; i += 2) {
      const printable = i + 1 < buffer.length && buffer[i + 1] === 0 && printableByte(buffer[i]);
      if (printable && begin < 0) begin = i;
      if (!printable && begin >= 0) {
        if (i - begin >= 8) yield buffer.toString('utf16le', begin, i);
        begin = -1;
      }
    }
  }
}

/** The runs joined, one per line, in pieces of about `size` characters (a debug host is 250 MB). */
export function* printableChunks(buffer, size = 4 << 20) {
  let parts = [];
  let length = 0;
  for (const run of printableRuns(buffer)) {
    parts.push(run);
    length += run.length + 1;
    if (length >= size) {
      yield parts.join('\n');
      parts = [];
      length = 0;
    }
  }
  if (parts.length) yield parts.join('\n');
}

/** All the runs as one text, as `strings` prints them. */
export const printableStrings = (buffer) => [...printableChunks(buffer, Infinity)].join('\n');

/** Whether `buffer` is binary: a NUL byte in its first 8 KiB, as git decides. */
export const isBinary = (buffer) => buffer.subarray(0, 8192).includes(0);

/** The shortest printable run, in a binary, that `epam` is read in (binaryNoise). */
export const MIN_BINARY_RUN = 8;
const NATURAL_EPAM = new Set(['epam', 'EPAM', 'Epam']);

/**
 * Whether an `epam` hit in a binary's printable runs is noise, not text. A packaged host carries megabytes of
 * compressed assets (Tauri embeds the web bundle brotli-compressed), which read as random bytes: a
 * case-insensitive four-letter match turns up in them about once per 250 MB (`… #R3O fZ2z/ ePAm eM0_J_ …`).
 * Text that names the brand is written in one of its natural cases, inside a string of some length (a URL, a
 * sentence, a packed string table). So in a binary the hit must be `epam`, `EPAM` or `Epam`, in a run of at
 * least MIN_BINARY_RUN characters; the word matcher and the identifiers are unchanged.
 */
export function binaryNoise(text, index, match) {
  if (!NATURAL_EPAM.has(match)) return true;
  const start = text.lastIndexOf('\n', index) + 1;
  const end = text.indexOf('\n', index);
  return (end < 0 ? text.length : end) - start < MIN_BINARY_RUN;
}

/** The findings in `text` (from `file`) for `lane`, after the allow-list. `binary`: the text is a binary's runs. */
export function scanText(text, { file, lane, identifiers, allowed = ALLOWED, compiled = matchers(identifiers), binary = false }) {
  const findings = [];
  for (const { kind, re } of compiled) {
    for (const m of text.matchAll(re)) {
      if (kind === 'epam' && (insideContentHash(text, m.index, m[0].length) || insideCspHash(text, m.index, m[0].length))) continue;
      if (kind === 'epam' && binary && binaryNoise(text, m.index, m[0])) continue;
      const context = text.slice(Math.max(0, m.index - 30), m.index + m[0].length + 30).replace(/\s+/g, ' ');
      const allow = allowed.find((a) => (a.lane === '*' || a.lane === lane) && (a.re ? a.re.test(context) : context.includes(a.text)));
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
  const compiled = matchers(identifiers);
  let scanned = 0;
  for (const root of paths) {
    for (const file of files(resolve(cwd, root))) {
      const shown = relative(cwd, file) || basename(file);
      if (SKIPPED_FILES.some((re) => re.test(shown))) continue;
      const buffer = readFileSync(file);
      const binary = isBinary(buffer);
      const texts = binary ? printableChunks(buffer) : [buffer.toString('utf8')];
      scanned++;
      for (const text of texts) findings.push(...scanText(text, { file: shown, lane, identifiers, allowed, compiled, binary }));
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
  let brandDir;
  try {
    brandDir = resolveBrandDir(process.env.ELITEA_BRAND);
  } catch {
    brandDir = undefined;
  }
  if (brandDir !== join(BRANDS_DIR, 'sample')) {
    console.log('::error::build and scan with ELITEA_BRAND=sample: the scan proves a sample build shows no ELITEA');
    process.exit(2);
  }
  const manifest = JSON.parse(readFileSync(join(REPO_ROOT, 'brand/elitea/brand.json'), 'utf8'));
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
