/**
 * dependency-cruiser@18.1.0 — the layer gate of record (decision D2 dropped
 * eslint-plugin-boundaries; this file is the SOLE enforcement of R-L1/R-L2/
 * R-L3 from spec §3.4, per the §3.2 layer model).
 *
 *   app → processes → pages → widgets → features → entities → shared
 *
 * Imports flow downward only; zero cycles at any granularity; slices under
 * processes/widgets/features/entities are entered through index.ts only; no
 * sideways imports within features/ or entities/ (cross-feature communication
 * goes through entities/ types or a processes/ store).
 *
 * Run: npx depcruise src --config .dependency-cruiser.cjs
 * Fixture proof: scripts/check-gates-selftest.mjs cruises
 * tools/lint-rules/fixtures/depcruise/{bad,good} with this same config and
 * asserts every rule below fires in `bad` and none fires in `good`.
 */

/** Layers strictly above the keyed layer (i.e. forbidden import targets). */
const LAYERS_ABOVE = {
  shared: ['app', 'processes', 'pages', 'widgets', 'features', 'entities'],
  entities: ['app', 'processes', 'pages', 'widgets', 'features'],
  features: ['app', 'processes', 'pages', 'widgets'],
  widgets: ['app', 'processes', 'pages'],
  pages: ['app', 'processes'],
  processes: ['app'],
};

const upwardRules = Object.entries(LAYERS_ABOVE).map(([layer, above]) => ({
  name: `no-upward-from-${layer}`,
  comment: `R-L1 (§3.2): ${layer}/ may not import from ${above.join('/')} — imports flow downward only`,
  severity: 'error',
  from: { path: `^src/${layer}/` },
  to: { path: `^src/(${above.join('|')})/` },
}));

/** Slice-holding layers (§3.3 anatomy applies to exactly these four). */
const SLICED = '(processes|widgets|features|entities)';

module.exports = {
  forbidden: [
    ...upwardRules,
    {
      name: 'no-circular',
      comment: 'R-L2 (§3.4): zero cycles at any granularity (old app: 21 cycle groups over 367 modules)',
      severity: 'error',
      from: {},
      to: { circular: true },
    },
    {
      name: 'no-sideways-features',
      comment: 'R-L1 (§3.2): no sideways imports within features/ — cross-feature communication goes through entities/ or a processes/ store',
      severity: 'error',
      from: { path: '^src/features/([^/]+)/', pathNot: '^src/features/chat-messages/' },
      to: { path: '^src/features/', pathNot: '^src/features/$1/' },
    },
    {
      name: 'no-sideways-entities',
      comment:
        'R-L1 (§3.2): no sideways imports within entities/ — with ONE exception, ' +
        'entities/provider-run. That slice carries no domain: it is the invoke → ' +
        'poll → cancel loop EVERY sub-application behind a facade runs (ADR-0023 ' +
        'decision 4), and it sits in entities/ only because the sibling fence ' +
        'no-sideways-features forbade the two features that first needed it from ' +
        'sharing one — its own header says so. A second sub-application (Inventory) ' +
        'needs the same loop from its entity, and the alternatives are worse: a ' +
        'copy of the poll contract per application is the drift ADR-0023 extracted ' +
        'it to prevent, and moving the domain code up into features/ would put four ' +
        'features back into sideways imports of each other. The exception is on the ' +
        'TARGET only: nothing may import a domain entity sideways, and provider-run ' +
        'itself imports no entity at all, so this cannot open a cycle.',
      severity: 'error',
      from: { path: '^src/entities/([^/]+)/', pathNot: '^src/entities/provider-run/' },
      to: {
        path: '^src/entities/',
        pathNot: '^src/entities/($1|provider-run)/',
      },
    },
    {
      name: 'no-deep-slice-import',
      comment: 'R-L3 (§3.3): a slice is imported only through its index.ts — deep imports are refactor-hostile coupling',
      severity: 'error',
      from: { path: '^src/(?:app|pages)/' },
      to: {
        path: `^src/${SLICED}/[^/]+/.+`,
        pathNot: `^src/${SLICED}/[^/]+/index\\.ts$`,
      },
    },
    {
      name: 'no-deep-slice-import-cross-slice',
      comment: 'R-L3 (§3.3): slice-to-slice imports also enter via index.ts only (intra-slice imports are free)',
      severity: 'error',
      from: { path: `^src/${SLICED}/([^/]+)/`, pathNot: `^src/features/chat-messages/` },
      to: {
        path: `^src/${SLICED}/[^/]+/.+`,
        pathNot: ['^src/$1/$2/', `^src/${SLICED}/[^/]+/index\\.ts$`],
      },
    },
    {
      name: 'not-to-unresolvable',
      comment: 'R-L3 support: every dependency must resolve on disk',
      severity: 'error',
      from: {},
      to: { couldNotResolve: true },
    },
  ],
  options: {
    doNotFollow: { path: 'node_modules' },
    tsConfig: { fileName: 'tsconfig.json' },
    exclude: { path: '\\.(test|spec|smoke\\.test)\\.[jt]sx?$|__mocks__|__tests__' },
    // TypeScript 7 (D2) ships no JS compiler API, and dependency-cruiser 18's
    // tsc transpiler peers `typescript >=2 <7` — with only TS 7 installed it
    // silently cruises 0 modules (verified 2026-07-26: "0 modules cruised" +
    // the missing-typescript-transpiler warning, .ts/.tsx extensions
    // disabled). @swc/core@1.15.46 is installed as the TS transpiler
    // dependency-cruiser supports (its presence alone enables .ts/.tsx —
    // no `parser` option needed; `parser: "swc"` as primary parser is
    // deprecated upstream and NOT used here).
    // KNOWN LIMIT (recorded in the F2 report): the swc path erases type-only
    // imports before extraction, so `import type` edges are not layer-checked
    // (tsPreCompilationDeps needs the tsc parser). verbatimModuleSyntax
    // forces type-only imports to be syntactically marked and they are erased
    // from the runtime bundle, so the gate covers every runtime edge.
    tsPreCompilationDeps: true,
    enhancedResolveOptions: {
      exportsFields: ['exports'],
      conditionNames: ['import', 'require', 'node', 'default', 'types'],
      mainFields: ['module', 'main', 'types', 'typings'],
    },
    reporterOptions: {
      dot: { collapsePattern: 'node_modules/(?:@[^/]+/[^/]+|[^/]+)' },
    },
  },
};
