/**
 * R-T15 (ADR-0062 decision 3.4, third bullet): reads of a generic legacy
 * palette id, for theme-gate check 10 (the style ratchet) ONLY. It is not in
 * .oxlintrc.json: existing reads are allowed, a PR may not add more in a
 * folder than it removes (scripts/lib/style-ratchet-core.mjs).
 *
 * The ids are those of `src/shared/brand/tokens/legacy-roles.json`, the map
 * `{ "<role>": ["<legacy id>"] }` that PR 3 wrote (decision 1.4): for each
 * role, the one generic id whose meaning is the role and whose colour is that
 * role's in both schemes (src/shared/brand/__tests__/roles.test.ts holds the
 * rule). This rule only reads the map. `resolveScheme` honours the same ids,
 * so every read counted here is one the role keeps a hand-tuned colour for.
 * An id off the map is not counted. A component id stays a tuning point of
 * its own component (ADR-0024). `text.default` and `focus.ring` are off the
 * map too (src/shared/brand/tokens/README.md, Roles): new code reads
 * `role.textMuted` for muted text and review holds that, and shared code reads
 * `focus.ring` for the focus ring.
 *
 * The map is read from THIS checkout, next to the rule, so theme-gate counts
 * the base tree with the head's map (decision 3.2): a PR that adds an id to
 * the map does not fail on reads it did not write.
 *
 * Counts:
 *   <any>.vars.palette.<id>          (theme.vars, t.vars in an sx callback, vars from a destructure)
 *   var(--el-palette-<id with dashes>) in a string or a template
 *   color / bgcolor / borderColor / backgroundColor: '<id>' in an object
 *     (an sx object; outside sx MUI does not resolve the string, so it is a
 *     defect either way) and as a JSX prop (`<Typography color="text.secondary">`),
 *     also in either branch of a conditional.
 */
import { readFileSync } from 'node:fs';

import { propertyKeyName, stringVisitors } from '../lib/ast.mjs';

const MAP_URL = new URL('../../../src/shared/brand/tokens/legacy-roles.json', import.meta.url);

/** legacy id → role name, inverted from the map. Read once per oxlint process. */
const LEGACY = new Map(
  Object.entries(JSON.parse(readFileSync(MAP_URL, 'utf8'))).flatMap(([role, ids]) => ids.map((id) => [id, role])),
);

/** `text-secondary` (the CSS variable spelling) → `text.secondary`. */
const BY_VARIABLE = new Map([...LEGACY.keys()].map((id) => [id.split('.').join('-'), id]));

const COLOUR_PROPS = new Set(['color', 'bgcolor', 'borderColor', 'backgroundColor']);

const VARIABLE_RE = /--el-palette-([A-Za-z0-9-]+)/g;

const message = (id) => {
  const role = LEGACY.get(id);
  return `R-T15 [legacy-palette] ${id} → role.${role} (theme.vars.palette.role.${role})`;
};

/** Names of a member chain from its root: `theme.vars.palette.text` → ['theme', 'vars', 'palette', 'text']. */
function chainNames(node) {
  if (node.type === 'TSNonNullExpression' || node.type === 'TSAsExpression') return chainNames(node.expression);
  if (node.type === 'Identifier') return [node.name];
  if (node.type !== 'MemberExpression') return ['?'];
  const head = chainNames(node.object);
  if (!node.computed && node.property.type === 'Identifier') return [...head, node.property.name];
  if (node.computed && node.property.type === 'Literal' && typeof node.property.value === 'string') {
    return [...head, node.property.value];
  }
  return [...head, '?'];
}

/** The palette id a member expression reads after `.vars.palette`, or null. */
function paletteIdOf(node) {
  const names = chainNames(node);
  for (let i = 0; i < names.length - 2; i += 1) {
    if (names[i] === 'vars' && names[i + 1] === 'palette') return names.slice(i + 2).join('.');
  }
  return null;
}

/** String literals a colour value can be: the literal itself or the branches of a conditional. */
function stringValues(node) {
  if (!node) return [];
  if (node.type === 'Literal' && typeof node.value === 'string') return [node];
  if (node.type === 'ConditionalExpression') return [...stringValues(node.consequent), ...stringValues(node.alternate)];
  if (node.type === 'JSXExpressionContainer') return stringValues(node.expression);
  return [];
}

export const legacyPaletteId = {
  meta: {
    type: 'suggestion',
    docs: {
      description:
        'R-T15: a generic legacy palette id (legacy-roles.json) has a brand-guide role; new code reads the role',
    },
    schema: [],
  },
  create(context) {
    const reportValues = (value) => {
      for (const literal of stringValues(value)) {
        if (LEGACY.has(literal.value)) context.report({ node: literal, message: message(literal.value) });
      }
    };
    return {
      MemberExpression(node) {
        const id = paletteIdOf(node);
        if (id !== null && LEGACY.has(id)) context.report({ node, message: message(id) });
      },
      ...stringVisitors((text, node) => {
        for (const match of text.matchAll(VARIABLE_RE)) {
          const id = BY_VARIABLE.get(match[1]);
          if (id) context.report({ node, message: message(id) });
        }
      }),
      Property(node) {
        if (COLOUR_PROPS.has(propertyKeyName(node))) reportValues(node.value);
      },
      JSXAttribute(node) {
        if (node.name.type === 'JSXIdentifier' && COLOUR_PROPS.has(node.name.name)) reportValues(node.value);
      },
    };
  },
};
