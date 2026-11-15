#!/usr/bin/env node
/**
 * Phase 0 of docs/plans/route-wiring-plan.md — freeze the route -> page map.
 *
 * Emits `parity/route-wiring-map.json`: one row per route that currently
 * renders scaffolding instead of its built page component.
 *
 * MECHANICAL fields (re-derived on every run, never hand-edited):
 *   routeFile, url, routeNum, specUrl, target, rendersRouteShell, pageImports
 * — all read out of the route file itself. Every stub route carries a
 * `ROUTE-NNN \`/url\` -> \`Component\`` docstring naming its target; that
 * docstring IS the contract.
 *
 * CURATED fields (RESOLUTION below, hand-authored from evidence, reviewed):
 *   targetPath, targetExport, requiredProps, status, note
 *
 * Run: node scripts/build-route-wiring-map.mjs [--check]
 * `--check` exits non-zero if the emitted map differs from the committed one
 * (CI use), so the map cannot silently drift from the routes.
 */
import { existsSync, readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';

import { checkFloors } from './lib/gate-floor.mjs';

const ROUTES = 'src/routes';
const OUT = 'parity/route-wiring-map.json';

/*
 * Floor on the walked route set (issue #528).
 *
 * `--check` compares the emitted bytes against the committed artifact, so it
 * proves only that the two agree. A run that walks NO route emits
 * `total: 0, routes: []`, and the writer branch accepts that. Commit it once
 * and `--check` agrees with it forever, at zero routes.
 *
 * A renamed `src/routes` covers one vector by accident — `walk` throws. A
 * changed file filter, an added skip, or a route tree that moves one level
 * deeper does not throw. It just walks less.
 *
 * Measured on 2026-08-28: 79 routes (53 wired, 25 unreviewed, 1
 * parity-dead-code).
 */
const MIN_ROUTES = 60;

/**
 * status values:
 *   ready              - target exists, no route-owned state needed; a
 *                        near-mechanical import swap.
 *   needs-route-state  - target exists but requires props the route must own
 *                        (tab/search state, navigation callbacks, projectId).
 *                        Real work, no external blocker.
 *   blocked-codegen    - target requires an injected `deps` writer whose
 *                        endpoint is absent from
 *                        src/shared/api/endpoints.manifest.json. The Go
 *                        handler EXISTS (see note); the generated client does
 *                        not cover it. Blocked on codegen, not on backend.
 *   hybrid-defect      - already renders the real page, but ALSO renders a
 *                        RouteShell heading above it. Remove the shell.
 *   parity-dead-code   - route is wired and CORRECT: the page it renders is
 *                        near-empty because the BASELINE's page is near-empty
 *                        too. Not outstanding work — verified parity. Kept as
 *                        its own status so a reader does not mistake a
 *                        faithful port of dead code for an unfinished one.
 *   wired              - DONE. The route now renders its real page component;
 *                        kept in the map so the tracker shows completed work
 *                        rather than silently dropping rows.
 */
const RESOLUTION = {
  // ---- agents (A1) ----
  '_shell/agents-hub.tsx': ['pages/marketplace/Marketplace', 'Marketplace', [], 'wired', 'Redirect source into the Marketplace (ADR-0051); redirectToMarketplace carries ?agentId= and ?tab=.'],
  '_shell/elitea-catalog.tsx': ['pages/marketplace/Marketplace', 'Marketplace', [], 'wired', 'Redirect source into the Marketplace (ADR-0051); redirectToMarketplace carries ?agentId= and ?tab=.'],
  '_shell/agents/$tab.tsx': ['pages/agents/Applications', 'Applications', [], 'wired', 'Route keeps its ExclusiveOutlet composition; only the leaf component changes.'],
  '_shell/agents/$tab.$agentId.tsx': ['pages/agents/EditApplication', 'EditApplication', [], 'wired', ''],
  '_shell/agents/create.tsx': ['pages/agents/CreateApplication', 'CreateApplication', [], 'wired', ''],

  // ---- apps (A6) ----
  '_shell/apps/index.tsx': ['pages/apps/Apps', 'Apps', [], 'wired', ''],
  '_shell/apps/$tab.tsx': ['pages/apps/Apps', 'Apps', [], 'wired', ''],
  '_shell/apps/$tab.$appId.tsx': ['pages/apps/AppDetail', 'AppDetail', [], 'wired', ''],
  '_shell/apps/create.tsx': ['pages/toolkits/CreateToolkit', 'CreateToolkit', ['isApplication', 'deps.createToolkit'], 'wired', 'Apps reuse the toolkit creator with isApplication.'],

  // ---- credentials (A7) ----
  '_shell/credentials/$tab.tsx': ['pages/credentials/Credentials', 'Credentials', ['tab', 'projectId', 'onSelectCredential', 'onCreateNew'], 'wired', 'Route must supply projectId from context and both navigation callbacks.'],
  '_shell/credentials/$tab.$credential_uid.tsx': ['pages/credentials/EditCredential', 'EditCredential', ['context', 'credentialUid', 'onSaved', 'onDiscarded'], 'wired', 'Docstring target "EditCredentialFromMain" = EditCredential with the main-app context, not a separate component.'],
  '_shell/credentials/create-credential.tsx': ['pages/credentials/CreateCredential', 'CreateCredential', ['context', 'credentialType', 'onCreated', 'onCancelled', 'onTypeChosen'], 'wired', 'Docstring target "CreateCredentialFromMain" = CreateCredential with the main-app context. `credentialType` comes from the ROUTE-024 child match (useParams strict:false) — the child is an empty pattern-A route, so this parent reads its param.'],

  // ---- misc (A11-A13) ----
  '_shell/help-center.tsx': ['pages/help-center/HelpCenterPage', 'HelpCenterPage', [], 'wired', 'Name drift only: docstring says `Resources`, the built component is HelpCenterPage (also a default export).'],
  '_shell/onboarding.tsx': ['pages/onboarding/Onboarding', 'Onboarding', [], 'wired', 'Route currently renders RouteShell + Outlet; the 398-line Onboarding page is unimported.'],

  // ---- mcps (A5) ----
  '_shell/mcps/$tab.tsx': ['pages/toolkits/Toolkits', 'Toolkits', ['isMCP'], 'wired', 'No pages/mcps list component exists and none is needed — Toolkits already branches on isMCP throughout (Toolkits.tsx:280 names this route family).'],
  '_shell/mcps/create.tsx': ['pages/toolkits/CreateToolkit', 'CreateToolkit', ['isMCP', 'deps.createToolkit'], 'wired', ''],
  '_shell/mcps/$tab.$mcpId.tsx': ['pages/toolkits/EditToolkit', 'EditToolkit', ['isMCP', 'deps.saveToolkit'], 'wired', ''],

  // ---- pipelines (A2) ----
  '_shell/pipelines/$tab.tsx': ['pages/pipelines/Pipelines', 'Pipelines', [], 'wired', ''],
  '_shell/pipelines/$tab.$agentId.tsx': ['pages/pipelines/EditPipeline', 'EditPipeline', [], 'wired', ''],
  '_shell/pipelines/create.tsx': ['pages/pipelines/CreatePipeline', 'CreatePipeline', [], 'wired', ''],

  // ---- settings (A9, issue #25) ----
  '_shell/settings/create-configuration.tsx': ['pages/credentials/CreateCredential', 'CreateCredential', ['context', 'credentialType', 'configurationMode', 'onCreated', 'onCancelled', 'onTypeChosen'], 'wired', '`credentialType` comes from the ROUTE-064 child match, same shape as its /credentials twin.'],
  '_shell/settings/edit-configuration.$credential_uid.tsx': ['pages/credentials/EditCredential', 'EditCredential', ['context', 'credentialUid', 'configurationMode', 'onSaved', 'onDiscarded'], 'wired', 'ROUTE-065. Param is $credential_uid, NOT $uid — "the MOUNTED route wins".'],
  '_shell/settings/secrets.tsx': ['pages/settings/Secrets', 'SecretsContent', ['shouldCreate', 'search', 'onSearchChange'], 'wired', 'Renders SecretsContent AND a RouteShell heading. Also passes search="" / onSearchChange={() => {}} — the search control is dead. Route must own search state.'],
  '_shell/settings/model-configuration.tsx': ['pages/settings/AIConfiguration', 'AIConfiguration', ['projectId'], 'wired', 'Renders AIConfiguration AND a RouteShell heading. The ModelConfiguration layer (issue #80) is now composed inside AIConfiguration.'],

  // ---- skills (A3, issue #23) ----
  '_shell/skills/$tab.tsx': ['pages/skills/Skills', 'Skills', [], 'wired', ''],
  '_shell/skills/$tab.$skillId.tsx': ['pages/skills/EditSkill', 'EditSkill', [], 'wired', ''],
  '_shell/skills/create.tsx': ['pages/skills/CreateSkill', 'CreateSkill', [], 'wired', ''],

  // ---- toolkits (A4) ----
  '_shell/toolkits/$tab.tsx': ['pages/toolkits/Toolkits', 'Toolkits', [], 'wired', ''],
  '_shell/toolkits/create.tsx': ['pages/toolkits/CreateToolkit', 'CreateToolkit', ['deps.createToolkit'], 'wired', ''],
  '_shell/toolkits/$tab.$toolkitId.tsx': ['pages/toolkits/EditToolkit', 'EditToolkit', ['deps.saveToolkit'], 'wired', ''],

  // ---- user-public (A13) ----
  '_shell/user-public/$tab.tsx': ['pages/user-public/ui/UserPublicPage', 'UserPublicPage', ['tab', 'onTabChange', 'statuses', 'onStatusesChange', 'authorId', 'authorName'], 'wired', 'Heaviest route-owned state in the set: tab + status filters + author identity.'],
  '_shell/user-public/agents.$agentId.tsx': ['pages/agents/EditApplication', 'EditApplication', [], 'wired', ''],
  '_shell/user-public/pipelines.$agentId.tsx': ['pages/pipelines/EditPipeline', 'EditPipeline', [], 'wired', ''],
  '_shell/user-public/apps.$appId.tsx': ['pages/apps/AppDetail', 'AppDetail', [], 'wired', ''],
  '_shell/user-public/toolkits.$toolkitId.tsx': ['pages/toolkits/EditToolkit', 'EditToolkit', ['deps.saveToolkit'], 'wired', ''],
  '_shell/user-public/mcps.$mcpId.tsx': ['pages/toolkits/EditToolkit', 'EditToolkit', ['isMCP', 'deps.saveToolkit'], 'wired', ''],

  // ---- artifacts (A8) — inline stubs added by PR #82, NOT RouteShell ----
  '_shell/artifacts/index.tsx': ['pages/artifacts/Artifacts', 'Artifacts', [], 'wired', 'Currently a hand-written heading + one Button. The 363-line Artifacts page is unimported. Revert the stub rather than editing it.'],
  '_shell/artifacts/create-bucket.tsx': ['pages/artifacts/CreateBucket', 'CreateBucket', [], 'wired', 'Currently a TextField whose submit only navigates — no API call. CreateBucket (built, tested) is unimported.'],

  // ---- wired, but to scaffolding ----
  '_shell/mode-switch.tsx': ['pages/mode-switch/ModeSwitch', 'ModeSwitch', [], 'parity-dead-code', 'Verified parity, not a gap (Phase 2). The BASELINE page is itself dead code: apps/elitea-ui/src/pages/ModeSwitch.jsx hard-codes `const enableToggle = false` and so renders only <h1>Switch Mode</h1>, and a whole-repo grep finds ZERO inbound links to /mode-switch in either app — it is mounted but unreachable by navigation in both. The port reproduces that exactly, and widgets/sidebar/__tests__/navSections.test.ts:119 already asserts it is not a nav item. Nothing to implement; shipping the toggle would be a NEW feature, not parity.'],
};

const DOMAIN = (f) => {
  const m = f.match(/^_shell\/([^/.]+)/);
  const d = m ? m[1] : 'root';
  return ['agents-hub', 'help-center', 'onboarding', 'mode-switch'].includes(d) ? 'misc' : d;
};

function walk(dir, acc = []) {
  for (const e of readdirSync(dir)) {
    const p = join(dir, e);
    if (statSync(p).isDirectory()) walk(p, acc);
    else if (p.endsWith('.tsx') && !p.includes('.test.') && !p.includes('__tests__')) acc.push(p);
  }
  return acc;
}

/**
 * Resolves an import specifier to a repo-relative path, if it names a real file.
 * `@/x/Y` -> `src/x/Y.tsx`; relative specifiers resolve against the route file.
 */
function resolveImport(spec, routeRel) {
  const base = spec.startsWith('@/')
    ? join('src', spec.slice(2))
    : spec.startsWith('.')
      ? join(ROUTES, dirname(routeRel), spec)
      : null;
  if (!base) return null;
  for (const cand of [`${base}.tsx`, `${base}.ts`, join(base, 'index.tsx'), join(base, 'index.ts')]) {
    if (existsSync(cand)) return cand;
  }
  return null;
}

/**
 * Every `import {A, B} from 'spec'` / `import C from 'spec'` in the file,
 * as a Map of local binding name -> specifier.
 */
function importBindings(txt) {
  const out = new Map();
  const re = /import\s+(type\s+)?([^;]+?)\s+from\s+['"]([^'"]+)['"]/g;
  for (const [, isType, clause, spec] of txt.matchAll(re)) {
    if (isType) continue;
    for (const name of clause.replace(/[{}]/g, ' ').split(',')) {
      const local = name.trim().split(/\s+as\s+/).pop()?.trim();
      if (local && /^[A-Za-z_$][\w$]*$/.test(local)) out.set(local, spec);
    }
  }
  return out;
}

/**
 * Slices out the route options object — the `{...}` passed to the second call
 * of `createFileRoute('/x')({ ... })`. Scoping the `component:` lookup to this
 * object matters: the route files carry long prose docstrings, and a file-wide
 * regex happily matched "component: TanStack Router..." out of a comment and
 * reported a component that does not exist.
 */
function routeOptions(txt) {
  // `createFileRoute('/x')({...})`, and the root's
  // `createRootRouteWithContext<T>()({...})` / `createRootRoute()({...})`.
  const m = /(?:createFileRoute\([^)]*\)|createRootRoute(?:WithContext)?(?:<[^>]*>)?\(\s*\))\s*\(/.exec(txt);
  if (!m) return null;
  const open = txt.indexOf('{', m.index + m[0].length - 1);
  if (open < 0) return null;
  let depth = 0;
  for (let i = open; i < txt.length; i += 1) {
    if (txt[i] === '{') depth += 1;
    else if (txt[i] === '}') {
      depth -= 1;
      if (depth === 0) return txt.slice(open, i + 1);
    }
  }
  return null;
}

/** Slices out `function <name>(...) { ... }` by brace matching. */
function functionBody(txt, name) {
  const start = txt.search(new RegExp(`function\\s+${name}\\s*\\(`));
  if (start < 0) return null;
  const open = txt.indexOf('{', start);
  if (open < 0) return null;
  let depth = 0;
  for (let i = open; i < txt.length; i += 1) {
    if (txt[i] === '{') depth += 1;
    else if (txt[i] === '}') {
      depth -= 1;
      if (depth === 0) return txt.slice(start, i + 1);
    }
  }
  return null;
}

// Layout/status primitives a route may legitimately render without that
// counting as "this route renders a real page".
const NON_PAGE_JSX = new Set([
  'Outlet', 'ExclusiveOutlet', 'RouteShell', 'RoutePending', 'RouteError',
  'Fragment', 'Suspense', 'Navigate', 'ScrollRestoration', 'HeadContent', 'Scripts',
]);

const rows = [];
for (const file of walk(ROUTES).sort()) {
  const rel = relative(ROUTES, file);
  if (rel.startsWith('-')) continue; // -ui/, -guards/, -search/ helpers
  const txt = readFileSync(file, 'utf8');

  const url = txt.match(/createFileRoute\(\s*['"]([^'"]+)/)?.[1] ?? null;
  const doc = txt.match(/ROUTE-(\d+)\s+`([^`]*)`\s*(?:->|->)\s*\n?\s*\*?\s*`([^`]*)`/);
  const opts = routeOptions(txt) ?? '';
  const componentRef = /\bcomponent:\s*([^,\n]+)/.exec(opts)?.[1]?.trim() ?? null;
  // `component: Foo` names a component; `component: () => null` and other inline
  // forms are recorded as inline rather than mistaken for a missing component.
  const componentName = componentRef && /^[A-Za-z_$][\w$]*$/.test(componentRef) ? componentRef : null;
  const hasInlineComponent = !!componentRef && !componentName;

  const imports = importBindings(txt);
  const body = componentName ? functionBody(txt, componentName) : null;

  // Which JSX elements does the route's component actually render?
  const rendered = body
    ? [...new Set([...body.matchAll(/<([A-Z][\w$]*)/g)].map((m) => m[1]))]
    : [];
  const pageTags = rendered.filter((tag) => !NON_PAGE_JSX.has(tag));

  // DERIVED target: the first rendered non-primitive component that resolves to
  // a real module. This is read out of the tree, never out of RESOLUTION, so a
  // curated row that has drifted cannot disguise what the route really renders.
  let derivedExport = null;
  let derivedPath = null;
  let derivedSpecifier = null;
  for (const tag of pageTags) {
    const spec = imports.get(tag);
    const resolved = spec ? resolveImport(spec, rel) : null;
    if (resolved) { derivedExport = tag; derivedPath = resolved; derivedSpecifier = spec; break; }
  }
  // The component may itself be imported rather than declared locally
  // (`component: SomePage` with `import { SomePage } from ...`).
  if (!derivedPath && componentName && imports.has(componentName)) {
    const spec = imports.get(componentName);
    const resolved = resolveImport(spec, rel);
    if (resolved) { derivedExport = componentName; derivedPath = resolved; derivedSpecifier = spec; }
  }

  /**
   * bodyShape classifies what the route renders:
   *   renders-page  - delegates to a resolvable component module
   *   heading-only  - the exact stub shape: intrinsic markup with a heading and
   *                   no component delegation. This is what `/mcp-auth-callback`
   *                   shipped while its real 200-line page was imported by
   *                   nothing, and what the 38-file allowlist could not see.
   *   inline-markup - renders intrinsic markup only, without a heading
   *   layout-only   - renders nothing but Outlet/RouteShell-style primitives
   *   inline        - `component:` is an inline expression (e.g. `() => null`)
   *   redirect-only - no `component:` at all; the route only redirects/guards
   */
  let bodyShape;
  // A file under src/routes that creates no route at all (a colocated layout
  // component, or __404.tsx which is deliberately not a TanStack file-route).
  if (!routeOptions(txt)) bodyShape = 'not-a-route';
  else if (hasInlineComponent) bodyShape = 'inline';
  else if (!componentName) bodyShape = 'redirect-only';
  else if (derivedPath) bodyShape = 'renders-page';
  else if (pageTags.length === 0 && rendered.length > 0) bodyShape = 'layout-only';
  else if (body && /<h[1-6][\s>]/.test(body)) bodyShape = 'heading-only';
  else bodyShape = 'inline-markup';

  const curated = RESOLUTION[rel];
  // Only the review fields are consumed here; targetPath/targetExport are
  // derived from the tree above and cross-checked against RESOLUTION below.
  const [, , requiredProps, status, note] = curated ?? [];

  rows.push({
    routeFile: `${ROUTES}/${rel}`,
    domain: DOMAIN(rel),
    url,
    routeNum: doc ? `ROUTE-${doc[1]}` : null,
    specUrl: doc ? doc[2] : null,
    docstringTarget: doc ? doc[3] : null,
    component: componentName,
    bodyShape,
    // Derived from the tree. `targetPath`/`targetExport` keep their names so the
    // consumers (routeWiring.test.ts, the tracker) keep working, but they are no
    // longer hand-authored.
    targetPath: derivedPath,
    // The literal specifier as written in the route file. Consumers assert on
    // this rather than reconstructing it from targetPath: `@/widgets/app-shell`
    // resolves to `.../index.ts`, and reconstruction produced a
    // `@/widgets/app-shell/index` string that appears nowhere in the source.
    targetSpecifier: derivedSpecifier,
    targetExport: derivedExport,
    requiredProps: requiredProps ?? [],
    status: status ?? (bodyShape === 'renders-page' ? 'wired' : 'unreviewed'),
    rendersRouteShell: /-ui\/RouteShell'/.test(txt),
    note: note ?? '',
    curated: !!curated,
  });
}

/*
 * Gate 1 — no route may render the stub shape. A heading with no delegation is
 * indistinguishable from an unimplemented screen, and every E2E assertion that
 * falls back to `getByRole('heading')` is satisfied by it.
 */
const stubs = rows.filter((r) => r.bodyShape === 'heading-only');
if (stubs.length) {
  console.error('Routes whose component body is only a heading (stub shape):');
  for (const s of stubs) console.error(`  ${s.routeFile}  component=${s.component}`);
  process.exit(3);
}

/*
 * Gate 2 — a curated row that disagrees with the tree is a stale curation, not
 * a fact. RESOLUTION's targetPath/targetExport were hand-authored and had
 * drifted; this makes drift fail loudly instead of being served as truth.
 */
const curatedDrift = [];
for (const r of rows) {
  if (!r.curated) continue;
  const key = r.routeFile.slice(`${ROUTES}/`.length);
  const [cPath, cExport] = RESOLUTION[key];
  if (r.targetPath && r.targetPath !== `src/${cPath}.tsx`) {
    curatedDrift.push(`${r.routeFile}: renders ${r.targetPath}, RESOLUTION claims src/${cPath}.tsx`);
  } else if (r.targetExport && cExport && r.targetExport !== cExport) {
    curatedDrift.push(`${r.routeFile}: renders <${r.targetExport}>, RESOLUTION claims ${cExport}`);
  }
}
if (curatedDrift.length) {
  console.error('RESOLUTION disagrees with the tree (stale curation):');
  for (const d of curatedDrift) console.error(`  ${d}`);
  process.exit(4);
}

const missing = rows.filter((r) => r.targetPath && !existsSync(r.targetPath));
if (missing.length) {
  console.error('Unresolvable targets:', missing.map((m) => `${m.routeFile} -> ${m.targetPath}`));
  process.exit(2);
}

const byStatus = rows.reduce((a, r) => ({ ...a, [r.status]: (a[r.status] ?? 0) + 1 }), {});
const out = {
  $comment: 'Generated by scripts/build-route-wiring-map.mjs — Phase 0 of docs/plans/route-wiring-plan.md. Do not hand-edit; edit RESOLUTION in the script.',
  generated: 'phase-0',
  total: rows.length,
  byStatus,
  byDomain: rows.reduce((a, r) => ({ ...a, [r.domain]: (a[r.domain] ?? 0) + 1 }), {}),
  routes: rows,
};

// The floor runs before either branch. An empty walk emits an empty map, and
// an empty map committed once makes `--check` agree with itself forever.
const floors = checkFloors('build-route-wiring-map', [
  { subject: `route files walked under ${ROUTES}`, observed: rows.length, floor: MIN_ROUTES },
]);
for (const line of floors.lines) console.log(line);
if (!floors.ok) {
  console.error(floors.error);
  process.exit(5);
}

const json = `${JSON.stringify(out, null, 2)}\n`;
if (process.argv.includes('--check')) {
  const cur = existsSync(OUT) ? readFileSync(OUT, 'utf8') : '';
  if (cur !== json) {
    console.error(`${OUT} is stale — re-run: node scripts/build-route-wiring-map.mjs`);
    process.exit(1);
  }
  console.log(`${OUT} up to date (${rows.length} routes).`);
} else {
  writeFileSync(OUT, json);
  console.log(`Wrote ${OUT}: ${rows.length} routes`, byStatus);
}
