import { isStringLiteral, propertyKeyName } from '../lib/ast.mjs';
import { REM_PX, nearestSteps, onScale, scaleFor, toUnits } from '../lib/spacing-scale.mjs';

/**
 * R-T9 (spec §4.4): spacing uses `theme.spacing(n)` with integer n (or plain
 * numbers in `sx`, which MUI multiplies by the spacing unit). Raw px strings
 * in margin/padding/gap are a lint error — the old app has 301 distinct
 * spacing string values.
 *
 * Covers both object styles (sx / styled objects) and CSS template literals
 * (styled`margin: 12px`).
 *
 * With options (ADR-0062 decision 3.4; used only by the style ratchet,
 * scripts/style-ratchet.oxlintrc.json) the rule also reports, each message
 * tagged with its ratchet kind:
 *   [spacing-rem]       rem lengths in spacing (`units` holds "rem");
 *   [spacing-px]        px lengths, and bare numbers where MUI does NOT
 *                       multiply (a `styled()` object, a `style` prop);
 *   [spacing-off-scale] numbers MUI multiplies (`sx`, a JSX spacing prop,
 *                       `theme.spacing()` in a spacing value) that are not on
 *                       the brand scale (`unit` set).
 * Options: `{ "units": ["px", "rem"], "unit": 8 }`. Without options the rule
 * reports exactly what it reported before, so the main lint does not change.
 */
const SPACING_KEYS = new Set([
  'm', 'mt', 'mb', 'ml', 'mr', 'mx', 'my',
  'p', 'pt', 'pb', 'pl', 'pr', 'px', 'py',
  'margin', 'marginTop', 'marginBottom', 'marginLeft', 'marginRight',
  'marginInline', 'marginInlineStart', 'marginInlineEnd',
  'marginBlock', 'marginBlockStart', 'marginBlockEnd',
  'padding', 'paddingTop', 'paddingBottom', 'paddingLeft', 'paddingRight',
  'paddingInline', 'paddingInlineStart', 'paddingInlineEnd',
  'paddingBlock', 'paddingBlockStart', 'paddingBlockEnd',
  'gap', 'rowGap', 'columnGap',
]);

