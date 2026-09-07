// Combines the extraction outputs (steps 1–7) into apps/elitea-web/parity/
// manifest.json. Deterministic: same baseline -> byte-identical manifest.
// Usage: node build-manifest.mjs <output-path>
import fs from 'node:fs';
import path from 'node:path';
import { BASELINE, DOMAIN_UNIT, OUT, UNIT_COVERAGE, read, src } from './common.mjs';

const OUTPUT = process.argv[2];
if (!OUTPUT) {
  console.error('usage: node build-manifest.mjs <output-path>');
  process.exit(2);
}
const COMMIT = 'a55f36cfb5ecb3834bb00bbc8d9cd9a1393168af';

const load = n => JSON.parse(fs.readFileSync(path.join(OUT, n), 'utf8'));
const routes = load('routes.json');
const queryparams = load('queryparams.json');
const { endpoints, rawTransports } = load('api.json');
const sockets = load('sockets.json');
const permissions = load('permissions.json');
const actions = load('actions.json');
const copy = load('copy.json');
const shell = load('shell.json');

const items = [];
const idset = new Set();
function push(item) {
  if (idset.has(item.id)) throw new Error(`duplicate id ${item.id}`);
  idset.add(item.id);
  items.push(item);
}
const pad = n => String(n).padStart(3, '0');
const human = s =>
  s
    .replace(/([a-z0-9])([A-Z])/g, '$1 $2')
    .replace(/[_-]+/g, ' ')
    .toLowerCase()
    .trim();
const cov = unit => {
  const c = UNIT_COVERAGE[unit];
  if (!c) throw new Error(`no coverage anchor for unit ${unit}`);
  return { file: c.file, min: c.min };
};

// ---------------------------------------------------------------- step 1: routes
const ROUTE_DOMAIN = p => {
  if (p.startsWith('/chat')) return 'chat';
  if (p.startsWith('/agents')) return 'agents';
  if (p.startsWith('/pipelines')) return 'pipelines';
  if (p.startsWith('/skills')) return 'skills';
  if (p.startsWith('/toolkits')) return 'toolkits';
  if (p.startsWith('/mcps') || p === '/mcp-auth-callback') return 'mcps';
  if (p.startsWith('/apps')) return 'apps';
  if (p.startsWith('/credentials') || p.startsWith('/settings/create-configuration') || p.startsWith('/settings/edit-configuration') || p === '/settings/model-configuration') return 'credentials';
  if (p.startsWith('/artifacts')) return 'artifacts';
  if (p.startsWith('/user-public')) return 'public';
  if (p === '/settings/secrets') return 'secrets';
  if (p === '/settings/users') return 'users';
  if (p === '/settings/tokens' || p === '/settings/create-personal-token') return 'tokens';
  if (p === '/settings/notifications') return 'notifications';
  if (p === '/settings/analytics') return 'analytics';
  return 'shell';
};
for (const r of routes) {
  const unit = ['ROUTE-070', 'ROUTE-071', 'ROUTE-074', 'ROUTE-075'].includes(r.id) ? 'R3' : 'R1';
  const humanTarget = human(r.target.replace(/\(.*\)/, '')).trim();
  const acceptance = r.anomaly
    ? [
        `GIVEN the application is running with the baseline route table`,
        `WHEN the browser navigates to ${r.pattern}`,
        `THEN the latent baseline behaviour is reproduced exactly: ${r.note}`,
        `AND no redirect, 404 page or new screen is introduced for this path`,
      ]
    : [
        `GIVEN an authenticated user with a selected project`,
        `WHEN the browser navigates to a URL matching ${r.pattern}`,
        `THEN the ${humanTarget} screen is displayed at that URL`,
        ...(r.note ? [`AND ${r.note}`] : [`AND the location, parameters and history behave exactly as in the baseline application`]),
      ];
  push({
    id: r.id,
    domain: ROUTE_DOMAIN(r.pattern),
    kind: 'route',
    priority: 'must',
    title: r.anomaly
      ? `Route anomaly \`${r.pattern}\` — declared but not mounted, reproduced bug-for-bug`
      : `Route \`${r.pattern}\` renders ${r.target}${r.specId !== r.id ? ` (spec §8.1 id ${r.specId})` : ''}`,
    source: r.sources,
    acceptance,
    verify: {
      type: 'command',
      command: 'go run ./tools/uictl parity-routes --baseline apps/elitea-ui',
      testId: r.id,
    },
    unit,
    status: 'todo',
    coverage: cov(unit),
    waiver: null,
  });
}

