/**
 * Decision logic for scripts/check-parity-manifest.mjs — the parts of the
 * deleted `tools/uictl parity-manifest --validate` that never needed the
 * apps/elitea-ui baseline checkout:
 *
 *   - shard bookkeeping (index vs shard domain and item count),
 *   - unknown keys refused at every level (uictl decoded with
 *     DisallowUnknownFields),
 *   - the §8.3 item schema rules,
 *   - the §8.3/§9.4 append-only rules against a base manifest: an id may
 *     never disappear, and an item's acceptance text may change only when
 *     the item carries a non-null waiver.
 *
 * What is NOT ported: resolving each `source` file:line anchor inside the
 * pinned baseline. The baseline went with the submodule, so the anchors are
 * checked for FORMAT only.
 */

const ID_RE = /^[A-Z]{3,8}-\d{3}$/;
const COMMIT_RE = /^[0-9a-f]{40}$/;
const DATE_RE = /^\d{4}-\d{2}-\d{2}$/;
const SOURCE_RE = /^apps\/(elitea-ui|deepwiki-ui)\/(.+):(\d+)(?:-(\d+))?$/;
const ACCEPT_RE = /^(GIVEN|WHEN|THEN|AND) /;

const DOMAINS = new Set([
  'shell', 'chat', 'agents', 'pipelines', 'skills', 'toolkits', 'mcps', 'apps',
  'credentials', 'artifacts', 'indexes', 'secrets', 'users', 'tokens',
  'notifications', 'analytics', 'public', 'admin', 'deepwiki',
]);
const KINDS = new Set(['route', 'behaviour', 'integration', 'permission', 'shell', 'visual']);
const PRIORITIES = new Set(['must', 'should', 'waived']);
const STATUSES = new Set(['todo', 'in-progress', 'implemented', 'verified', 'waived']);
const VERIFY_TYPES = new Set(['vitest', 'playwright', 'command']);
const UNITS = new Set([
  'F1', 'F2', 'F3', 'F4', 'F5', 'P1', 'T1', 'T2', 'W1', 'W2', 'W3',
  'S1', 'S2', 'S3', 'S4', 'S5', 'S6', 'S7', 'S8',
  'R1', 'R2', 'R3', 'M1', 'E1',
  'C1', 'C2', 'C3', 'C4', 'C5', 'C6',
  'A1', 'A2', 'A3', 'A4', 'A5', 'A6', 'A7', 'A8', 'A9', 'A10',
  'A11', 'A12', 'A13', 'A14', 'A15',
  'W-shell', 'V1', 'V2', 'V3', 'V4', 'X4', 'X5', 'X6',
  'DW0', 'DW1', 'DW2', 'DW3', 'DW4', 'DW5', 'DW6', 'DW7', 'DW8', 'DW9',
]);
/** Acceptance describes observable behaviour, not source structure. */
const IMPL_DETAIL_TOKENS = ['src/', '.jsx', '.tsx', '.test.', 'useState', 'useEffect', 'Redux', 'RTK '];

const ALLOWED_KEYS = {
  root: ['$schema', 'version', 'generatedFrom', 'shards', 'items'],
  generatedFrom: ['repo', 'commit', 'date'],
  shardRef: ['path', 'domain', 'items'],
  shard: ['$schema', 'domain', 'items'],
  item: ['id', 'domain', 'kind', 'priority', 'title', 'source', 'acceptance', 'verify', 'unit', 'status', 'coverage', 'waiver'],
  verify: ['type', 'command', 'testId'],
  coverage: ['file', 'min'],
  waiver: ['reason', 'decidedBy', 'date', 'replacesBehaviour'],
};

const isObject = (value) => value !== null && typeof value === 'object' && !Array.isArray(value);

/** Names every key of `value` that `kind` does not allow. */
export function unknownKeys(value, kind, where) {
  if (!isObject(value)) return [];
  const allowed = ALLOWED_KEYS[kind];
  return Object.keys(value)
    .filter((key) => !allowed.includes(key))
    .map((key) => `${where}: unknown field "${key}"`);
}

