import { propertyKeyName } from '../lib/ast.mjs';

/**
 * R-T13 (typography spec rev. 2 §4.2): MUI's STOCK typography variants are
 * aliases of the one type scale, never something a call site names.
 *
 * `theme.augment.d.ts` already makes a stock variant on Typography (and on
 * every component that takes `TypographyProps`) a tsc error. This rule is the
 * half tsc cannot see, so it needs no type information:
 *
 *  - `…typography.<stock>` member reads and spreads (`theme.typography.body2`,
 *    `...theme.typography.caption`) — the alias is MUI's internal plumbing;
 *    a style names the custom variant it means;
 *  - `variant="body1"` — `body1` stays declared for MUI's own use, so tsc
 *    accepts it; a call site writes `bodyMedium`;
 *  - a stock name passed as a string through an untyped prop or object
 *    (`textVariant="caption"`, `{ variant: 'body2' }`).
 */
const STOCK_VARIANTS = new Set([
  'body1',
  'body2',
  'caption',
  'h1',
  'h2',
  'h3',
  'h4',
  'h5',
  'h6',
  'subtitle1',
  'subtitle2',
  'overline',
  'button',
]);

/** `variant`, or any `…Variant` prop / key (`textVariant`, `titleVariant`). */
const isVariantKey = (name) => typeof name === 'string' && (name === 'variant' || name.endsWith('Variant'));

const message = (what) =>
  `R-T13: ${what} — MUI's stock typography variants are aliases; name one of the eight type-scale variants (shared/brand/typography.ts)`;

function stringValue(node) {
  if (!node) return null;
  if (node.type === 'Literal' && typeof node.value === 'string') return node.value;
  if (node.type === 'JSXExpressionContainer') return stringValue(node.expression);
  if (node.type === 'TemplateLiteral' && node.expressions.length === 0) {
    return node.quasis[0]?.value?.cooked ?? null;
  }
  return null;
}

export const noStockTypographyVariant = {
  meta: {
    type: 'problem',
    docs: {
      description: 'R-T13: no MUI stock typography variant (body1/2, caption, h1–h6, subtitle1/2, overline, button) at a call site',
    },
    schema: [],
  },
  create(context) {
    return {
      MemberExpression(node) {
        if (node.computed || node.property.type !== 'Identifier') return;
        if (!STOCK_VARIANTS.has(node.property.name)) return;
        const owner = node.object;
        const ownerName =
          owner.type === 'Identifier'
            ? owner.name
            : owner.type === 'MemberExpression' && !owner.computed && owner.property.type === 'Identifier'
              ? owner.property.name
              : null;
        if (ownerName !== 'typography') return;
        context.report({ node, message: message(`typography.${node.property.name}`) });
      },
      JSXAttribute(node) {
        const name = node.name.type === 'JSXIdentifier' ? node.name.name : null;
        if (!isVariantKey(name)) return;
        const value = stringValue(node.value);
        if (value !== null && STOCK_VARIANTS.has(value)) {
          context.report({ node, message: message(`${name}="${value}"`) });
        }
      },
      Property(node) {
        const key = propertyKeyName(node);
        if (!isVariantKey(key)) return;
        const value = stringValue(node.value);
        if (value !== null && STOCK_VARIANTS.has(value)) {
          context.report({ node: node.value, message: message(`${key}: '${value}'`) });
        }
      },
    };
  },
};
