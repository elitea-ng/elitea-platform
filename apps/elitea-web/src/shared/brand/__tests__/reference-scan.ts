import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';

import { parse } from '@babel/parser';

import { CSS_VAR_PREFIX } from '../constants';
import { formatHex, parseColor } from '../color';

/**
 * Machinery for §4.6 check 7 assertions (b) and (c). Kept out of the test
 * file so the assertions read as assertions.
 *
 * Nothing here knows anything about the default pack: the reference set is
 * whatever the source tree actually asks for, so it widens by itself as
 * units S1/W-shell/A* author components.
 */

const SOURCE_RE = /\.(ts|tsx)$/;
const SKIP_DIRS = new Set(['node_modules', 'dist', 'coverage', '.git']);

export interface TokenReference {
  /** Source file, relative to the scan root. */
  file: string;
  line: number;
  /** Dotted token path, e.g. `background.button.primary.default`. */
  token: string;
  /** The CSS variable the token must resolve to. */
  cssVar: string;
}

function* sourceFiles(dir: string): Generator<string> {
  // `withFileTypes` answers directory-or-file from the directory entry the
  // read already returned, instead of a `stat` syscall per entry. A symlink
  // reports neither, so those alone still pay for a `stat` — which is what
  // the old code did for all ~4300 entries.
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (SKIP_DIRS.has(entry.name)) continue;
    const full = join(dir, entry.name);
    const isDirectory = entry.isSymbolicLink()
      ? statSync(full).isDirectory()
      : entry.isDirectory();
    if (isDirectory) yield* sourceFiles(full);
    else if (SOURCE_RE.test(entry.name)) yield full;
  }
}

/**
 * Whether a file's RAW TEXT leaves it able to contribute anything at all.
 *
 * Both halves of the scan need the seven characters `palette` to be present
 * in the source: the AST half only records chains under
 * `theme.vars.palette.`, and the literal half only matches
 * `--el-palette-…`. The same is true of the hard failure in
 * `classifyMember` — an aliased or computed `theme.vars.palette` read also
 * spells `palette`. So a file without it can be skipped BEFORE the Babel
 * parse, which is the expensive step, without changing a single result.
 *
 * The escape clause is what keeps that argument airtight rather than
 * merely true in practice. JavaScript lets both identifiers and string
 * literals spell those characters indirectly — `theme.vars.\u0070alette.x`
 * is a legal member chain, and `'--el-\x70alette-token'` cooks to a real
 * custom property name — and in either case the raw text no longer
 * contains `palette`. Rather than reason about which escapes can reach
 * which position, any file containing a `\u` or `\x` escape at all is
 * parsed. It is a handful of files, and it means the filter is sound by
 * construction: skipping requires PROVING the file cannot contribute.
 */
const CANDIDATE_RE = /palette|--el-|\\[ux]/;

/** Flatten a static member chain to a dotted path; null if anything is dynamic. */
function staticPath(node: Record<string, unknown>): string | null {
  if (node['type'] === 'Identifier') return node['name'] as string;
  if (node['type'] !== 'MemberExpression') return null;
  const object = staticPath(node['object'] as Record<string, unknown>);
  if (object === null) return null;
  const property = node['property'] as Record<string, unknown>;
  if (node['computed'] === true || property['type'] !== 'Identifier') return null;
  return `${object}.${property['name'] as string}`;
}

const PREFIX = 'theme.vars.palette.';

const AST_META_KEYS = new Set(['loc', 'leadingComments', 'trailingComments', 'extra']);

/** Depth-first AST walk; `onNode` returning true prunes that subtree. */
function walkAst(node: unknown, onNode: (node: Record<string, unknown>) => boolean): void {
  if (node === null || typeof node !== 'object') return;
  if (Array.isArray(node)) {
    for (const item of node) walkAst(item, onNode);
    return;
  }
  const record = node as Record<string, unknown>;
  if (typeof record['type'] === 'string' && onNode(record)) return;
  for (const [key, value] of Object.entries(record)) {
    if (!AST_META_KEYS.has(key)) walkAst(value, onNode);
  }
}

const lineOf = (node: Record<string, unknown>): number =>
  (node['loc'] as { start: { line: number } } | undefined)?.start.line ?? 0;

/**
 * Classify one MemberExpression: a token reference, an unscannable read, or
 * neither. Split out of the walk so each half stays inside the §3.5
 * complexity budget.
 */