// ---------------------------------------------------------- step 2: query params
const QP_PURPOSE = {
  createSecret: 'the create-secret modal opens',
  inviteUsers: 'the invite-users modal opens',
  create: 'the generic create panel opens',
  tour: 'the referenced interactive tour starts',
  history_run_id: 'the run-history detail opens',
  destTab: 'the named tab (History) is activated',
  edited_participant_id: 'the participant editor opens for that participant',
  index_name: 'the index detail view opens',
  file: 'the file preview opens inside the bucket',
  folder: 'the folder is opened inside the bucket',
  bucket: 'the bucket is selected',
  auth_state: 'the auth popup result is validated against the stored state value',
};
let qpN = 0;
// expand per route, then DEDUPE by (key × route): two screens whose route
// scope normalises to the same value (e.g. 'any' and 'any (shell)') are one
// scope, so one item — sites/ops/spec-ids merge.
const qpMerged = new Map();
for (const q of queryparams) {
  for (const rawRoute of q.routes.split(', ')) {
    const route = rawRoute.startsWith('any') ? 'any' : rawRoute;
    const k = `${q.key}||${route}`;
    if (!qpMerged.has(k)) {
      qpMerged.set(k, {
        key: q.key, route, screens: new Set(), domains: new Set(),
        ops: new Set(), sites: [], specIds: new Set(),
      });
    }
    const rec = qpMerged.get(k);
    rec.screens.add(q.screen);
    rec.domains.add(q.domain);
    for (const o of q.ops) rec.ops.add(o);
    for (const s of q.sites) if (!rec.sites.includes(s)) rec.sites.push(s);
    for (const s of q.specIds) rec.specIds.add(s);
  }
}
const qpExpanded = [...qpMerged.values()].map(r => ({
  ...r,
  screen: r.screens.size === 1 ? [...r.screens][0] : 'shared',
  domain:
    r.domains.size === 1
      ? [...r.domains][0]
      : [...r.domains].filter(d => d !== 'shell').length === 1
        ? [...r.domains].filter(d => d !== 'shell')[0]
        : 'shell',
  ops: [...r.ops].sort(),
  specIds: [...r.specIds].sort(),
}));
qpExpanded.sort((a, b) => a.screen.localeCompare(b.screen) || a.key.localeCompare(b.key) || a.route.localeCompare(b.route));
for (const q of qpExpanded) {
  qpN += 1;
  const purpose = QP_PURPOSE[q.key] || 'the corresponding panel or view state is applied';
  const writes = q.ops.some(o => o === 'set' || o === 'delete');
  const linkOnly = q.ops.every(o => o === 'link');
  const acceptance = linkOnly
    ? [
        `GIVEN the feature that generates links carrying the ${q.key} parameter`,
        `WHEN such a link is produced`,
        `THEN the URL contains ${q.key} with the same value semantics as the baseline`,
        `AND this is a URL contract: the parameter shape must be preserved even though no in-app reader consumes it, because external consumers and shared links depend on it`,
      ]
    : [
        `GIVEN the application is at ${q.route}`,
        `WHEN the URL contains the ${q.key} query parameter with a valid value`,
        `THEN ${purpose} on cold load, without a full page reload`,
        writes
          ? `AND the same state change performed by interaction updates the URL parameter accordingly`
          : `AND removing the parameter returns the screen to its default state`,
        `AND a malformed value is rejected without crashing the screen`,
      ];
  push({
    id: `PARAM-${pad(qpN)}`,
    domain: q.domain,
    kind: 'behaviour',
    priority: 'must',
    title: `Query param \`${q.key}\` on ${q.route}${q.specIds.length ? ` (spec ${q.specIds.join(', ')})` : ''}${linkOnly ? ' [write-only URL contract]' : ''}`,
    source: q.sites,
    acceptance,
    verify: {
      type: 'vitest',
      command: `npm run test:unit -- src/routes/__tests__/searchParams.${q.screen}.test.tsx`,
      testId: `search-params > ${q.screen} > ${q.key}`,
    },
    unit: 'R1',
    status: 'todo',
    coverage: cov('R1'),
    waiver: null,
  });
}