const PX_VALUE_RE = /\d+(?:\.\d+)?px/;
const TEMPLATE_SPACING_PX_RE =
  /(?:^|[;{\s])(?:margin|padding|gap|row-gap|column-gap)[a-z-]*\s*:[^;{}]*\d+(?:\.\d+)?px/i;

// ── ratchet mode (options set) ──────────────────────────────────────────────

/** MUI system aliases the legacy key set misses. */
const RATCHET_KEYS = new Set([...SPACING_KEYS, 'marginX', 'marginY', 'paddingX', 'paddingY']);
/** JSX props MUI multiplies by the unit (system props, Stack/Grid spacing). */
const JSX_SPACING_PROPS = new Set([...RATCHET_KEYS, 'spacing', 'rowSpacing', 'columnSpacing']);
/** A length in a CSS value: `1rem`, `-0.5rem`, `12px`; not `--x-12px`, not `w12px`. */
const LENGTH_RE = /(?:^|[\s(,+*/])(-?(?:\d+\.?\d*|\.\d+))(rem|px)\b/g;
const TEMPLATE_SPACING_RE =
  /(?:^|[;{\s])(?:margin|padding|gap|row-gap|column-gap)[a-z-]*\s*:([^;{}]*)/gi;
/** Type names that mark an object as an `sx` value. */
const SX_TYPES = new Set(['SxProps', 'SystemStyleObject', 'SxProp']);

const fmt = (n) => String(n);

/** True when a TS type subtree names one of SX_TYPES anywhere. */
function namesSxType(node) {
  if (!node || typeof node !== 'object') return false;
  if (node.type === 'TSTypeReference') {
    const name = node.typeName?.type === 'TSQualifiedName' ? node.typeName.right?.name : node.typeName?.name;
    if (SX_TYPES.has(name)) return true;
  }
  for (const key of Object.keys(node)) {
    if (key === 'parent' || key === 'loc' || key === 'range') continue;
    const child = node[key];
    if (Array.isArray(child)) {
      if (child.some(namesSxType)) return true;
    } else if (child && typeof child === 'object' && typeof child.type === 'string' && namesSxType(child)) {
      return true;
    }
  }
  return false;
}

/** `styled`, `styled.div`, `styled(X)` — the callee of a styled() style call. */
function isStyledCallee(callee) {
  if (!callee) return false;
  if (callee.type === 'Identifier') return callee.name === 'styled';
  if (callee.type === 'MemberExpression') return callee.object?.type === 'Identifier' && callee.object.name === 'styled';
  if (callee.type === 'CallExpression') return isStyledCallee(callee.callee);
  return false;
}

/** `x.spacing(...)` — theme.spacing or a callback's `t.spacing`. */
function isSpacingCall(node) {
  return (
    node.type === 'CallExpression' &&
    node.callee?.type === 'MemberExpression' &&
    !node.callee.computed &&
    node.callee.property?.name === 'spacing'
  );
}

/** Root identifier of a JSX element name: `Foo` for `<Foo>` and `<Foo.Bar>`. */
function jsxRootName(name) {
  let node = name;
  while (node && node.type === 'JSXMemberExpression') node = node.object;
  return node && node.type === 'JSXIdentifier' ? node.name : null;
}

function ratchetVisitors(context, options) {
  const units = new Set(options.units ?? ['px']);
  const unit = options.unit;
  const scale = unit ? scaleFor(unit) : null;
  /** Local names imported from a package that is not MUI (`@xyflow/react`'s `Background`). */
  const thirdParty = new Set();
  /** Stack of style contexts: 'sx' (MUI multiplies a number), 'px' (a number is px). */
  const stack = [];
  const ctx = () => stack[stack.length - 1]?.kind ?? null;
  const push = (node, kind) => stack.push({ node, kind });
  const pop = (node) => {
    if (stack.length && stack[stack.length - 1].node === node) stack.pop();
  };

  const stepAdvice = (units_) => {
    if (onScale(units_, scale ?? [])) return `→ ${fmt(units_)} (theme.spacing(${fmt(units_)}) or the sx number ${fmt(units_)})`;
    const near = scale ? nearestSteps(units_, scale) : [];
    return `is ${fmt(units_)} units, off the scale → nearest steps ${near.map(fmt).join(' or ')} (a visible change, ADR-0062 decision 5)`;
  };

  function reportLength(node, key, raw, value, lengthUnit) {
    if (!units.has(lengthUnit)) return;
    const px = lengthUnit === 'rem' ? value * REM_PX : value;
    const advice = unit ? stepAdvice(toUnits(px, unit)) : '→ theme.spacing(n) or a unitless sx number';
    context.report({
      node,
      message: `R-T9 [spacing-${lengthUnit}] ${key}: '${raw}' ${advice}`,
    });
  }

  function scanString(node, key, text) {
    for (const match of text.matchAll(LENGTH_RE)) {
      reportLength(node, key, `${match[1]}${match[2]}`, Number(match[1]), match[2]);
    }
  }

  function reportNumber(node, key, n, kind) {
    if (kind === 'sx' && scale && !onScale(n, scale)) {
      const near = nearestSteps(n, scale);
      context.report({
        node,
        message: `R-T9 [spacing-off-scale] ${key}: ${fmt(n)} is off the scale → nearest steps ${near.map(fmt).join(' or ')} (a visible change, ADR-0062 decision 5)`,
      });
    } else if (kind === 'px' && n !== 0 && units.has('px')) {
      const advice = unit ? stepAdvice(toUnits(n, unit)) : '→ theme.spacing(n)';
      context.report({
        node,
        message: `R-T9 [spacing-px] ${key}: ${fmt(n)} is px here (a styled() object or a style prop) ${advice}`,
      });
    }
  }

  /** Every value shape a spacing key can hold. `kind` is the number context. */
  function analyze(node, key, kind) {
    if (!node) return;
    switch (node.type) {
      case 'Literal':
        if (typeof node.value === 'string') scanString(node, key, node.value);
        else if (typeof node.value === 'number') reportNumber(node, key, node.value, kind);
        return;
      case 'UnaryExpression':
        if (node.operator !== '-') return;
        if (node.argument?.type === 'Literal' && typeof node.argument.value === 'number') {
          reportNumber(node, key, -node.argument.value, kind);
        } else {
          analyze(node.argument, key, kind);
        }
        return;
      case 'TemplateLiteral':
        for (const quasi of node.quasis) scanString(quasi, key, quasi.value.cooked ?? quasi.value.raw);
        // A number inside `${}` becomes text: only theme.spacing() calls matter there.
        for (const expression of node.expressions) analyze(expression, key, null);
        return;
      case 'ConditionalExpression':
        analyze(node.consequent, key, kind);
        analyze(node.alternate, key, kind);
        return;
      case 'LogicalExpression':
        analyze(node.left, key, kind);
        analyze(node.right, key, kind);
        return;
      case 'ObjectExpression': // responsive: { xs: 1, md: 2 }
        for (const property of node.properties) {
          if (property.type === 'Property') analyze(property.value, key, kind);
        }
        return;
      case 'ArrayExpression': // responsive: [1, 2]
        for (const element of node.elements) analyze(element, key, kind);
        return;
      case 'CallExpression':
        if (isSpacingCall(node)) {
          for (const argument of node.arguments) analyze(argument, `${key} → spacing()`, 'sx');
        }
        return;
      case 'ArrowFunctionExpression': // sx callback value: gap: (theme) => theme.spacing(0.25)
        if (node.body?.type !== 'BlockStatement') analyze(node.body, key, kind);
        else for (const statement of node.body.body) if (statement.type === 'ReturnStatement') analyze(statement.argument, key, kind);
        return;
      case 'TSAsExpression':
      case 'TSSatisfiesExpression':
      case 'TSNonNullExpression':
      case 'ParenthesizedExpression':
        analyze(node.expression, key, kind);
        return;
      default:
    }
  }

  return {
    ImportDeclaration(node) {
      const source = node.source?.value;
      if (typeof source !== 'string' || source.startsWith('.') || source.startsWith('@/') || source.startsWith('@mui/')) return;
      for (const specifier of node.specifiers ?? []) thirdParty.add(specifier.local.name);
    },
    JSXAttribute(node) {
      const name = node.name?.type === 'JSXIdentifier' ? node.name.name : null;
      if (name === 'sx') push(node, 'sx');
      else if (name === 'style') push(node, 'px');
      if (!name || !JSX_SPACING_PROPS.has(name)) return;
      const element = node.parent;
      const root = element?.type === 'JSXOpeningElement' ? jsxRootName(element.name) : null;
      if (root && thirdParty.has(root)) return; // a chart or canvas part: its own px props
      const value = node.value?.type === 'JSXExpressionContainer' ? node.value.expression : node.value;
      analyze(value, name, 'sx');
    },
    'JSXAttribute:exit': pop,
    Property(node) {
      const key = propertyKeyName(node);
      if (key === 'sx') push(node, 'sx');
      if (!key || !RATCHET_KEYS.has(key)) return;
      analyze(node.value, key, ctx());
    },
    'Property:exit': pop,
    VariableDeclarator(node) {
      if (namesSxType(node.id?.typeAnnotation)) push(node, 'sx');
    },
    'VariableDeclarator:exit': pop,
    TSSatisfiesExpression(node) {
      if (namesSxType(node.typeAnnotation)) push(node, 'sx');
    },
    'TSSatisfiesExpression:exit': pop,
    TSAsExpression(node) {
      if (namesSxType(node.typeAnnotation)) push(node, 'sx');
    },
    'TSAsExpression:exit': pop,
    ArrowFunctionExpression(node) {
      if (namesSxType(node.returnType)) push(node, 'sx');
    },
    'ArrowFunctionExpression:exit': pop,
    FunctionDeclaration(node) {
      if (namesSxType(node.returnType)) push(node, 'sx');
    },
    'FunctionDeclaration:exit': pop,
    CallExpression(node) {
      if (isStyledCallee(node.callee)) push(node, 'px');
    },
    'CallExpression:exit': pop,
    TemplateElement(node) {
      const raw = node.value && (node.value.cooked ?? node.value.raw);
      if (typeof raw !== 'string') return;
      for (const declaration of raw.matchAll(TEMPLATE_SPACING_RE)) {
        scanString(node, 'css template', declaration[1]);
      }
    },
  };
}

export const rawPxSpacing = {
  meta: {
    type: 'problem',
    docs: {
      description:
        'R-T9: raw px in margin/padding/gap is banned — use theme.spacing(n) or unitless sx numbers',
    },
    schema: [
      {
        type: 'object',
        properties: {
          units: { type: 'array', items: { enum: ['px', 'rem'] } },
          unit: { type: 'number' },
        },
        additionalProperties: false,
      },
    ],
  },
  create(context) {
    const options = context.options?.[0];
    if (options) return ratchetVisitors(context, options);
    return {
      Property(node) {
        const key = propertyKeyName(node);
        if (!key || !SPACING_KEYS.has(key)) return;
        if (isStringLiteral(node.value) && PX_VALUE_RE.test(node.value.value)) {
          context.report({
            node: node.value,
            message: `R-T9: ${key}: ${JSON.stringify(node.value.value)} — spacing is theme.spacing(n) or a unitless sx number, never raw px`,
          });
        }
      },
      TemplateElement(node) {
        const raw = node.value && (node.value.cooked ?? node.value.raw);
        if (typeof raw === 'string' && TEMPLATE_SPACING_PX_RE.test(raw)) {
          context.report({
            node,
            message:
              'R-T9: raw px spacing inside a css template — spacing is theme.spacing(n), never raw px',
          });
        }
      },
    };
  },
};
