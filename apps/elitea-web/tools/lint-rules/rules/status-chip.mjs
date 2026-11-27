/**
 * A status drawn with a coloured MUI `Chip` by hand (ADR-0062 decision 4, the
 * status family), for theme-gate check 10 (the style ratchet) ONLY. A status
 * comes from `StatusChip` in `shared/ui/status-chip`; existing coloured chips
 * are allowed, a PR may not add more in a folder than it removes
 * (scripts/lib/style-ratchet-core.mjs).
 *
 * Only a coloured chip is a status. A `Chip` with no `color`, or with
 * `color="default"`, is a tag, a value in a field or a filter — other families —
 * and is not counted; banning every `Chip` would push them into the status.
 *
 * Counts a JSX element bound to MUI's Chip with a `color` attribute other than
 * the literal "default":
 *   import { Chip } from '@mui/material'          (any local name)
 *   import Chip from '@mui/material/Chip'
 *   import { Chip } from '@mui/material/Chip'
 *   <Chip color="success" />   <Chip color={tone} />   <Chip color="default" /> is not counted
 * shared/ui is exempt in scripts/style-ratchet.oxlintrc.json.
 */
const MESSAGE = '[status-chip] a status comes from StatusChip (shared/ui/status-chip); if it is not enough, extend StatusChip';

function isChipImport(node) {
  const source = node.source.value;
  if (source !== '@mui/material' && source !== '@mui/material/Chip') return [];
  if (node.importKind === 'type') return [];
  const names = [];
  for (const spec of node.specifiers) {
    if (spec.importKind === 'type') continue;
    if (spec.type === 'ImportDefaultSpecifier' && source === '@mui/material/Chip') names.push(spec.local.name);
    if (spec.type === 'ImportSpecifier') {
      const imported = spec.imported.type === 'Identifier' ? spec.imported.name : spec.imported.value;
      if (imported === 'Chip' || (imported === 'default' && source === '@mui/material/Chip')) names.push(spec.local.name);
    }
  }
  return names;
}

function isDefaultColour(value) {
  if (!value) return false;
  if (value.type === 'Literal') return value.value === 'default';
  if (value.type === 'JSXExpressionContainer') {
    const e = value.expression;
    return (e.type === 'Literal' && e.value === 'default') || (e.type === 'TemplateLiteral' && e.expressions.length === 0 && e.quasis[0]?.value.cooked === 'default');
  }
  return false;
}

export const statusChip = {
  meta: {
    type: 'suggestion',
    docs: { description: 'A status is drawn by StatusChip in shared/ui, not by a coloured MUI Chip' },
    schema: [],
  },
  create(context) {
    const chips = new Set();
    return {
      ImportDeclaration(node) {
        for (const name of isChipImport(node)) chips.add(name);
      },
      JSXOpeningElement(node) {
        if (node.name.type !== 'JSXIdentifier' || !chips.has(node.name.name)) return;
        const colour = node.attributes.find((a) => a.type === 'JSXAttribute' && a.name.type === 'JSXIdentifier' && a.name.name === 'color');
        if (colour && !isDefaultColour(colour.value)) context.report({ node: colour, message: MESSAGE });
      },
    };
  },
};