// ------------------------------------------------------------ step 3: endpoints
const sortedEndpoints = [...endpoints].sort(
  (a, b) => a.module.localeCompare(b.module) || a.line - b.line,
);
// The two decided contract corrections (decision record, "Contract
// corrections from unit W1", waivers W-008 / W-009):
const UNPREFIXED_S3 = new Set(['bucketList', 'artifactList']); // raw fetch, NOT /api/v2-prefixed
let apiN = 0;
for (const e of sortedEndpoints) {
  apiN += 1;
  const isMutation = e.type === 'mutation';
  let item;
  if (e.endpoint === 'stopApplicationTask') {
    // W-008: dead surface — zero importers in the old SPA; live stop paths
    // use the served stop-task route. Deliberately NOT ported.
    //
    // The ROUTE came back with issue 254 P2 (GET polls, DELETE stops, both in
    // internal/api/v2/agentexecution). The waiver did not: it records that the
    // new app has no caller for it, which is still true. The wording below no
    // longer claims the router refuses the path.
    item = {
      id: `API-${pad(apiN)}`,
      domain: e.domain, kind: 'integration', priority: 'waived',
      title: `Endpoint ${e.method} ${e.url} (${e.endpoint}) — dead surface, not ported (waiver W-008)`,
      source: [src(e.module, e.line, e.endLine)],
      acceptance: [
        `GIVEN the old client declares a stop-application-task request that no component ever invokes`,
        `WHEN the reimplementation ships`,
        `THEN this request is not ported: it is dead surface with no caller in the new app`,
        `AND live task stopping continues to work through the served stop-task route, which has its own manifest item`,
      ],
      verify: {
        type: 'command',
        command: 'go run ./tools/uictl parity-manifest --validate',
        testId: `waived > W-008 > ${e.endpoint}`,
      },
      unit: e.unit, status: 'waived', coverage: cov(e.unit),
      waiver: {
        reason: 'W-008: dead surface — endpoint has zero importers in the old SPA and none in the new app; live stop paths use the served stop-task route. The Go router serves the path again as of issue 254 P2 (internal/api/v2/agentexecution), so the waiver is about the UI, not about the route',
        decidedBy: 'unit W1 contract correction (decision record 2026-07-26, flagged for operator veto)',
        date: '2026-07-26',
        replacesBehaviour: 'old SPA declared DELETE /elitea_core/application_task/prompt_lib/{projectId}/{taskId} but never called it',
      },
    };
  } else if (e.endpoint === 'setApplicationDefaultVersion') {
    // W-009: the old SPA's 2-segment call with the version in the body is a
    // 405 against the Go router (live-broken today); the new app calls the
    // WORKING 3-segment shape. Acceptance codifies the working shape.
    const workingURL = '/elitea_core/default_version/prompt_lib/{projectId}/{applicationId}/{versionId}';
    item = {
      id: `API-${pad(apiN)}`,
      domain: e.domain, kind: 'integration', priority: 'must',
      title: `Endpoint PATCH ${workingURL} (${e.endpoint}) — router-aligned shape (waiver W-009)`,
      source: [src(e.module, e.line, e.endLine)],
      acceptance: [
        `GIVEN the user sets an application version as the default`,
        `WHEN the client issues the request`,
        `THEN it targets PATCH ${workingURL} — the three-segment shape the router actually serves (waiver W-009)`,
        `AND the old client's two-segment call carrying the version in the body is NOT reproduced: it is rejected with a method-not-allowed error by the served router, i.e. set-default-version is live-broken in the baseline today`,
        `AND on success the version list shows the new default without a manual refresh`,
      ],
      verify: {
        type: 'vitest',
        command: `npm run test:integration -- src/shared/api/__tests__/${path.basename(e.module).replace(/\.js$/, '')}.contract.test.ts`,
        testId: `api > ${path.basename(e.module)} > ${e.endpoint}`,
      },
      unit: e.unit, status: 'todo', coverage: cov(e.unit), waiver: null,
    };
  } else {
    const acceptance = [
      `GIVEN the user performs the ${human(e.endpoint)} operation`,
      `WHEN the client issues the request`,
      `THEN it targets ${e.method} ${e.url} with the same parameters, body shape and semantics as the baseline`,
      isMutation
        ? `AND on success the affected lists and detail views reflect the change without a manual refresh, while failures surface an error state`
        : `AND the response payload drives the same visible data as in the baseline, with error responses surfaced as an error state instead of a blank screen`,
    ];
    if (UNPREFIXED_S3.has(e.endpoint)) {
      acceptance.push(
        `AND the request goes to the UN-PREFIXED /artifacts/s3/ path — deliberately not under the /api/v2 base path, because the deployment routes /artifacts/ straight to the backend; this exact un-prefixed shape is a real routing fact and must be preserved`,
      );
    }
    // opaque shapes ({url}, {base}?…) are dynamic path templates too
    const opaqueURL = e.dynamic || e.url.includes('{url}') || e.url.includes('{base}') || e.url.includes('|');
    item = {
      id: `API-${pad(apiN)}`,
      domain: e.domain, kind: 'integration', priority: 'must',
      title: `Endpoint ${e.method} ${e.url} (${e.endpoint})${opaqueURL ? ' [dynamic path template]' : ''}`,
      source: [src(e.module, e.line, e.endLine)],
      acceptance,
      verify: {
        type: 'vitest',
        command: `npm run test:integration -- src/shared/api/__tests__/${path.basename(e.module).replace(/\.js$/, '')}.contract.test.ts`,
        testId: `api > ${path.basename(e.module)} > ${e.endpoint}`,
      },
      unit: e.unit, status: 'todo', coverage: cov(e.unit), waiver: null,
    };
  }
  push(item);
}
// §5.7 raw transports
const XPORT_UNIT = m =>
  m.includes('mcp') ? 'A5' : m.includes('tracing') || m.includes('Trace') ? 'F4' : 'S6';
let xpN = 0;
for (const t of [...rawTransports].sort((a, b) => a.module.localeCompare(b.module) || a.line - b.line)) {
  xpN += 1;
  const unit = XPORT_UNIT(t.module);
  const what =
    t.transport === 'XMLHttpRequest'
      ? 'a chunked upload request with progress reporting'
      : t.transport === 'axios'
        ? `a direct ${t.method || 'PUT'} upload request`
        : `a raw fetch request${t.url && !t.url.startsWith('{') ? ` to ${t.url}` : ''}`;
  const opaque = !t.url || t.dynamic || t.url.startsWith('{');
  push({
    id: `XPORT-${pad(xpN)}`,
    domain: t.module.includes('mcp') ? 'mcps' : t.module.includes('upload') || t.module.includes('slices') ? 'chat' : t.module.includes('artifact') || t.module.includes('utils') ? 'artifacts' : 'shell',
    kind: 'integration',
    priority: 'must',
    title: `Non-REST transport: ${t.transport} at ${t.module.split('/').pop()}:${t.line}${opaque ? ' [dynamic path template]' : ''}`,
    source: [src(t.module, t.line)],
    acceptance: [
      `GIVEN the feature served by this transport is used`,
      `WHEN the browser performs ${what}`,
      `THEN the request shape, credential mode, URL prefix handling and progress or streaming behaviour match the baseline exactly`,
      `AND failures are reported to the user rather than silently dropped`,
    ],
    verify: {
      type: 'vitest',
      command: 'npm run test:unit -- src/shared/api/upload.test.ts src/shared/api/artifacts.test.ts',
      testId: `transport > ${t.module.split('/').pop()}:${t.line}`,
    },
    unit,
    status: 'todo',
    coverage: cov(unit),
    waiver: null,
  });
}

// -------------------------------------------------------------- step 4: sockets
const SOCK_DOMAIN = n =>
  n.startsWith('notifications') ? 'notifications' : n.startsWith('mcp') || n === 'test_mcp_connection' ? 'mcps' : 'chat';
