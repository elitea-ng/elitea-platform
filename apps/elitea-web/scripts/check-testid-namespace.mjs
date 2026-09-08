#!/usr/bin/env node
/**
 * check-testid-namespace.mjs — one test-id namespace for this app (issue #312).
 *
 * WHAT THE ISSUE ACTUALLY ASKED FOR, AND WHAT THIS ENFORCES
 *
 * The legacy UI sync recorded three living test-id namespaces: legacy
 * EliteaUI's promoted ids, the QA suites' own ids, and elitea-web's. Measured
 * against `apps/elitea-web/e2e`, every `getByTestId` call selects on
 * elitea-web's OWN convention — lowercase kebab-case, semantic, no prefix
 * (`agent-name-input`, `pipeline-save-button`, `chat-with-agent-button`). So
 * there is nothing to migrate inside this repository: the other two namespaces
 * live in repositories this one does not own and cannot rewrite. What this
 * repository CAN do is refuse a new id that leaves the convention, which is
 * what this gate is. Cross-repo alignment stays a coordination task, not a
 * code change.
 *
 * THE RULE. Every test id this app AUTHORS matches
 * `^[a-z0-9]+(-[a-z0-9]+)*$` — lowercase letters, digits, single hyphens
 * between segments. No camelCase, no snake_case, no PascalCase, no dots, no
 * spaces, no leading or trailing hyphen. An interpolated segment
 * (`` `agent-skill-option-${id}` ``) counts as one opaque segment, because its
 * value is only known at run time; the STATIC text around it is still held to
 * the rule, so `` `Foo_${id}` `` is refused and `` `${testIdPrefix}-tab` `` is
 * not.
 *
 * WHAT IS SCANNED. Every `.ts`/`.tsx` under `src/`, tests and stories
 * included: a probe id in a test file is still an id this repository authors,
 * and the convention is the whole point of having one. Attribute names:
 * `data-testid`, `data-test-id`, `testId`, `testid`, `testID`, `testIdPrefix`
 * and `dataTestId` — the prop spellings this tree actually uses to reach a
 * `data-testid`.
 *
 * WHAT IS NOT AN AUTHORED ID, and why each exclusion is real rather than
 * convenient:
 *
 *   - a CSS/DOM SELECTOR (`[data-testid="KeyboardArrowDownIcon"]`,
 *     `` [`&:hover [data-testid="${X}"]`] ``) READS an id, and the ids read
 *     that way are MUI's own internal icon ids. This repository does not name
 *     them and cannot rename them. Detected by the `[` immediately before the
 *     attribute name, which is what makes it a selector rather than a JSX
 *     attribute.
 *   - a COMMENT quoting an id documents one; it does not create one. Comments
 *     are stripped before the scan.
 *   - a value this scanner cannot resolve statically (a call, a ternary, an
 *     imported constant) is COUNTED and reported as dynamic, never silently
 *     dropped — the count is printed on the pass path so a reader can see how
 *     much of the tree the rule actually reached. A same-file
 *     `const X = 'literal'` IS resolved, because that is how this tree names
 *     an id it uses twice.
 *
 * Usage:
 *   node scripts/check-testid-namespace.mjs [--root <dir>] [--json]
 */
import { readFileSync, readdirSync, statSync } from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { checkFloors, subjectPath } from './lib/gate-floor.mjs';

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const appRoot = path.resolve(scriptDir, '..');

const args = process.argv.slice(2);
const asJson = args.includes('--json');
const rootArg = args.indexOf('--root');
const scanRoot =
  rootArg >= 0 && args[rootArg + 1] ? path.resolve(process.cwd(), args[rootArg + 1]) : path.join(appRoot, 'src');

/** The one convention: lowercase kebab-case, no leading/trailing/doubled hyphen. */
const TESTID_PATTERN = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;

/** The prop spellings in this tree that end up as a `data-testid` in the DOM. */
const ATTRIBUTES = ['data-testid', 'data-test-id', 'dataTestId', 'testIdPrefix', 'testId', 'testID', 'testid'];

/**
 * Blank out comments so a quoted id in prose is not read as an authored one.
 *
 * Replaced with spaces of the same length rather than deleted, so every
 * reported line number still matches the file on disk.
 */
