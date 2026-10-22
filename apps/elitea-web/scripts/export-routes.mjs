#!/usr/bin/env node
/**
 * NOTE: `tools/uictl`, the consumer this export was written for, was deleted
 * with the apps/elitea-ui baseline submodule. parity/new-app-routes.json is
 * kept (and drift-checked in ci-web.yml) as the record of the mounted routes.
 *
 * Exports the NEW app's mounted route patterns as the []string JSON P1's
 * `tools/uictl parity-routes --new-routes <file>` expected (spec §9.3 unit
 * R1; `apps/elitea-web/parity/REPRODUCE.md`'s `LoadNewRoutes`/`DiffNewApp`
 * — exact SET EQUALITY against the OLD app's baseline-extracted mounted
 * patterns, in the OLD app's `:param`/`*` syntax, not TanStack's `$param`).
 *
 * Reads the REAL generated `src/routeTree.gen.ts` (produced by the
 * installed `@tanstack/router-generator`, not reimplemented here) and
 * converts its `FileRouteTypes.fullPaths` union to old-app syntax:
 *
 *  - `$param` segments -> `:param` (TanStack's dynamic-segment marker vs
 *    old app's `react-router` one).
 *  - A trailing-slash "index" fullPath (`/agents/`, `/apps/`, ...) is an
 *    artifact of this unit's `<dir>/index.tsx` file convention (chosen so
 *    sibling files under the same directory do NOT structurally nest under
 *    it — see `_shell/agents/index.tsx`'s header) and maps back to the
 *    BARE path old app mounts for that same route
 *    (`RouteDefinitions.Applications = '/agents'`, no trailing slash).
 *  - EXCEPTION: `/settings/` maps to the literal `/settings (index)`
 *    (ROUTE-052), not bare `/settings` — `/settings` is ALREADY ROUTE-051
 *    (the layout itself, `RouteDefinitions.Settings`), and P1's manifest
 *    /`uictl`'s baseline extraction records the settings index redirect
 *    under this special literal (its old-app JSX has no `path` of its own
 *    to extract — a nested `<Route index element={<Navigate .../>}/>` —
 *    so `tools/uictl/internal/routes/routes.go`'s `ExtractBaseline`
 *    detects it by presence of `<Navigate ... to="model-configuration">`
 *    text instead, and labels it this way).
 *  - `/*` (ROUTE-002, the `AppLayout` shell wrapper) and bare `*`
 *    (ROUTE-071, `Page404`) are added manually: neither materialises as
 *    its own TanStack route in this tree (`_shell` is a PATHLESS layout
 *    contributing no path segment of its own; unit R3's `__404.tsx`
 *    deliberately exports no `Route` at all — see that file's header for
 *    why, and P11/D4 for why old app's own `Page404` mount is likewise
 *    structurally unreachable there too) — but the OLD app's
 *    `ExtractBaseline` finds both as literal `path="*"` JSX text
 *    regardless of runtime reachability, so `ext.Mounted` requires both
 *    for `DiffNewApp`'s exact-set-equality check to pass.
 */
import { readFileSync, writeFileSync } from 'node:fs';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { compareGenerated } from './lib/generated-drift.mjs';

const routeTreePath = fileURLToPath(new URL('../src/routeTree.gen.ts', import.meta.url));
const outPath = fileURLToPath(new URL('../parity/new-app-routes.json', import.meta.url));

const source = readFileSync(routeTreePath, 'utf8');
// Stops at the NEXT sibling field (`fileRoutesByTo:`), not the interface's
// final closing brace: `FileRouteTypes` also declares `to:`/`id:` unions
// immediately after `fullPaths:` with no blank line or brace between them,
// so a naive "capture until \n}" match swallows those too (reproduced: an
// earlier version of this script emitted 144 entries instead of 73,
// including raw `/_shell/...` ids and `__root__` from the `id:` union).
const match = /fullPaths:\s*([\s\S]*?)\n {2}fileRoutesByTo:/.exec(source);
if (!match) {
  throw new Error(`export-routes: could not find "fullPaths:" union in ${routeTreePath}`);
}
const fullPaths = [...match[1].matchAll(/'([^']*)'/g)].map((m) => m[1]);
if (fullPaths.length === 0) {
  throw new Error('export-routes: parsed zero fullPaths — routeTree.gen.ts shape may have changed');
}

function toOldSyntax(tanstackPath) {
  // Named params first ($projectId -> :projectId), then the bare splat
  // segment (a lone "$", unit R3's `$projectId.$.tsx` -> `/$projectId/$`)
  // to old app's `*` (react-router splat syntax) — order matters: the
  // named-param pass only touches `$` immediately followed by a word
  // character, so it never touches the bare wildcard segment.
  const withColonParams = tanstackPath.replaceAll(/\$([A-Za-z_][A-Za-z0-9_]*)/g, ':$1');
  const withSplat = withColonParams.replace(/\/\$$/, '/*');
  if (withSplat === '/settings/') return '/settings (index)';
  if (withSplat === '/') return '/';
  if (withSplat.endsWith('/')) return withSplat.slice(0, -1);
  return withSplat;
}

const converted = fullPaths.map(toOldSyntax);
const MANUAL_ADDITIONS = [
  '/*', // ROUTE-002: AppLayout shell wrapper — _shell is pathless, contributes no fullPath of its own.
  '*', // ROUTE-071: Page404 — unit R3's __404.tsx deliberately exports no Route (see its header).
];
const patterns = [...new Set([...converted, ...MANUAL_ADDITIONS])].sort();
const rendered = `${JSON.stringify(patterns, null, 2)}\n`;

function parseArgs(argv) {
  const opts = { check: false };
  for (const arg of argv) {
    if (arg === '--check') {
      opts.check = true;
    } else if (arg === '--help' || arg === '-h') {
      console.log('usage: export-routes.mjs [--check]');
      process.exit(0);
    } else {
      console.error(`export-routes: unknown argument: ${arg}`);
      process.exit(2);
    }
  }
  return opts;
}

/**
 * --check mode (issue #490): compare the committed parity/new-app-routes.json
 * with what this run renders, and write nothing. An absent committed file and
 * an empty render are both failures — see scripts/lib/generated-drift.mjs.
 */
function runCheck() {
  let actual = null;
  try {
    actual = readFileSync(outPath, 'utf8');
  } catch {
    actual = null;
  }
  const result = compareGenerated([{ path: 'apps/elitea-web/parity/new-app-routes.json', expected: rendered, actual }]);
  if (!result.ok) {
    console.error('export-routes: --check FAIL');
    for (const failure of result.failures) console.error(`  ${failure}`);
    console.error('  command: node scripts/export-routes.mjs');
    process.exit(1);
  }
  console.log(`export-routes: --check OK — ${patterns.length} patterns match ${outPath}`);
}

const opts = parseArgs(process.argv.slice(2));
if (opts.check) {
  runCheck();
} else {
  writeFileSync(outPath, rendered);
  console.log(`export-routes: wrote ${patterns.length} patterns to ${outPath}`);
}