let sockN = 0;
for (const s of sockets) {
  sockN += 1;
  const isEvent = s.kind === 'event';
  const acceptance = isEvent
    ? s.direction === 'emit' || s.direction === 'both'
      ? [
          `GIVEN an active socket connection`,
          `WHEN the user action associated with ${s.name} occurs`,
          `THEN the client emits ${s.name} with the payload shape the server expects`,
          `AND server responses or follow-up events for this exchange are handled without error`,
        ]
      : [
          `GIVEN an active socket connection on a screen that consumes ${s.name}`,
          `WHEN the server emits ${s.name}`,
          `THEN the visible state updates accordingly without a refetch`,
          `AND a malformed payload is logged and ignored rather than crashing the client`,
        ]
    : [
        `GIVEN a streaming run is in progress`,
        `WHEN a streamed message with type ${s.name} arrives`,
        `THEN the message is applied to the conversation or run view according to its meaning in the baseline`,
        `AND the stream continues uninterrupted afterwards`,
      ];
  push({
    id: `SOCK-${pad(sockN)}`,
    domain: SOCK_DOMAIN(s.name),
    kind: 'integration',
    priority: 'must',
    title: isEvent
      ? `Socket event \`${s.name}\` (${s.direction})`
      : `Streaming payload discriminant \`${s.name}\``,
    source: [src('src/common/constants.js', s.line), ...s.sites.slice(0, 3)],
    acceptance,
    verify: {
      type: 'vitest',
      command: 'npm run test:unit -- src/shared/api/socket',
      testId: `socket > ${s.kind} > ${s.name}`,
    },
    unit: 'S5',
    status: 'todo',
    coverage: cov('S5'),
    waiver: null,
  });
}

// ---------------------------------------------------------- step 5: permissions
let permN = 0;
for (const p of permissions) {
  permN += 1;
  const id = `PERM-${pad(permN)}`;
  if (p.kind === 'permission-string') {
    const unit = DOMAIN_UNIT[p.domain] || 'W-shell';
    push({
      id, domain: p.domain, kind: 'permission', priority: 'must',
      title: `Permission \`${p.permission}\` gates its UI`,
      source: p.sources,
      acceptance: [
        `GIVEN a user whose project role does not include ${p.permission}`,
        `WHEN the screens that reference this permission render`,
        `THEN the gated controls, tabs or routes are hidden or disabled exactly as in the baseline`,
        `AND granting the permission makes them available without any other change`,
      ],
      verify: {
        type: 'vitest',
        command: 'npm run test:unit -- --grep permissions',
        testId: `permissions > ${p.permission}`,
      },
      unit, status: 'todo', coverage: cov(unit), waiver: null,
    });
  } else if (p.kind === 'permission-group') {
    push({
      id, domain: 'shell', kind: 'permission', priority: 'must',
      title: `Sidebar permission group \`${p.group}\` requires [${p.members.join(', ')}]`,
      source: p.sources,
      acceptance: [
        `GIVEN a user with none of the permissions [${p.members.join(', ')}]`,
        `WHEN the sidebar renders`,
        `THEN the ${p.group} navigation entry is hidden`,
        `AND it appears when at least one of the listed permissions is granted`,
      ],
      verify: {
        type: 'vitest',
        command: 'npm run test:unit -- src/widgets',
        testId: `permissions > group > ${p.group}`,
      },
      unit: 'W-shell', status: 'todo', coverage: cov('W-shell'), waiver: null,
    });
  } else if (p.kind === 'guard') {
    push({
      id, domain: p.domain, kind: 'permission', priority: 'must',
      title: `Route guard ${p.guard}`,
      source: p.sources,
      acceptance: [
        `GIVEN the guard condition for ${p.guard} applies`,
        `WHEN a guarded route is visited`,
        `THEN ${p.behaviour}`,
        `AND when the condition does not apply the guarded content renders normally`,
      ],
      verify: {
        type: 'vitest',
        command: 'npm run test:unit -- src/routes',
        testId: `guards > ${p.guard}`,
      },
      unit: 'R1', status: 'todo', coverage: cov('R1'), waiver: null,
    });
  } else if (p.kind === 'platform-flag') {
    push({
      id, domain: 'mcps', kind: 'permission', priority: 'must',
      title: `Platform flag \`${p.flag}\` controls MCP visibility`,
      source: p.sources,
      acceptance: [
        `GIVEN a deployment where ${p.flag} is disabled`,
        `WHEN the application loads`,
        `THEN the MCP menu entries and MCP routes governed by this flag are hidden`,
        `AND enabling the flag makes them visible without a rebuild`,
      ],
      verify: {
        type: 'vitest',
        command: 'npm run test:unit -- src/widgets',
        testId: `flags > ${p.flag}`,
      },
      unit: 'W-shell', status: 'todo', coverage: cov('W-shell'), waiver: null,
    });
  }
}

// -------------------------------------------------------------- step 6: actions
let actN = 0;
for (const a of actions) {
  actN += 1;
  const isSock = a.endpoint.startsWith('sio:');
  const unit = DOMAIN_UNIT[a.domain] || 'W-shell';
  const dispatchDesc = isSock
    ? `the socket event ${a.url} is emitted`
    : `a ${a.method} request to ${a.url} is dispatched`;
  push({
    id: `ACT-${pad(actN)}`,
    domain: a.domain,
    kind: 'behaviour',
    priority: 'must',
    title: `Action on ${a.screen}: ${isSock ? `socket ${a.url}` : human(a.endpoint)} via ${a.handlerProps.join('/')}`,
    source: [...a.sites.slice(0, 4), src(a.defLine.module, a.defLine.line)],
    acceptance: [
      `GIVEN the ${a.screen} screen is displayed with the relevant entity available`,
      `WHEN the user activates the control wired to ${isSock ? `the ${a.url} exchange` : human(a.endpoint)}`,
      `THEN ${dispatchDesc}`,
      `AND the visible outcome matches the baseline: success updates the view, failure surfaces an error without losing user input`,
    ],
    verify: {
      type: 'vitest',
      command: `npm run test:integration -- --grep "actions ${a.screen}"`,
      testId: `action > ${a.screen} > ${a.endpoint}`,
    },
    unit,
    status: 'todo',
    coverage: cov(unit),
    waiver: null,
  });
}

