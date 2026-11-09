/**
 * The product-name literal scan (ADR-0052 W3.3): no user-visible string in
 * the web client's source names the product itself. Messages use
 * `{{product}}` (en.json); code reads `productName()` / `productFullName()`
 * (`src/shared/brand/productName.ts`), so a brand or a served pack renames
 * every screen.
 *
 * "A string" is a string literal, a template literal's text, or JSX text, as
 * @babel/parser sees them. Comments, identifiers and import paths are not
 * strings; wire names (`X-Elitea-…` headers, `ELITEA_…` variables) are
 * excluded by the pattern (the name preceded by `-`, `_`, a letter, a
 * digit, `.`, `/` or `@`, or followed by one of those or by a `.` that
 * starts a domain: "elitea.ai" is a wire name, "…in Elitea." is copy).
 */
import { parse } from '@babel/parser';

import { walk } from './budgets-core.mjs';

/** The product name as a word on its own, not part of a wire name. */
const PRODUCT_LITERAL_RE = /(?<![-\w/.@])(?:Elitea|ELITEA|EliteA)(?![-\w/@]|\.\w)/;

/**
 * Files allowed to name the product, each with why. Paths are relative to
 * the app, posix.
 */
export const PRODUCT_LITERAL_ALLOWED = new Map([
  // Data, not copy: the marketplace categories the server stores.
  ['src/features/agent-lifecycle/lib/publishValidation.ts', 'a stored category value'],
  // The bug report goes to ELITEA's own public repository, which builds every brand.
  ['src/widgets/account-menu/lib/accountLinks.ts', 'names the upstream product in an upstream bug report'],
  // Development-only fake hosts.
  ['src/entries/desktop/devHarness.tsx', 'development harness'],
  ['src/shared/desktop/doctorIpc.fake.ts', 'fake host for tests and the harness'],
  ['src/shared/desktop/computerIpc.fake.ts', 'fake host for tests and the harness'],
  // The vendored support widget keeps its upstream defaults; the app passes the title.
  ['src/widgets/support-assistant/vendor/EliteaAssistant.tsx', 'vendored; the caller passes the product name'],
]);

/** Every `{ line, text }` in `source` whose string names the product. */
export function productLiterals(source, { jsx = false } = {}) {
  const ast = parse(source, { sourceType: 'module', plugins: jsx ? ['typescript', 'jsx'] : ['typescript'] });
  const found = [];
  walk(ast.program, (node) => {
    let text;
    if (node.type === 'StringLiteral') text = node.value;
    else if (node.type === 'TemplateElement') text = node.value.cooked ?? node.value.raw;
    else if (node.type === 'JSXText') text = node.value;
    if (text !== undefined && PRODUCT_LITERAL_RE.test(text))
      found.push({ line: node.loc.start.line, text: text.replace(/\s+/g, ' ').trim() });
  });
  return found;
}
