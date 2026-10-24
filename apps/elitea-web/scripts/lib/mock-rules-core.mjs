/**
 * R-M2 decision logic (spec §6.5), proven by the F2 gate self-test and wired
 * into CI by scripts/check-handlers.mjs.
 *
 *   R-M2 — a hand-written MSW handler answers with one of two things:
 *          - a generated contract mock (`get<Op>ResponseMock()` from
 *            `shared/api/generated/**.msw.ts`, whose value is the operation's
 *            example in v2.yaml — scripts/check-spec-examples.mjs checks it
 *            against the schema), or
 *          - a module constant with a type (`const X: T = …` or
 *            `… satisfies T`), typed with the generated model type where the
 *            contract has one, so `tsc` checks its shape.
 *          Flagged: an inline object/array literal body, an untyped
 *          object/array module constant used as a body, and a JSON import
 *          (the hand-written JSON fixtures were retired 2026-10-23; nothing
 *          type-checked them).
 *
 * R-M4 (fixture freshness) and the fixture `recordedAt` metadata went with
 * those fixtures. R-M5 is enforced at runtime by src/test/setup.ts.
 */
import { parse } from '@babel/parser';

import { walk } from './budgets-core.mjs';

const RESPONSE_FACTORIES = new Set(['json', 'text', 'xml', 'html']);
const RESPONSE_OBJECTS = new Set(['HttpResponse', 'Response']);
const LITERAL_BODY_TYPES = new Set(['ObjectExpression', 'ArrayExpression']);
const FIX = 'use a generated get<Op>ResponseMock() or a typed module constant';

/** The root identifier of a body expression (`X`, `X.y`, `X[0]`), if any. */
function rootName(node) {
  let cur = node;
  while (cur.type === 'MemberExpression') cur = cur.object;
  return cur.type === 'Identifier' ? cur.name : undefined;
}

/**
 * Inline literal bodies, plus the names of the identifiers used as bodies
 * (the untyped-constant check only looks at those).
 */
function bodyFindings(filename, program) {
  const findings = [];
  const bodyNames = new Set();
  walk(program, (node) => {
    if (node.type !== 'CallExpression') return;
    const callee = node.callee;
    if (
      callee.type !== 'MemberExpression' ||
      callee.object.type !== 'Identifier' ||
      !RESPONSE_OBJECTS.has(callee.object.name) ||
      callee.property.type !== 'Identifier' ||
      !RESPONSE_FACTORIES.has(callee.property.name)
    ) {
      return;
    }
    const body = node.arguments[0];
    if (!body) return;
    const name = rootName(body);
    if (name !== undefined) bodyNames.add(name);
    if (LITERAL_BODY_TYPES.has(body.type)) {
      findings.push({
        rule: 'R-M2',
        file: filename,
        line: node.loc.start.line,
        message: `${callee.object.name}.${callee.property.name}(<inline literal>) — ${FIX}`,
      });
    }
  });
  return { findings, bodyNames };
}

function topLevelFindings(filename, program, bodyNames) {
  const findings = [];
  for (const raw of program.body) {
    const stmt = raw.type === 'ExportNamedDeclaration' && raw.declaration ? raw.declaration : raw;
    if (stmt.type === 'ImportDeclaration' && stmt.source.value.endsWith('.json')) {
      findings.push({
        rule: 'R-M2',
        file: filename,
        line: stmt.loc.start.line,
        message: `imports ${stmt.source.value} — JSON fixtures are retired; ${FIX}`,
      });
    }
    if (stmt.type !== 'VariableDeclaration') continue;
    for (const decl of stmt.declarations) {
      const init = decl.init;
      if (!init || !LITERAL_BODY_TYPES.has(init.type) || decl.id.typeAnnotation || !bodyNames.has(decl.id.name)) continue;
      findings.push({
        rule: 'R-M2',
        file: filename,
        line: decl.loc.start.line,
        message: `body constant ${decl.id.name} has no type — annotate it with the generated model type (or \`satisfies\` it)`,
      });
    }
  }
  return findings;
}

/**
 * R-M2: scan a hand-written handler module's source. Returns findings;
 * empty == pass. A `satisfies` initializer is a TSSatisfiesExpression, not a
 * literal, so it passes the untyped-constant check by construction. A
 * constant that is not a body (a handler list, say) is not checked.
 */
export function checkHandlerSource(filename, source) {
  const ast = parse(source, {
    sourceType: 'module',
    plugins: filename.endsWith('.tsx') ? ['typescript', 'jsx'] : ['typescript'],
  });
  const { findings, bodyNames } = bodyFindings(filename, ast.program);
  return [...topLevelFindings(filename, ast.program, bodyNames), ...findings];
}