// ----------------------------------------------------------------- step 7: copy
let copyN = 0;
for (const c of copy) {
  copyN += 1;
  const sample = (c.samples[0] || '').replace(/"/g, "'").slice(0, 60);
  // aria-only files: a visual-regression suite cannot see assistive-tech
  // labels, so these are verified by the storybook a11y suite instead.
  const acceptance = c.ariaOnly
    ? [
        `GIVEN the ${c.screen} screen renders`,
        `WHEN its accessibility labels are inspected with assistive technology`,
        `THEN all ${c.count} assistive-technology strings from this component (for example "${sample}") match the baseline verbatim`,
        `AND every labelled control remains reachable and announced by a screen reader`,
      ]
    : [
        `GIVEN the ${c.screen} screen renders with the default brand in both colour schemes`,
        `WHEN its user-visible texts are compared with the baseline application`,
        `THEN all ${c.count} strings from this component (for example "${sample}") appear verbatim`,
        `AND no string is dropped, truncated or reworded`,
      ];
  push({
    id: `COPY-${pad(copyN)}`,
    domain: c.domain,
    kind: 'visual',
    priority: 'should',
    title: `Copy: ${c.file.split('/').slice(-2).join('/')} (${c.count} strings)${c.ariaOnly ? ' [aria-only]' : ''}`,
    source: c.sources,
    acceptance,
    verify: c.ariaOnly
      ? {
          type: 'command',
          command: 'npm run test:storybook',
          testId: `a11y > ${c.screen} > ${c.file.split('/').pop()}`,
        }
      : {
          type: 'playwright',
          command: 'npx playwright test --grep @visual',
          testId: `visual > ${c.screen} > ${c.file.split('/').pop()}`,
        },
    unit: c.ariaOnly ? 'S1' : 'V2',
    status: 'todo',
    coverage: cov(c.ariaOnly ? 'S1' : 'V2'),
    waiver: null,
  });
}

// ------------------------------------------------------------------ shell items
let shellN = 0;
for (const s of shell) {
  shellN += 1;
  const id = `SHELL-${pad(shellN)}`;
  let title, acceptance, waiver = null, priority = 'must';
  switch (s.kind) {
    case 'sidebar-entry':
      title = `Sidebar entry "${s.label}" (group ${s.group})`;
      acceptance = [
        `GIVEN a user whose permissions allow the ${s.value} area`,
        `WHEN the sidebar renders`,
        `THEN the "${s.label}" entry appears in group ${s.group} and navigates to its screen with the matching breadcrumb`,
        `AND the entry is absent when its permission group or visibility rule excludes it`,
      ];
      break;
    case 'sidebar-filtering':
      title = 'Sidebar groups are permission-filtered';
      acceptance = [
        `GIVEN users with differing permission sets`,
        `WHEN the sidebar renders`,
        `THEN each of the three groups shows only the entries whose permission group is satisfied, skills are hidden in the public project, and MCPs are hidden when the platform flags disable them`,
        `AND groups left empty by filtering disappear entirely`,
      ];
      break;
    case 'deferred-cache-reset':
      title = 'Sidebar navigation defers API cache reset while streaming';
      acceptance = [
        `GIVEN a chat response is currently streaming or navigation is blocked by unsaved changes`,
        `WHEN the user navigates from the sidebar`,
        `THEN the cached API state is not reset immediately but flagged for reset once streaming completes`,
        `AND without an active stream the cache is reset at navigation time, exactly as in the baseline`,
      ];
      break;
    case 'socket-indicator':
      title = 'Sidebar socket connectivity indicator (SHELL-012)';
      acceptance = [
        `GIVEN the realtime socket connection drops`,
        `WHEN the connection state changes`,
        `THEN the sidebar shows the connectivity indicator reflecting the current status`,
        `AND on reconnect the indicator returns to the connected state and previously joined rooms are usable again`,
      ];
      break;
    case 'create-entity':
      title = `Global Create option "${s.label}"`;
      acceptance = [
        `GIVEN a user holding ${s.permissions && s.permissions.length ? `the ${s.permissions.join(' and ')} permission(s)` : 'any role (no specific permission is required)'}`,
        `WHEN the global Create control is opened`,
        `THEN the "${s.label}" option is available and leads to the ${s.option} creation flow with the correct breadcrumb and back path`,
        `AND the option is hidden when the required permissions are missing`,
      ];
      break;
    case 'simple-create-routes':
      title = 'Global Create suppressed on simple routes';
      acceptance = [
        `GIVEN the user is on any of the designated simple routes (${s.routes.join(', ')})`,
        `WHEN the shell header renders`,
        `THEN the global Create control is suppressed`,
        `AND it reappears when navigating to any other route`,
      ];
      break;
    case 'feedback-dialog':
      priority = 'waived';
      waiver = {
        reason: 'ship dormant feature',
        decidedBy: 'Alexander Kharkevich',
        date: '2026-07-26',
        replacesBehaviour: 'dialog hidden (commented out of sidebar)',
      };
      title = 'Feedback dialog ships ENABLED (waiver W-007, decision D5)';
      acceptance = [
        `GIVEN a user holding the social feedback creation permission`,
        `WHEN the sidebar renders in the new application`,
        `THEN a feedback entry is present and opens the feedback dialog`,
        `AND this deliberately deviates from the baseline, where the fully implemented dialog is commented out of the sidebar`,
      ];
      break;
    default:
      throw new Error(`unknown shell kind ${s.kind}`);
  }
  push({
    id, domain: 'shell', kind: 'shell', priority, title,
    source: s.sources,
    acceptance,
    verify: {
      type: 'vitest',
      command: 'npm run test:unit -- src/widgets',
      testId: `shell > ${s.kind}${s.label ? ` > ${s.label}` : ''}`,
    },
    unit: 'W-shell', status: 'todo', coverage: cov('W-shell'), waiver,
  });
}

// ------------------------------------------------------- §8.5 journeys (30)
// Anchors are resolved mechanically against the extraction outputs; the
// journey list itself is spec-authored (like the ROUTE ids).
const epByName = new Map(endpoints.map(e => [e.endpoint, e]));
const sockByName = new Map(sockets.map(s => [s.name, s]));
const routeById = new Map(routes.map(r => [r.id, r]));
function grepLine(file, needle) {
  const lines = read(file).split('\n');
  const i = lines.findIndex(l => l.includes(needle));
  if (i < 0) throw new Error(`journey anchor not found: ${file} :: ${needle}`);
  return src(file, i + 1);
}
const anchorsOf = j => {
  const out = [];
  for (const rid of j.routes || []) out.push(...routeById.get(rid).sources.slice(0, 1));
  for (const en of j.endpoints || []) {
    const e = epByName.get(en);
    if (!e) throw new Error(`journey endpoint ${en} unknown`);
    out.push(src(e.module, e.line));
  }
  for (const sn of j.socks || []) out.push(src('src/common/constants.js', sockByName.get(sn).line));
  for (const [f, n] of j.greps || []) out.push(grepLine(f, n));
  if (!out.length) throw new Error(`journey ${j.n} has no anchors`);
  return out;
};
const JOURNEYS = [
  { n: 1, t: 'Cold load / resolves through the redirect chain to /chat', routes: ['ROUTE-003'], domain: 'shell',
    steps: ['GIVEN an authenticated user with a personal project', 'WHEN the application is cold-loaded at /', 'THEN the user ends on the chat screen via the documented redirect chain', 'AND a user without a personal project lands on onboarding instead'] },
  { n: 2, t: 'Login via OIDC honours target_to', greps: [['src/api/eliteaApi.js', 'target_to']], domain: 'shell',
    steps: ['GIVEN an unauthenticated user deep-links into the application', 'WHEN the OIDC login flow completes', 'THEN the user is returned to the originally requested location', 'AND the auth redirect parameter is stripped from the final URL'] },
  { n: 3, t: 'Session expiry mid-request triggers re-auth popup and retry', greps: [['src/api/eliteaApi.js', 'openAuthPopup']], domain: 'shell',
    steps: ['GIVEN the session expires while the app is open', 'WHEN a request is answered with a login redirect', 'THEN a re-authentication popup opens once, even for concurrent failures', 'AND the original request is retried and succeeds after re-auth'] },
  { n: 4, t: 'Logout clears user state and storage', greps: [['src/slices/user.js', 'logout']], domain: 'shell',
    steps: ['GIVEN a logged-in user', 'WHEN the user logs out', 'THEN the user state is cleared and the login screen is reached', 'AND all application storage keys are removed'] },
  { n: 5, t: 'Deep link to an agent version cold-loads', routes: ['ROUTE-012', 'ROUTE-067'], domain: 'agents',
    steps: ['GIVEN a shared deep link to a specific agent version', 'WHEN it is opened in a fresh session', 'THEN the agent detail opens on that version', 'AND the URL is preserved'] },
  { n: 6, t: 'Share link with project id switches project and reloads', routes: ['ROUTE-070'], domain: 'shell',
    steps: ['GIVEN a share link whose first segment is a project id', 'WHEN it is opened', 'THEN the active project switches and the page hard-reloads at the same path with the project segment stripped', 'AND the target entity then loads in the switched project'] },
  { n: 7, t: 'Project switch from the sidebar', endpoints: ['projectList'], domain: 'shell',
    steps: ['GIVEN a user who is a member of several projects', 'WHEN a different project is chosen from the sidebar switcher', 'THEN all project-scoped data reloads for the new project', 'AND the selection persists across a reload'] },
  { n: 8, t: 'Create conversation, send message, stream tokens, stop', endpoints: ['conversationCreate', 'stopChatTask'], socks: ['chat_predict'], domain: 'chat',
    steps: ['GIVEN the chat screen', 'WHEN the user creates a conversation and sends a message', 'THEN response tokens stream into the conversation live', 'AND the stop control interrupts the stream and the partial answer is kept'] },
  { n: 9, t: 'Regenerate a response', endpoints: ['regenerate'], domain: 'chat',
    steps: ['GIVEN a conversation with a completed assistant response', 'WHEN the user triggers regenerate', 'THEN a new response streams in and replaces the previous one', 'AND the conversation history stays consistent'] },
  { n: 10, t: 'Create folder, drag conversation into it, reorder', endpoints: ['folderCreate', 'folderUpdate'], domain: 'chat',
    steps: ['GIVEN the conversation list', 'WHEN the user creates a folder and drags a conversation into it', 'THEN the conversation is grouped under the folder and reordering persists', 'AND the ordering survives a reload'] },
  { n: 11, t: 'Server-side conversation rename live-updates', socks: ['chat_conversation_name_updated'], domain: 'chat',
    steps: ['GIVEN an open unnamed conversation', 'WHEN the server pushes a generated name', 'THEN the list entry, header and folder grouping update live without a refetch', 'AND the URL is unchanged'] },
  { n: 12, t: 'Attach a small file to a chat message', endpoints: ['uploadAttachments'], domain: 'chat',
    steps: ['GIVEN the chat composer', 'WHEN the user attaches a file under the chunking threshold', 'THEN the attachment uploads and appears with the message', 'AND upload failures are surfaced'] },
  { n: 13, t: 'Attach a large file (chunked upload with progress)', greps: [['src/hooks/chat/useUploadWithProgress.js', 'XMLHttpRequest']], domain: 'chat',
    steps: ['GIVEN the chat composer', 'WHEN the user attaches a file above the chunk threshold', 'THEN it uploads in chunks with visible progress and intermediate in-progress responses', 'AND the completed attachment is usable in the conversation'] },
  { n: 14, t: 'Create agent, save, publish, unpublish', endpoints: ['applicationCreate', 'publishApplication', 'unpublishApplication'], domain: 'agents',
    steps: ['GIVEN the create-agent flow', 'WHEN the user saves, publishes and later unpublishes the agent', 'THEN each state change is reflected in the lists and detail view', 'AND publish validation failures are surfaced'] },
  { n: 15, t: 'Create a new version, set default, delete old', endpoints: ['saveApplicationNewVersion', 'setApplicationDefaultVersion', 'deleteApplicationVersion'], domain: 'agents',
    steps: ['GIVEN an agent with an existing version', 'WHEN the user saves a new version, makes it the default and deletes the old one', 'THEN the version list and default marker update accordingly', 'AND deleting a version still in use is prevented with a clear message'] },
  { n: 16, t: 'Create pipeline, edit flow graph, save', endpoints: ['applicationCreate'], routes: ['ROUTE-018'], domain: 'pipelines',
    steps: ['GIVEN the create-pipeline flow', 'WHEN the user edits the flow graph and saves', 'THEN the saved pipeline reloads with the same graph', 'AND validation errors block saving with a visible reason'] },
  { n: 17, t: 'Create toolkit, configure, test connection', endpoints: ['toolkitCreate', 'toolkitTest'], domain: 'toolkits',
    steps: ['GIVEN the create-toolkit flow', 'WHEN the user configures the toolkit and runs the connection test', 'THEN the test result is displayed', 'AND the saved toolkit appears in the list'] },
  { n: 18, t: 'Create MCP with OAuth callback round trip', endpoints: ['exchangeMcpOAuthToken'], routes: ['ROUTE-050'], domain: 'mcps',
    steps: ['GIVEN the create-MCP flow requiring OAuth', 'WHEN the OAuth provider redirects back to the callback route', 'THEN the token exchange completes and the MCP becomes usable', 'AND OAuth errors are shown on the callback screen'] },
  { n: 19, t: 'Create credential and use it in an agent', endpoints: ['createConfiguration'], domain: 'credentials',
    steps: ['GIVEN the create-credential flow', 'WHEN the user creates a credential and selects it inside an agent configuration', 'THEN the credential is persisted and selectable', 'AND removing it invalidates dependent configuration gracefully'] },
  { n: 20, t: 'Artifacts bucket lifecycle: create, upload, preview, download, ZIP, delete', endpoints: ['createBucket', 'createArtifact', 'deleteArtifact'], routes: ['ROUTE-048'], domain: 'artifacts',
    steps: ['GIVEN the artifacts screen', 'WHEN the user creates a bucket, uploads a file, previews and downloads it, multi-downloads as ZIP and deletes it', 'THEN each step behaves as in the baseline including the direct storage upload', 'AND errors at any step are surfaced without corrupting the bucket view'] },
  { n: 21, t: 'Settings: create secret', endpoints: ['secretAdding'], routes: ['ROUTE-058'], domain: 'secrets',
    steps: ['GIVEN the secrets settings tab', 'WHEN the user creates a secret', 'THEN it appears in the list with its value hidden', 'AND the create modal is reachable directly by URL'] },
  { n: 22, t: 'Settings: invite user and change role', endpoints: ['userCreate', 'userUpdate'], routes: ['ROUTE-059'], domain: 'users',
    steps: ['GIVEN the users settings tab', 'WHEN the user invites a member and changes their role', 'THEN the member list reflects both changes', 'AND the invite modal is reachable directly by URL'] },
  { n: 23, t: 'Settings: create personal token', endpoints: ['tokenCreate'], routes: ['ROUTE-066'], domain: 'tokens',
    steps: ['GIVEN the personal tokens tab', 'WHEN the user creates a token', 'THEN the token value is shown once and the list updates', 'AND navigation away with unsaved input is blocked'] },
  { n: 24, t: 'Settings: analytics loads', routes: ['ROUTE-060'], domain: 'analytics',
    steps: ['GIVEN the analytics settings tab', 'WHEN it loads', 'THEN the dashboards render with project data', 'AND loading failures show an error state instead of empty charts'] },
  { n: 25, t: 'Unsaved-changes navigation block', routes: ['ROUTE-012'], domain: 'agents',
    steps: ['GIVEN a dirty agent form', 'WHEN the user navigates away', 'THEN a confirmation dialog appears and cancel keeps the user on the page with state intact', 'AND confirming leaves without saving'] },
  { n: 26, t: 'Socket disconnect indicator and room rejoin', socks: ['chat_enter_room'], domain: 'chat',
    steps: ['GIVEN an open conversation with a live socket', 'WHEN the socket disconnects and reconnects', 'THEN the sidebar indicator reflects each state', 'AND rooms are rejoined so live updates resume'] },
  { n: 27, t: 'Admin users screen loads with server-injected config', endpoints: ['userList'], domain: 'admin',
    steps: ['GIVEN an administrator', 'WHEN the admin users screen is opened', 'THEN the user list loads under the admin base path with server-provided configuration', 'AND non-admin users cannot reach it'] },
  { n: 28, t: 'Admin role permission matrix edit', endpoints: ['roleList'], domain: 'admin',
    steps: ['GIVEN the admin roles screen', 'WHEN a permission cell is toggled and saved', 'THEN the role matrix persists the change', 'AND affected users see the permission change after reload'] },
  { n: 29, t: 'Theme switch persists across reload', greps: [['src/components/ThemeModeToggle.jsx', 'ThemeMode'], ['src/pages/UserSettings/components/ProfilePersonalization.jsx', 'ThemeMode']], domain: 'shell',
    steps: ['GIVEN the personalization settings', 'WHEN the user switches between light and dark themes and reloads', 'THEN the chosen scheme is applied after the reload', 'AND every screen renders correctly in both schemes'] },
  { n: 30, t: 'Brand pack swaps logo, primary colour and product name without rebuild', greps: [['src/MainTheme.js', 'palette']], domain: 'shell',
    steps: ['GIVEN a deployment with a tenant brand pack configured', 'WHEN the application loads', 'THEN the logo, primary colour and product name come from the pack with no rebuild', 'AND the default pack reproduces the baseline appearance byte-for-byte'] },
];
if (JOURNEYS.length !== 30) throw new Error('expected 30 journeys');
for (const j of JOURNEYS) {
  push({
    id: `JRNY-${pad(j.n)}`,
    domain: j.domain,
    kind: 'integration',
    priority: 'must',
    title: `Journey ${j.n}: ${j.t}`,
    source: anchorsOf(j),
    acceptance: j.steps,
    verify: {
      type: 'playwright',
      command: `npx playwright test e2e/journeys --grep "journey-${String(j.n).padStart(2, '0')}"`,
      testId: `journey > ${String(j.n).padStart(2, '0')} > ${j.t}`,
    },
    unit: 'V1',
    status: 'todo',
    coverage: cov('V1'),
    waiver: null,
  });
}

// -------------------------------------------------------------------- emit
// Sharded layout: parity/manifest.json is a root index (top-level §8.3
// fields + shard list); items live in parity/manifest/<domain>.json, one
// shard per §8.6 domain. Repo gate scripts/no-binaries-check.sh rejects any
// tracked file >= 1,048,576 bytes; every shard must stay well under that,
// and per-domain shards give CI a per-PR diff seam (a chat PR touches
// manifest/chat.json only).
const SHARD_LIMIT = 1000000;
const DOMAIN_ORDER = [
  'shell', 'chat', 'agents', 'pipelines', 'skills', 'toolkits', 'mcps',
  'apps', 'credentials', 'artifacts', 'indexes', 'secrets', 'users',
  'tokens', 'notifications', 'analytics', 'public', 'admin',
];
const outDir = path.dirname(OUTPUT);
const shardDir = path.join(outDir, 'manifest');
fs.mkdirSync(shardDir, { recursive: true });
// remove stale shards so renamed/dropped domains cannot linger
for (const f of fs.existsSync(shardDir) ? fs.readdirSync(shardDir) : [])
  if (f.endsWith('.json')) fs.unlinkSync(path.join(shardDir, f));

const shards = [];
for (const domain of DOMAIN_ORDER) {
  const domainItems = items.filter(i => i.domain === domain);
  if (!domainItems.length) throw new Error(`domain ${domain} has no items`);
  const shard = { $schema: '../manifest.schema.json', domain, items: domainItems };
  const body = JSON.stringify(shard, null, 2) + '\n';
  if (Buffer.byteLength(body) >= SHARD_LIMIT)
    throw new Error(`shard ${domain} is ${Buffer.byteLength(body)} bytes — split it further`);
  fs.writeFileSync(path.join(shardDir, `${domain}.json`), body);
  shards.push({ path: `manifest/${domain}.json`, domain, items: domainItems.length });
}
const known = new Set(DOMAIN_ORDER);
for (const i of items) if (!known.has(i.domain)) throw new Error(`item ${i.id} has unsharded domain ${i.domain}`);

const index = {
  $schema: './manifest.schema.json',
  version: 1,
  generatedFrom: { repo: 'apps/elitea-ui', commit: COMMIT, date: '2026-07-26' },
  shards,
};
fs.writeFileSync(OUTPUT, JSON.stringify(index, null, 2) + '\n');

const byPrefix = {};
const byDomain = {};
for (const i of items) {
  const p = i.id.replace(/-\d+$/, '');
  byPrefix[p] = (byPrefix[p] || 0) + 1;
  byDomain[i.domain] = (byDomain[i.domain] || 0) + 1;
}
console.log('total items:', items.length);
console.log('by prefix:', JSON.stringify(byPrefix));
console.log('by domain:', JSON.stringify(byDomain));
