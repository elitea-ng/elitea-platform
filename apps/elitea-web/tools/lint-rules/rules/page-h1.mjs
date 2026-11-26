/**
 * A page title written by hand (ADR-0062 decisions 3.7 and 4, the page frame
 * and header family), for theme-gate check 10 (the style ratchet) ONLY. A
 * page's `h1` comes from `PageHeader` in `shared/ui/page-frame`; existing
 * hand-written ones are allowed, a PR may not add more in a folder than it
 * removes (scripts/lib/style-ratchet-core.mjs).
 *
 * A header copy is `Box` and `Typography`, with no MUI import of its own for a
 * rule to see; its `h1` is the part a rule can see.
 *
 * Counts:
 *   component="h1" on any element (`<Typography variant="headingLarge" component="h1">`)
 *   a native <h1> element
 * shared/ui is exempt in scripts/style-ratchet.oxlintrc.json.
 */
const MESSAGE = '[page-h1] a page title comes from PageHeader (shared/ui/page-frame); if it is not enough, extend PageHeader';

function isH1String(value) {
  if (!value) return false;
  if (value.type === 'Literal') return value.value === 'h1';
  if (value.type === 'JSXExpressionContainer') return isH1String(value.expression);
  return false;
}

export const pageH1 = {
  meta: {
    type: 'suggestion',
    docs: { description: 'A page h1 is written by PageHeader in shared/ui, not by hand' },
    schema: [],
  },
  create(context) {
    return {
      JSXAttribute(node) {
        if (node.name.type === 'JSXIdentifier' && node.name.name === 'component' && isH1String(node.value)) {
          context.report({ node, message: MESSAGE });
        }
      },
      JSXOpeningElement(node) {
        if (node.name.type === 'JSXIdentifier' && node.name.name === 'h1') context.report({ node, message: MESSAGE });
      },
    };
  },
};