/**
 * Merge a sharded root index with its shards. `readShard(path)` returns the
 * parsed shard or null when the file is absent. Returns the merged item list
 * and every bookkeeping problem (they mean the index and the shards disagree,
 * which validation of the merged set cannot see).
 */
export function mergeManifest(root, readShard) {
  const problems = [...unknownKeys(root, 'root', 'manifest.json')];
  unknownKeys(root.generatedFrom, 'generatedFrom', 'manifest.json generatedFrom').forEach((p) => problems.push(p));
  const shards = Array.isArray(root.shards) ? root.shards : [];
  const ownItems = Array.isArray(root.items) ? root.items : [];
  if (shards.length > 0 && ownItems.length > 0) {
    problems.push('manifest.json: a manifest must have either items or shards, not both');
  }
  const items = [...ownItems];
  for (const ref of shards) {
    problems.push(...unknownKeys(ref, 'shardRef', `manifest.json shard ${ref.path}`));
    const shard = readShard(ref.path);
    if (shard === null) {
      problems.push(`shard ${ref.path}: file is missing`);
      continue;
    }
    problems.push(...unknownKeys(shard, 'shard', `shard ${ref.path}`));
    const shardItems = Array.isArray(shard.items) ? shard.items : [];
    if (shard.domain !== ref.domain) {
      problems.push(`shard ${ref.path}: index says domain "${ref.domain}", shard says "${shard.domain}"`);
    }
    if (shardItems.length !== ref.items) {
      problems.push(`shard ${ref.path}: index says ${ref.items} items, shard has ${shardItems.length}`);
    }
    for (const item of shardItems) {
      if (item.domain !== shard.domain) {
        problems.push(`shard ${ref.path}: item ${item.id} has domain "${item.domain}", expected "${shard.domain}"`);
      }
    }
    items.push(...shardItems);
  }
  return { items, problems };
}

function validateHeader(root) {
  const problems = [];
  const from = isObject(root.generatedFrom) ? root.generatedFrom : {};
  if (root.$schema !== './manifest.schema.json') {
    problems.push(`header: $schema must be ./manifest.schema.json, got "${root.$schema}"`);
  }
  if (root.version !== 1) problems.push(`header: version must be 1, got ${root.version}`);
  if (from.repo !== 'apps/elitea-ui') {
    problems.push(`header: generatedFrom.repo must be apps/elitea-ui, got "${from.repo}"`);
  }
  if (!COMMIT_RE.test(String(from.commit))) {
    problems.push(`header: generatedFrom.commit must be a 40-char sha, got "${from.commit}"`);
  }
  if (!DATE_RE.test(String(from.date))) {
    problems.push(`header: generatedFrom.date must be YYYY-MM-DD, got "${from.date}"`);
  }
  return problems;
}

function validateAcceptance(id, acceptance) {
  const problems = [];
  if (acceptance.length < 3) problems.push(`${id}: acceptance has ${acceptance.length} lines, need >=3`);
  acceptance.forEach((line, index) => {
    if (!ACCEPT_RE.test(line)) {
      problems.push(`${id}: acceptance line ${index + 1} does not start with GIVEN/WHEN/THEN/AND`);
    }
    for (const token of IMPL_DETAIL_TOKENS) {
      if (line.includes(token)) {
        problems.push(`${id}: acceptance line ${index + 1} contains implementation detail ("${token}")`);
      }
    }
  });
  if (acceptance.length > 0 && !acceptance[0].startsWith('GIVEN ')) {
    problems.push(`${id}: acceptance must start with a GIVEN line`);
  }
  const hasWhen = acceptance.some((line) => line.startsWith('WHEN '));
  const hasThen = acceptance.some((line) => line.startsWith('THEN '));
  if (!hasWhen || !hasThen) problems.push(`${id}: acceptance must contain a WHEN line and a THEN line`);
  return problems;
}