function stripComments(text) {
  const blank = (match) => match.replace(/[^\n]/g, ' ');
  return text
    .replace(/\/\*[\s\S]*?\*\//g, blank)
    // `[^:]` keeps `https://…` inside a string from swallowing the rest of its
    // line. A test id declared AFTER a `//` on the same line is not a case
    // this tree has, and reading one would be the safe direction anyway.
    .replace(/(^|[^:])(\/\/[^\n]*)/g, (whole, before, comment) => before + blank(comment));
}

/** Every `.ts`/`.tsx` file under `dir`, excluding declarations and node_modules. */
function sourceFiles(dir) {
  let stats;
  try {
    stats = statSync(dir);
  } catch {
    return [];
  }
  if (stats.isFile()) return /\.tsx?$/.test(dir) && !dir.endsWith('.d.ts') ? [dir] : [];
  const out = [];
  for (const entry of readdirSync(dir, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
    if (entry.name === 'node_modules' || entry.name.startsWith('.')) continue;
    out.push(...sourceFiles(path.join(dir, entry.name)));
  }
  return out;
}

/** Same-file `const NAME = 'literal'` bindings, the only identifier this resolves. */
function literalConstants(text) {
  const table = new Map();
  const pattern = /\bconst\s+([A-Za-z_$][\w$]*)\s*(?::[^=\n]+)?=\s*(['"`])((?:\\.|(?!\2)[^\\])*)\2/g;
  for (const match of text.matchAll(pattern)) table.set(match[1], match[3]);
  return table;
}

/**
 * The value(s) assigned to one attribute occurrence, from the text that
 * follows the `=`.
 *
 * A LIST, not one value, because of the shape this tree writes for a pair of
 * ids chosen at run time — `{isLastMessage ? 'skill-test-last-response' :
 * 'chat-answer-content'}`. Reading only the first branch would hold half of
 * that pair to the rule and let the other half through, which is the kind of
 * partial coverage a gate must not have.
 *
 * @returns {{kind: 'literal', values: string[]} | {kind: 'dynamic'}}
 */
function readAttributeValue(rest, constants) {
  const plain = /^\s*(['"])((?:\\.|(?!\1)[^\\])*)\1/.exec(rest);
  if (plain) return { kind: 'literal', values: [plain[2]] };
  const braced = /^\s*\{\s*/.exec(rest);
  if (!braced) return { kind: 'dynamic' };
  const inner = rest.slice(braced[0].length);
  const quoted = /^(['"`])((?:\\.|(?!\1)[^\\])*)\1\s*\}/.exec(inner);
  if (quoted) return { kind: 'literal', values: [quoted[2]] };
  const ternary = /^[^{}?]*\?\s*(['"`])((?:\\.|(?!\1)[^\\])*)\1\s*:\s*(['"`])((?:\\.|(?!\3)[^\\])*)\3\s*\}/.exec(inner);
  if (ternary) return { kind: 'literal', values: [ternary[2], ternary[4]] };
  const identifier = /^([A-Za-z_$][\w$]*)\s*\}/.exec(inner);
  if (identifier) {
    const bound = constants.get(identifier[1]);
    return bound === undefined ? { kind: 'dynamic' } : { kind: 'literal', values: [bound] };
  }
  return { kind: 'dynamic' };
}

/**
 * Does this id obey the convention?
 *
 * `${…}` collapses to one opaque segment first: its run-time value cannot be
 * checked, but the static text around it can, and that is where a stray
 * `Foo_` or a `.` shows up.
 */
function conforms(value) {
  return TESTID_PATTERN.test(value.replace(/\$\{[^}]*\}/g, 'x'));
}

/** Every authored test id in one file, with the line it sits on. */
function scanSource(text) {
  const stripped = stripComments(text);
  const constants = literalConstants(stripped);
  const found = [];
  const pattern = new RegExp(String.raw`(.?)\b(${ATTRIBUTES.join('|')})\s*=`, 'g');
  for (const match of stripped.matchAll(pattern)) {
    // `[data-testid="…"]` is a selector READING an id (MUI's own icon ids),
    // not this app authoring one.
    if (match[1] === '[') continue;
    const value = readAttributeValue(stripped.slice(match.index + match[0].length), constants);
    const line = stripped.slice(0, match.index).split('\n').length;
    found.push({ line, attribute: match[2], ...value });
  }
  return found;
}

const files = sourceFiles(scanRoot);
const violations = [];
let ids = 0;
let dynamic = 0;

for (const file of files) {
  let text;
  try {
    text = readFileSync(file, 'utf8');
  } catch {
    continue;
  }
  for (const found of scanSource(text)) {
    if (found.kind === 'dynamic') {
      dynamic += 1;
      continue;
    }
    for (const value of found.values) {
      ids += 1;
      if (conforms(value)) continue;
      violations.push({ file: subjectPath(appRoot, file), line: found.line, attribute: found.attribute, value });
    }
  }
}

// The floors are this gate's own "absence reads as correctness" guard: a
// renamed source directory, or a regex that stops matching, would otherwise
// report a clean tree over nothing at all.
const floor = checkFloors('check-testid-namespace', [
  { subject: `source files under ${subjectPath(appRoot, scanRoot)}`, observed: files.length, floor: 50 },
  { subject: 'statically resolved test ids in them', observed: ids, floor: 200 },
]);

if (asJson) {
  console.log(JSON.stringify({ ok: floor.ok && violations.length === 0, files: files.length, ids, dynamic, violations }, null, 2));
} else {
  for (const line of floor.lines) console.log(line);
  console.log(`check-testid-namespace: ${dynamic} id(s) computed at run time and not statically checkable.`);
  for (const violation of violations) {
    console.error(
      `TESTID  ${violation.file}:${violation.line}  ${violation.attribute}="${violation.value}" ` +
        'is not kebab-case. Test ids in this app are lowercase-with-hyphens (e.g. "pipeline-save-button").',
    );
  }
}

if (!floor.ok) {
  if (!asJson) console.error(floor.error);
  process.exit(2);
}
if (violations.length > 0) {
  if (!asJson) console.error(`check-testid-namespace: FAIL — ${violations.length} id(s) outside the namespace`);
  process.exit(1);
}
if (!asJson) console.log(`check-testid-namespace: OK — ${ids} test id(s) in one namespace`);