function classifyMember(node: Record<string, unknown>, file: string): TokenReference | null {
  const path = staticPath(node);
  if (path !== null && path.startsWith(PREFIX)) {
    const token = path.slice(PREFIX.length);
    return {
      file,
      line: lineOf(node),
      token,
      cssVar: `--${CSS_VAR_PREFIX}-palette-${token.split('.').join('-')}`,
    };
  }
  if (path === PREFIX.slice(0, -1)) {
    // Reached only when the node is NOT inside a longer static chain, i.e. a
    // computed access or an alias. Either hides tokens from this scan, so the
    // gate would silently stop protecting them.
    throw new Error(
      `${file}:${lineOf(node)} — theme.vars.palette must be read through a static dotted ` +
        'path (no computed access, no aliasing); the reference scan behind §4.6 check 7 ' +
        'cannot see anything else',
    );
  }
  return null;
}

/**
 * MEDIUM-1 (adversarial verification, 2026-07-27): the MemberExpression scan
 * above only sees `theme.vars.palette.x.y` reads. A hard-coded string —
 * `'var(--el-palette-nonexistent-token)'`, plain or as a template literal —
 * never goes through a MemberExpression at all, so it sailed past assertion
 * (b) undetected: a typo'd or nonexistent token referenced that way shipped
 * unverified. This regex catches any `--el-palette-*` custom property name
 * embedded in a string or template-element's literal text, complementing the
 * `theme.vars.palette.*` AST scan. `emittedCssVars` walks the whole
 * stylesheet, so checking membership works for all palette tokens.
 *
 * Deliberately NOT covered (and this is a real, accepted limit, not an
 * oversight): text split across a template literal's INTERPOLATED holes —
 * `` `var(--el-palette-${path})` `` — because the reference literally does
 * not exist in source until runtime. `typography.ts`'s `paletteVar` helper
 * is exactly that case; it is validated by `buildTheme.test.ts` asserting
 * the CONCRETE output strings it produces, which are ordinary string
 * literals in the test file and so ARE caught by this same regex there.
 */
const CSS_VAR_LITERAL_RE = /--el-palette-[A-Za-z][A-Za-z0-9-]*/g;

/**
 * Test/story files are excluded from the LITERAL scan only — component
 * MemberExpression reads are still scanned everywhere, including tests.
 * Test files legitimately contain `--el-…` text that is not a component
 * referencing a token: this very file's test-description strings quote
 * `--el-palette-*` verbatim, and `buildTheme.test.ts` checks
 * `!value.startsWith('var(--el-palette-')` — a deliberately incomplete
 * prefix, not a reference. Both are indistinguishable from a real reference
 * by regex alone, so the boundary is drawn at the file, matching the
 * exemption pattern the rest of the tree already uses for tests (§3.5,
 * `.oxlintrc.json`'s `src/**\/*.test.*` overrides). Real per-token literal
 * assertions inside test files (e.g. `typography.test.ts` expecting
 * `'var(--el-palette-text-secondary)'`) stay validated — via the
 * `emittedCssVars`/`resolveScheme` machinery those tests already drive, not
 * via this source-text scan.
 */
const TEST_FILE_RE = /\.(test|spec|stories)\.[tj]sx?$/;

/** The literal text of a string Literal or a template quasi; null otherwise. */
function literalText(node: Record<string, unknown>): string | null {
  if (node['type'] === 'StringLiteral') {
    const value = node['value'];
    return typeof value === 'string' ? value : null;
  }
  if (node['type'] === 'TemplateElement') {
    const value = node['value'] as { raw?: string; cooked?: string | null } | undefined;
    return (value?.cooked ?? value?.raw) ?? null;
  }
  return null;
}

/** Record one `TokenReference` per `--el-*` custom property named in `text`. */
function scanLiteralForCssVars(
  text: string,
  file: string,
  line: number,
  found: TokenReference[],
): void {
  for (const match of text.matchAll(CSS_VAR_LITERAL_RE)) {
    const cssVar = match[0];
    found.push({ file, line, token: cssVar.slice('--el-'.length).split('-').join('.'), cssVar });
  }
}

/**
 * Every `theme.vars.palette.<path>` read in the tree (R-T7's authoring
 * surface), PLUS every `--el-*` custom property named in a string or
 * template literal (see `CSS_VAR_LITERAL_RE` below). A dynamic
 * `theme.vars.palette[…]` segment is a hard failure rather than a silent
 * gap: a reference the scan cannot see is a reference the gate cannot
 * protect.
 */