function validateWaiver(id, item) {
  const problems = [];
  const waiver = item.waiver ?? null;
  if (item.priority === 'waived' && waiver === null) {
    problems.push(`${id}: priority waived requires a non-null waiver`);
  }
  if (item.priority !== 'waived' && waiver !== null) {
    problems.push(`${id}: waiver must be null unless priority is waived`);
  }
  if (waiver !== null) {
    problems.push(...unknownKeys(waiver, 'waiver', `${id} waiver`));
    if (!waiver.reason || !waiver.decidedBy || !waiver.replacesBehaviour || !DATE_RE.test(String(waiver.date))) {
      problems.push(`${id}: waiver object incomplete (need reason, decidedBy, date YYYY-MM-DD, replacesBehaviour)`);
    }
  }
  if (item.status === 'waived' && item.priority !== 'waived') {
    problems.push(`${id}: status waived requires priority waived`);
  }
  return problems;
}

function validateItem(item) {
  const id = item.id;
  const problems = [...unknownKeys(item, 'item', String(id))];
  if (!ID_RE.test(String(id))) problems.push(`${id}: id does not match ^[A-Z]{3,8}-\\d{3}$`);
  if (!DOMAINS.has(item.domain)) problems.push(`${id}: unknown domain "${item.domain}"`);
  if (!KINDS.has(item.kind)) problems.push(`${id}: unknown kind "${item.kind}"`);
  if (!PRIORITIES.has(item.priority)) problems.push(`${id}: unknown priority "${item.priority}"`);
  if (String(item.title ?? '').length < 8) problems.push(`${id}: title too short`);

  const source = Array.isArray(item.source) ? item.source : [];
  if (source.length === 0) problems.push(`${id}: source is empty — an item with no evidence is rejected`);
  for (const anchor of source) {
    const match = SOURCE_RE.exec(anchor);
    const start = match ? Number(match[3]) : 0;
    const end = match && match[4] ? Number(match[4]) : start;
    if (!match || start < 1 || end < start) {
      problems.push(`${id}: source "${anchor}" is not apps/elitea-ui|deepwiki-ui/<file>:<line>[-<line>]`);
    }
  }

  problems.push(...validateAcceptance(id, Array.isArray(item.acceptance) ? item.acceptance : []));

  const verify = isObject(item.verify) ? item.verify : {};
  problems.push(...unknownKeys(verify, 'verify', `${id} verify`));
  if (!VERIFY_TYPES.has(verify.type)) problems.push(`${id}: unknown verify.type "${verify.type}"`);
  if (String(verify.command ?? '').length < 4) problems.push(`${id}: verify.command missing`);
  if (String(verify.testId ?? '').length < 3) problems.push(`${id}: verify.testId missing`);

  if (!UNITS.has(item.unit)) problems.push(`${id}: unknown unit "${item.unit}" (must be a §9.3 unit id)`);
  if (!STATUSES.has(item.status)) problems.push(`${id}: unknown status "${item.status}"`);

  const coverage = isObject(item.coverage) ? item.coverage : {};
  problems.push(...unknownKeys(coverage, 'coverage', `${id} coverage`));
  if (!coverage.file) problems.push(`${id}: coverage.file missing`);
  if (!Number.isInteger(coverage.min) || coverage.min < 0 || coverage.min > 100) {
    problems.push(`${id}: coverage.min ${coverage.min} out of range`);
  }

  problems.push(...validateWaiver(id, item));
  return problems;
}

/** Every §8.3 schema rule over the root header and the merged item set. */
export function validateManifest(root, items) {
  const problems = validateHeader(root);
  if (items.length === 0) problems.push('items: empty');
  const seen = new Set();
  for (const item of items) {
    if (seen.has(item.id)) problems.push(`${item.id}: duplicate id`);
    seen.add(item.id);
    problems.push(...validateItem(item));
  }
  return problems;
}

/**
 * The append-only rules against the base manifest's items: no id removed,
 * and acceptance unchanged unless the current item carries a waiver.
 */
export function checkImmutability(baseItems, items) {
  const byId = new Map(items.map((item) => [item.id, item]));
  const problems = [];
  for (const old of baseItems) {
    const current = byId.get(old.id);
    if (current === undefined) {
      problems.push(`${old.id}: id present in the base manifest but deleted — ids are immutable`);
      continue;
    }
    const same = JSON.stringify(old.acceptance ?? []) === JSON.stringify(current.acceptance ?? []);
    if (!same && (current.waiver ?? null) === null) {
      problems.push(`${old.id}: acceptance text changed relative to the base manifest without a waiver`);
    }
  }
  return problems;
}
