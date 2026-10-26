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
 *          Flagged: an inline object/array literal body; a constant a body
 *          reads (at any depth, module level or in a function) that is an
 *          untyped literal, typed `any`/`unknown`/`object`, or a double
 *          cast; and a JSON import
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

const LOOSE_TYPES = new Set(['TSAnyKeyword', 'TSUnknownKeyword', 'TSObjectKeyword']);
const LOOSE_TYPE_NAMES = { TSAnyKeyword: 'any', TSUnknownKeyword: 'unknown', TSObjectKeyword: 'object' };

/**
 * Every identifier a body expression reads — through casts, conditionals,
 * spreads, calls and member chains — minus non-computed property names
 * (`x.key`, `{ key: … }`), which name no binding.
 */
function referencedNames(expr, into) {
  walk(expr, (node, ancestors) => {
    if (node.type !== 'Identifier') return;
    const parent = ancestors.at(-1);
    if (parent?.type === 'MemberExpression' && parent.property === node && !parent.computed) return;
    if (parent?.type === 'ObjectProperty' && parent.key === node && !parent.computed) return;
    into.add(node.name);
  });
}

/** Inline literal bodies, plus every name the bodies read. */
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
    referencedNames(body, bodyNames);
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

/** Why a declaration that a body reads is not a typed constant, or null. */
function declarationProblem(decl) {
  const annotation = decl.id.typeAnnotation?.typeAnnotation;
  if (annotation && LOOSE_TYPES.has(annotation.type)) {
    return `is typed \`${LOOSE_TYPE_NAMES[annotation.type]}\`, which checks nothing — use the generated model type`;
  }
  const init = decl.init;
  if (init?.type === 'TSAsExpression' && init.expression.type === 'TSAsExpression') {
    return 'is a double cast (`as … as T`), which checks nothing — annotate it instead';
  }
  const literal = init?.type === 'TSAsExpression' ? init.expression : init;
  if (!annotation && literal && LITERAL_BODY_TYPES.has(literal.type)) {
    return 'has no type — annotate it with the generated model type (or `satisfies` it)';
  }
  return null;
}

/**
 * JSON imports, and declarations (module level or inside a function) that a
 * body reads but that are not typed constants.
 */
function declarationFindings(filename, program, bodyNames) {
  const findings = [];
  for (const stmt of program.body) {
    if (stmt.type === 'ImportDeclaration' && stmt.source.value.endsWith('.json')) {
      findings.push({
        rule: 'R-M2',
        file: filename,
        line: stmt.loc.start.line,
        message: `imports ${stmt.source.value} — JSON fixtures are retired; ${FIX}`,
      });
    }
  }
  walk(program, (node) => {
    if (node.type !== 'VariableDeclarator' || node.id.type !== 'Identifier' || !bodyNames.has(node.id.name)) return;
    const problem = declarationProblem(node);
    if (problem) {
      findings.push({ rule: 'R-M2', file: filename, line: node.loc.start.line, message: `body constant ${node.id.name} ${problem}` });
    }
  });
  return findings;
}

/**
 * R-M2: scan a hand-written handler module's source. Returns findings;
 * empty == pass. A `satisfies` initializer is a TSSatisfiesExpression, not a
 * literal, so it passes the untyped-constant check by construction. A
 * constant no body reads (a handler list, say) is not checked.
 */
export function checkHandlerSource(filename, source) {
  const ast = parse(source, {
    sourceType: 'module',
    plugins: filename.endsWith('.tsx') ? ['typescript', 'jsx'] : ['typescript'],
  });
  const { findings, bodyNames } = bodyFindings(filename, ast.program);
  return [...declarationFindings(filename, ast.program, bodyNames), ...findings];
}
