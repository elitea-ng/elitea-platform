import { propertyKeyName } from '../lib/ast.mjs';

/**
 * R-T11 (spec §4.4; typography spec rev. 2 §4.1): a font size comes from the
 * type scale, and from nowhere else.
 *
 * A `fontSize:` property may hold ONLY:
 *  - the CSS keyword `'inherit'` (defers to the variant on the parent);
 *  - a member read of a CUSTOM variant's size,
 *    `<…>.typography.<variant>.fontSize` (or `typeScale(…).fontSize`).
 *
 * Every other value shape is reported, because each one is a way the old
 * app's 34 sizes (and this app's 10/12.8/13/15/18/24px drift) got in:
 *  - number and string literals — INCLUDING `var(--el-…)`, which names the
 *    `font` shorthand variable and is not a valid size at all;
 *  - template literals (`${n}rem`, `calc(…)`);
 *  - calls (`theme.typography.pxToRem(13)`, `calc` helpers);
 *  - identifiers, and conditionals / logical expressions with any operand
 *    that is not itself allowed;
 *  - member reads of MUI's STOCK variants (`theme.typography.body2.fontSize`
 *    — those are aliases, a call site names the custom variant) and of
 *    anything that is not a typography variant.
 *
 * Icons size through the icon's own `fontSize="small|medium|inherit"` prop
 * (a JSX attribute, not a style property, so not seen here) or `width` /
 * `height`. A `font-size:` inside a css template is reported unless it is
 * `inherit`.
 */
const FONT_SIZE_KEYS = new Set(['fontSize']);
const TEMPLATE_FONT_SIZE_RE = /font-size\s*:\s*(?!\s*inherit\b)[^;{}]*\d/i;
/** `font-size: ${…}` — the size is an interpolation, which is never a variant. */
const TEMPLATE_FONT_SIZE_INTERP_RE = /font-size\s*:\s*$/i;

/** The eight variants of the one type scale (`shared/brand/typography.ts`). */
const CUSTOM_VARIANTS = new Set([
  'headingLarge',
  'headingMedium',
  'headingSmall',
  'labelMedium',
  'bodyMedium',
  'labelSmall',
  'bodySmall',
  'subtitle',
]);

function unwrap(node) {
  let current = node;
  while (
    current &&
    (current.type === 'TSAsExpression' ||
      current.type === 'TSSatisfiesExpression' ||
      current.type === 'TSNonNullExpression' ||
      current.type === 'ParenthesizedExpression')
  ) {
    current = current.expression;
  }
  return current;
}

function memberName(node) {
  if (!node || node.type !== 'MemberExpression' || node.computed) return null;
  return node.property.type === 'Identifier' ? node.property.name : null;
}

function isTypeScaleCall(node) {
  const n = unwrap(node);
  return n?.type === 'CallExpression' && n.callee.type === 'Identifier' && n.callee.name === 'typeScale';
}

/**
 * `X.typography.<custom>.fontSize`, `typography.<custom>.fontSize`, or
 * `typeScale(…).fontSize`.
 */
function isAllowedMember(node) {
  if (memberName(node) !== 'fontSize') return false;
  const owner = unwrap(node.object);
  if (isTypeScaleCall(owner)) return true;
  const variant = memberName(owner);
  if (variant === null || !CUSTOM_VARIANTS.has(variant)) return false;
  const typography = unwrap(owner.object);
  if (typography?.type === 'Identifier') return typography.name === 'typography';
  return memberName(typography) === 'typography';
}

/** Returns the offending node, or null when the whole value is allowed. */
function offending(value) {
  const node = unwrap(value);
  if (!node) return null;
  switch (node.type) {
    case 'Literal':
      return node.value === 'inherit' ? null : node;
    case 'MemberExpression':
      return isAllowedMember(node) ? null : node;
    case 'ConditionalExpression':
      return offending(node.consequent) ?? offending(node.alternate);
    case 'LogicalExpression':
      return offending(node.left) ?? offending(node.right);
    case 'ArrowFunctionExpression':
      // sx's per-property callback: `fontSize: (theme) => theme.typography.…`
      return node.body.type === 'BlockStatement' ? node : offending(node.body);
    default:
      // TemplateLiteral, CallExpression, Identifier, BinaryExpression, …
      return node;
  }
}

function describe(node) {
  switch (node.type) {
    case 'Literal':
      return JSON.stringify(node.value);
    case 'TemplateLiteral':
      return 'a template literal';
    case 'CallExpression':
      return 'a function call';
    case 'Identifier':
      return `the identifier \`${node.name}\``;
    case 'MemberExpression':
      return 'a member read that is not a custom typography variant';
    default:
      return `a ${node.type}`;
  }
}

export const adHocFontSize = {
  meta: {
    type: 'problem',
    docs: {
      description:
        'R-T11: ad-hoc fontSize is banned — use a typography variant (Typography variant= / typeScale(theme.typography.*) / theme.typography.<variant>.fontSize)',
    },
    schema: [],
  },
  create(context) {
    return {
      Property(node) {
        const key = propertyKeyName(node);
        if (!key || !FONT_SIZE_KEYS.has(key)) return;
        const bad = offending(node.value);
        if (bad) {
          context.report({
            node: bad,
            message: `R-T11: fontSize: ${describe(bad)} — sizes come from the type scale (a Typography variant, typeScale(theme.typography.<variant>) or theme.typography.<variant>.fontSize)`,
          });
        }
      },
      TemplateElement(node) {
        const raw = node.value && (node.value.cooked ?? node.value.raw);
        if (
          typeof raw === 'string' &&
          (TEMPLATE_FONT_SIZE_RE.test(raw) || (!node.tail && TEMPLATE_FONT_SIZE_INTERP_RE.test(raw)))
        ) {
          context.report({
            node,
            message:
              'R-T11: ad-hoc font-size inside a css template — sizes come from typography variants',
          });
        }
      },
    };
  },
};