export function scanThemeVarReferences(root: string): TokenReference[] {
  const found: TokenReference[] = [];
  for (const file of sourceFiles(root)) {
    const rel = relative(root, file).split('\\').join('/');
    const scanLiterals = !TEST_FILE_RE.test(rel);
    const source = readFileSync(file, 'utf8');
    // The parse is ~90% of this scan's cost and ~90% of the tree cannot
    // contribute to it; see CANDIDATE_RE for why skipping is sound.
    if (!CANDIDATE_RE.test(source)) continue;
    const ast = parse(source, {
      sourceType: 'module',
      plugins: file.endsWith('.tsx') ? ['typescript', 'jsx'] : ['typescript'],
    });
    walkAst(ast.program, (node) => {
      if (node['type'] === 'MemberExpression') {
        const reference = classifyMember(node, rel);
        if (reference === null) return false;
        found.push(reference);
        return true; // outermost chain only — do not re-record its prefixes
      }
      if (scanLiterals) {
        const text = literalText(node);
        if (text !== null) {
          scanLiteralForCssVars(text, rel, lineOf(node), found);
          return true; // leaf node either way — nothing further to walk into
        }
      }
      return false;
    });
  }
  return found;
}

/** Every CSS custom property a vars-theme emits, across all its stylesheets. */
export function emittedCssVars(theme: {
  generateStyleSheets: () => Array<Record<string, unknown>>;
}): Set<string> {
  const vars = new Set<string>();
  const walk = (node: unknown): void => {
    if (node === null || typeof node !== 'object') return;
    for (const [key, value] of Object.entries(node as Record<string, unknown>)) {
      if (key.startsWith('--')) vars.add(key);
      else walk(value);
    }
  };
  theme.generateStyleSheets().forEach(walk);
  return vars;
}

const COLOR_IN_TEXT_RE = /#[0-9a-f]{3,8}\b|\b(?:rgba?|hsla?)\([^)]*\)/gi;

/**
 * Every colour in a blob of CSS/inline style text, canonicalised to
 * `#rrggbb[aa]` so notation differences cannot hide a match.
 */
export function colorsIn(text: string): Set<string> {
  const out = new Set<string>();
  for (const match of text.match(COLOR_IN_TEXT_RE) ?? []) {
    const parsed = parseColor(match);
    if (parsed) out.add(formatHex(parsed).toLowerCase());
  }
  return out;
}

/** Same, over a whole vars-theme's generated stylesheets. */
export function colorsInTheme(theme: {
  generateStyleSheets: () => Array<Record<string, unknown>>;
}): Set<string> {
  const out = new Set<string>();
  const walk = (node: unknown): void => {
    if (typeof node === 'string') {
      colorsIn(node).forEach((c) => out.add(c));
      return;
    }
    if (node === null || typeof node !== 'object') return;
    Object.values(node as Record<string, unknown>).forEach(walk);
  };
  theme.generateStyleSheets().forEach(walk);
  return out;
}

/** Colour-bearing declarations worth sampling from a computed style. */
const SAMPLED_PROPERTIES = [
  'color',
  'background-color',
  'background-image',
  'border-top-color',
  'border-right-color',
  'border-bottom-color',
  'border-left-color',
  'outline-color',
  'fill',
  'stroke',
  'box-shadow',
] as const;

export interface RenderedSample {
  /** Number of elements actually sampled. */
  size: number;
  /** Union of every colour the sample and the document's stylesheets carry. */
  colors: Set<string>;
}

/**
 * Sample the rendered document: up to `cap` elements' computed and inline
 * styles, PLUS every injected `<style>` element.
 *
 * The stylesheet half is load-bearing. jsdom does not resolve `var()` in
 * `getComputedStyle`, so a component whose colour comes from a CSS variable
 * reports the literal `var(--el-…)` string — sampling computed styles alone
 * would assert nothing. The `<style>` scan is where MUI's variable
 * definitions (and every emotion rule generated for the sample) actually
 * live, so the two together cover the whole painted surface.
 */
export function sampleRenderedColors(doc: Document, cap = 200): RenderedSample {
  const elements = [...doc.querySelectorAll('*')].slice(0, cap);
  const colors = new Set<string>();
  const add = (text: string): void => {
    colorsIn(text).forEach((c) => colors.add(c));
  };
  for (const element of elements) {
    const computed = doc.defaultView?.getComputedStyle(element);
    if (computed) {
      for (const property of SAMPLED_PROPERTIES) add(computed.getPropertyValue(property));
    }
    add(element.getAttribute('style') ?? '');
  }
  for (const style of doc.querySelectorAll('style')) add(style.textContent ?? '');
  return { size: elements.length, colors };
}
