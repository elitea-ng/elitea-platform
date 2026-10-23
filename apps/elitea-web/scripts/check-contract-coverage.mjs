#!/usr/bin/env node
// check-contract-coverage.mjs — the §5.1 `contract-coverage` check (unit W2).
// Wired into CI as the "Contract coverage (§5.1)" step of ci-web.yml's
// `gates` job (.github/workflows/ci-web.yml), which triggers on changes
// under apps/elitea-web/** or services/elitea-main/** (a spec-only PR still
// runs it).
//
// Reads (a) the OpenAPI spec services/elitea-main/api/openapi/v2.yaml and
// (b) unit P1's endpoint census (apps/elitea-web/parity/manifest.json root
// index + manifest/<domain>.json shards, the API-* items), and classifies
// every census endpoint:
//
//   generated   — resolves to a spec operation (by operationId, else by
//                 method+path shape) whose request/response schemas are
//                 field-level.
//   handwritten — no spec operation covers it. Allowed and counted; these
//                 endpoints get hand-written clients built from Channel-B
//                 fixtures (spec §5.3).
//   VIOLATION   — resolves to a spec operation that still references the
//                 generic `Struct`/`GenericResponse` shells, or whose schema
//                 graph contains a permissive object (additionalProperties
//                 true/{}, a bare/empty-properties object, or an untyped
//                 schema) WITHOUT a VALID `x-elitea-passthrough` marker.
//   waived      — the census item carries a parity waiver (e.g. W-008);
//                 excluded from the burn-down denominator.
//
// Exit code is non-zero iff any VIOLATION exists. The printed
// generated/total ratio is the §5.1 burn-down metric.
//
// Usage:
//   node apps/elitea-web/scripts/check-contract-coverage.mjs \
//     [--spec <path>] [--manifest <path>] [--json] [--verbose] [--update-lock]
//
// Dependency-light by design: js-yaml is resolved from apps/elitea-web's own
// node_modules (declared devDependency; decision record D2 dropped
// openapi-typescript — this checker parses YAML directly).
//
// ---------------------------------------------------------------------------
// Three detections added after the W2 adversarial audit (each has a RED
// fixture; see REPRODUCE below):
//
// D1. MARKER VALUE VALIDATION. `x-elitea-passthrough` is a justification, not
//     a mute button. The value must be a non-empty string containing
//     "NOTE(W2):" AND a file:line citation (e.g. `handler.go:1764-1783`).
//     An empty or garbage marker is itself a VIOLATION and does NOT justify
//     the permissive node it sits on — previously only key presence was
//     tested, so `x-elitea-passthrough: ""` silenced the whole subtree.
//
// D2. BURN-DOWN RATCHET. contract-coverage.lock.json (beside this script)
//     pins the census ids currently classified `generated`. If any locked id
//     stops being generated — spec op deleted, operationId renamed, path
//     shape drifted — that is a VIOLATION, not a silent reclassification to
//     `handwritten`. Mirrors §9.6's ratchet philosophy: coverage may only go
//     up unless a human updates the lock. Regenerate deliberately with
//     `--update-lock` (and say why in the commit message). A lock file that
//     is missing, empty, or not valid JSON with a `generated` array is ALSO
//     a hard failure (not a soft "ratchet inactive" skip) — deleting or
//     corrupting the lock must not be a way to silence a real regression;
//     `--update-lock` is the only sanctioned way to not have a valid lock.
//
// D3. PERMISSIVE-SHAPE EVASIONS. Every OpenAPI spelling of "accepts
//     anything" is caught, not just `additionalProperties: true`:
//       - `{type: object, properties: {}}` — empty map, same as bare
//       - `{properties: {}}` with no `type` key — `type` isn't required for
//         `properties` to imply an object
//       - `{type: array}` with no `items` — unconstrained elements
//     A bare/empty branch inside `allOf` is exempt ONLY when the composite
//     is genuinely constrained elsewhere — some branch (itself or a sibling)
//     declares `required`, or a sibling is a `$ref` or has non-empty
//     `properties` (the real `VersionWriteRequest` + `{required:[name]}`
//     pattern). A lone bare `allOf` branch, or an allOf of nothing but bare
//     branches, is a full bypass and stays flagged; `oneOf`/`anyOf` never
//     get this exemption (they are disjunctive — a bare branch there really
//     does admit anything for that arm).
//
// A valid `x-elitea-passthrough` marker justifies ONLY the node it sits on —
// there is no ancestor inheritance. A marker at a response root does not
// whitelist a permissive descendant several levels down; that descendant
// needs its own marker (or its own real type constraint).
//
// Operation `parameters` schemas are validated alongside responses and
// requestBody (path-level and operation-level, `$ref`s resolved) — both a
// parameter's `schema` AND its `content` typing (D4), and response
// `headers` schemas, which are typed the same way and were previously
// unwalked entirely (E1).
//
// REPRODUCE (RED/GREEN proofs — fixtures live in the W2 scratch set, each is
// a minimal spec exercising exactly one detection):
//   D1  --spec fixture-marker-empty.yaml    -> exit 1, "marker must be a non-empty string"
//       --spec fixture-marker-garbage.yaml  -> exit 1, "marker must cite file:line evidence"
//   D2  delete a spec op the census matches -> exit 1, "left the generated set"
//   D3  --spec fixture-empty-properties.yaml-> exit 1, "bare object schema"
//   real spec (this repo)                   -> exit 0, ratio printed, lock satisfied

import { readFileSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { checkFloors, subjectPath } from './lib/gate-floor.mjs';

const require = createRequire(import.meta.url);
const yaml = require('js-yaml');
const parseYaml = (text) => (yaml.safeLoad ? yaml.safeLoad(text) : yaml.load(text));

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(scriptDir, '..', '..', '..');

// --- CLI ---------------------------------------------------------------------
const args = process.argv.slice(2);
const opts = {
  json: false, verbose: false, spec: null, manifest: null,
  lock: null, updateLock: false,
};
const FLAGS = { '--json': 'json', '--verbose': 'verbose', '--update-lock': 'updateLock' };
const VALUE_FLAGS = { '--spec': 'spec', '--manifest': 'manifest', '--lock': 'lock' };
for (let i = 0; i < args.length; i++) {
  const a = args[i];
  if (FLAGS[a]) opts[FLAGS[a]] = true;
  else if (VALUE_FLAGS[a]) opts[VALUE_FLAGS[a]] = args[++i];
  else if (a === '--help' || a === '-h') {
    console.log('usage: check-contract-coverage.mjs [--spec path] [--manifest path] [--lock path] [--json] [--verbose] [--update-lock]');
    process.exit(0);
  } else {
    console.error(`unknown argument: ${a}`);
    process.exit(2);
  }
}
const specPath = path.resolve(
  opts.spec ?? path.join(repoRoot, 'services/elitea-main/api/openapi/v2.yaml'),
);
const manifestPath = path.resolve(
  opts.manifest ?? path.join(repoRoot, 'apps/elitea-web/parity/manifest.json'),
);
const lockPath = path.resolve(opts.lock ?? path.join(scriptDir, 'contract-coverage.lock.json'));

// --- Spec loading --------------------------------------------------------------
const FORBIDDEN_SCHEMAS = new Set(['Struct', 'GenericResponse']);
const HTTP_METHODS = ['get', 'put', 'post', 'delete', 'options', 'head', 'patch', 'trace'];

const doc = parseYaml(readFileSync(specPath, 'utf8'));
if (!doc || typeof doc !== 'object' || !doc.paths) {
  console.error(`spec ${specPath} has no paths object`);
  process.exit(2);
}
const componentSchemas = doc.components?.schemas ?? {};

function serverBasePaths(servers) {
  if (!Array.isArray(servers) || servers.length === 0) return [];
  return servers
    .map((s) => {
      try {
        // Server URLs here are path-only ("/api/v2") or "/".
        const u = new URL(s.url, 'http://x');
        return u.pathname.replace(/\/$/, '');
      } catch {
        return '';
      }
    })
    .filter((p, i, arr) => arr.indexOf(p) === i);
}

const rootBases = serverBasePaths(doc.servers);

/** @type {Array<{operationId:string, method:string, path:string, candidates:string[][], op:object}>} */
const specOps = [];
for (const [p, item] of Object.entries(doc.paths)) {
  if (!item || typeof item !== 'object') continue;
  const itemBases = item.servers ? serverBasePaths(item.servers) : rootBases;
  for (const method of HTTP_METHODS) {
    const op = item[method];
    if (!op || !op.operationId) continue;
    const opBases = op.servers ? serverBasePaths(op.servers) : itemBases;
    // Candidate shapes: each server base + path, plus the bare path (the SPA
    // census records paths relative to its /api/v2 baseUrl).
    const rawCandidates = [p, ...opBases.map((b) => b + p)];
    const candidates = [...new Set(rawCandidates)].map(normalizeSegments);
    const params = [...(item.parameters ?? []), ...(op.parameters ?? [])];
    specOps.push({
      operationId: op.operationId, method: method.toUpperCase(), path: p,
      candidates, op, params,
    });
  }
}
const specOpsById = new Map(specOps.map((o) => [o.operationId, o]));

/*
 * Floor on the spec side (issue #528).
 *
 * The census side already refused an empty set. The SPEC side did not, and
 * `paths: {}` passes the `doc.paths` guard above because an empty object is
 * truthy. The loop then drops every operation that has lost its
 * `operationId`, so `specOps` can also empty out while the file stays full.
 *
 * With `specOps` empty, nothing matches, every census item becomes
 * `handwritten`, and no VIOLATION is reported. The burn-down ratchet catches
 * that today only because the lock happens to list ids — an accident of the
 * lock contents, not an assertion of this script.
 *
 * Measured on 2026-08-28: 157 operations in v2.yaml.
 */
const MIN_SPEC_OPERATIONS = 100;

/*
 * Floor on the census side. It replaces a bare `length === 0` refusal: zero is
 * not the only broken state, and the count belongs in the log either way.
 * Measured on 2026-08-28: 218 API-* items.
 */
const MIN_CENSUS_ITEMS = 150;

const specFloor = checkFloors('contract-coverage', [
  { subject: `operations with an operationId in ${subjectPath(repoRoot, specPath)}`, observed: specOps.length, floor: MIN_SPEC_OPERATIONS },
]);
if (!specFloor.ok) {
  console.error(specFloor.error);
  process.exit(2);
}

// --- Path shape matching (mirrors oapiserver/conformance.go rules) --------------
function normalizeSegments(p) {
  let s = String(p).trim();
  s = s.replace(/\/+$/, '');
  s = s.replace(/^\/+/, '');
  if (s === '') return [];
  return s.split('/').map((seg) => (seg.startsWith('{') && seg.endsWith('}') ? '{}' : seg));
}

// spec side plays the "route" role: its placeholders accept anything, but a
// census placeholder never matches a spec literal (the census would be wider).
function segmentsMatch(specSegs, manSegs) {
  if (specSegs.length !== manSegs.length) return false;
  for (let i = 0; i < specSegs.length; i++) {
    const sp = specSegs[i];
    const mn = manSegs[i];
    if (sp === '{}') continue; // placeholder accepts one segment of anything
    if (mn === '{}' || (mn.startsWith('{') && mn.endsWith('}'))) return false;
    if (sp !== mn) return false;
  }
  return true;
}

// Census paths carry SPA template noise; reduce to a matchable shape or null.
function cleanManifestPath(raw) {
  if (!raw) return null;
  let p = String(raw).trim();
  if (p === '{url}' || p === '{base}' || p.startsWith('{url}') || p.startsWith('{base}')) return null;
  // alternation templates like /elitea_core{/a/{x}|/b} are not a single shape
  if (/\{\//.test(p) || /\|/.test(p)) return null;
  p = p.split('?')[0];
  // strip glued trailing template tokens: ...{projectId}{expr} -> ...{projectId}
  for (;;) {
    const next = p.replace(/(\})\{[^{}]*\}$/, '$1');
    if (next === p) break;
    p = next;
  }
  if (!p.startsWith('/')) p = '/' + p;
  if (p === '/') return null;
  return p;
}

// --- Schema graph validation -----------------------------------------------------
function refName(ref) {
  const m = /^#\/components\/schemas\/([^/]+)$/.exec(ref ?? '');
  return m ? m[1] : null;
}

const STRUCTURAL_KEYS = [
  'type', 'properties', 'items', '$ref', 'allOf', 'oneOf', 'anyOf', 'not',
  'enum', 'const', 'format', 'pattern', 'minimum', 'maximum', 'minLength',
  'maxLength', 'minItems', 'maxItems',
];

function isEmptySchema(node) {
  if (node === true) return true;
  if (!node || typeof node !== 'object') return false;
  if (STRUCTURAL_KEYS.some((k) => k in node)) return false;
  // additionalProperties alone still counts as structural intent
  if ('additionalProperties' in node) return false;
  return true; // only description / nullable / x-* / example etc.
}

// --- D1: x-elitea-passthrough marker VALUE validation ----------------------------
const MARKER = 'x-elitea-passthrough';
const MARKER_NOTE = 'NOTE(W2):';
// A citation is a filename with an extension followed by :<line> (ranges and
// multi-cite lists both satisfy it): handler.go:1764-1783, types.go:23, ...
const FILE_LINE_RE = /[\w./+-]+\.(?:go|sql|mjs|ts|tsx|js|jsx|yaml|yml|json|py):\d+/;

/** @returns {string|null} the reason the marker is invalid, or null if valid. */
function markerIssue(value) {
  if (typeof value !== 'string' || value.trim() === '') {
    return `${MARKER} marker must be a non-empty string`;
  }
  if (!value.includes(MARKER_NOTE)) {
    return `${MARKER} marker must contain "${MARKER_NOTE}"`;
  }
  if (!FILE_LINE_RE.test(value)) {
    return `${MARKER} marker must cite file:line evidence`;
  }
  return null;
}

function hasValidMarker(n) {
  return !!n && typeof n === 'object' && MARKER in n && markerIssue(n[MARKER]) === null;
}

function isPermissiveAP(ap) {
  if (ap === true) return true;
  if (ap && typeof ap === 'object' && isEmptySchema(ap) && !hasValidMarker(ap)) return true;
  return false;
}

// D3: a bare {type: object} — or the equivalent {type: object, properties: {}}
// — implicitly accepts anything, because OpenAPI defaults additionalProperties
// to true when it is absent. An empty `properties` map is NOT field-level
// coverage.
function hasNoProperties(n) {
  return n.properties === undefined || Object.keys(n.properties).length === 0;
}

// MEDIUM-3a: `type` is not required for `properties` to imply an object —
// {properties: {}} with no `type` key is exactly as permissive as
// {type: object, properties: {}} and must be caught the same way.
function impliesObject(n) {
  return n.type === 'object' || (n.type === undefined && 'properties' in n);
}

function isBareObject(n) {
  return impliesObject(n) && hasNoProperties(n)
    && !n.allOf && !n.oneOf && !n.anyOf;
}

// MEDIUM-3b: {type: array} with no `items` places no constraint on element
// shape — as permissive as a bare object, just for arrays.
function isUnconstrainedArray(n) {
  return n.type === 'array' && n.items === undefined;
}

// HIGH-2: a bare allOf BRANCH is exempt from the bare-object check only when
// the composite genuinely cannot be widened by it — some branch in the SAME
// allOf (itself or a sibling) declares `required`, or a sibling is a $ref or
// carries non-empty `properties`. `allOf: [{type: object}]` on its own (no
// constraining branch anywhere) is a full bypass, not a constraint fragment
// — oneOf/anyOf are disjunctive and never get this exemption.
function allOfIsConstrained(branches) {
  const hasRequired = branches.some((b) => Array.isArray(b?.required) && b.required.length > 0);
  if (hasRequired) return true;
  return branches.length > 1 && branches.some(
    (b) => b && typeof b === 'object' && (b.$ref || (b.properties && Object.keys(b.properties).length > 0)),
  );
}

/**
 * Walk a schema graph; report violations.
 * A node is a passthrough when: it $refs Struct/GenericResponse, OR it is
 * permissive (additionalProperties true/empty, bare object with no
 * properties, or an untyped schema). Permissive nodes are allowed only when
 * the node or an ancestor on the traversal path carries x-elitea-passthrough.
 */
function validateSchema(node, ctx) {
  const violations = [];
  const seenRefs = new Set();

  function walkRef(ref, where) {
    const name = refName(ref);
    if (name && FORBIDDEN_SCHEMAS.has(name)) {
      violations.push(`${where}: resolves to forbidden generic schema "${name}"`);
      return;
    }
    if (!name) return;
    if (seenRefs.has(name)) return; // cycle guard; each component validated once per root
    seenRefs.add(name);
    const target = componentSchemas[name];
    if (target === undefined) {
      violations.push(`${where}: dangling $ref ${ref}`);
      return;
    }
    // Justification does NOT leak across a $ref boundary: a named
    // component must justify its own permissiveness.
    walk(target, `${where} -> ${name}`);
  }

  function checkPermissiveObject(n, where, selfJustified, bareExempt) {
    if ('additionalProperties' in n) {
      const ap = n.additionalProperties;
      if (isPermissiveAP(ap) && !selfJustified) {
        violations.push(`${where}: additionalProperties passthrough without a valid ${MARKER}`);
      } else if (ap && typeof ap === 'object') {
        // No inheritance (MEDIUM-2): ap is justified only by its OWN marker,
        // checked fresh inside this walk call.
        walk(ap, `${where}.additionalProperties`);
      }
      return;
    }
    if (selfJustified) return;
    if (isUnconstrainedArray(n)) {
      violations.push(`${where}: array schema with no items (unconstrained elements) without a valid ${MARKER}`);
      return;
    }
    // bareExempt is true only for an allOf branch whose composite is
    // genuinely constrained elsewhere (see allOfIsConstrained) — a real
    // `required`-override fragment, not a passthrough.
    if (bareExempt) return;
    if (isBareObject(n)) {
      violations.push(`${where}: bare object schema (no properties) without a valid ${MARKER}`);
    }
  }

  // D1: a marker that is present but malformed is itself a violation, and it
  // does NOT justify the node it sits on.
  function checkMarker(n, where) {
    if (!(MARKER in n)) return false;
    const issue = markerIssue(n[MARKER]);
    if (issue === null) return true;
    violations.push(`${where}: ${issue}`);
    return false;
  }

  function walkCombinator(n, kw, where) {
    const branches = n[kw];
    if (!branches) return;
    const exempt = kw === 'allOf' && allOfIsConstrained(branches);
    branches.forEach((sub, i) => walk(sub, `${where}.${kw}[${i}]`, exempt));
  }

  // MEDIUM-2: NO ancestor inheritance. Each node's justification comes only
  // from checkMarker(n) on that SAME node — a marker at a response root (or
  // anywhere up the tree) does not whitelist permissive descendants. Every
  // recursive call below therefore starts a fresh, unjustified walk; the
  // only per-call state that travels downward is `bareExempt`, and only to
  // the immediate allOf branch it was computed for (walkCombinator).
  function walkChildren(n, where) {
    for (const [k, v] of Object.entries(n.properties ?? {})) {
      walk(v, `${where}.${k}`);
    }
    if (n.items) walk(n.items, `${where}[]`);
    for (const kw of ['allOf', 'oneOf', 'anyOf']) {
      walkCombinator(n, kw, where);
    }
  }

  function walk(n, where, bareExempt = false) {
    if (n == null) return;
    if (n === true) {
      // A bare JSON-Schema `true` cannot carry its own marker (booleans have
      // no keys), so under the no-inheritance rule it can never be
      // justified — always a violation.
      violations.push(`${where}: boolean-true schema without a valid ${MARKER}`);
      return;
    }
    if (typeof n !== 'object') return;

    const selfJustified = checkMarker(n, where);

    if (n.$ref) {
      walkRef(n.$ref, where);
      return;
    }

    // untyped / empty schema
    if (isEmptySchema(n) && !selfJustified) {
      violations.push(`${where}: untyped schema without a valid ${MARKER}`);
      return;
    }

    checkPermissiveObject(n, where, selfJustified, bareExempt);
    walkChildren(n, where);
  }

  walk(node, ctx);
  return violations;
}

const componentResponses = doc.components?.responses ?? {};

function resolveResponse(resp) {
  if (resp && resp.$ref) {
    const m = /^#\/components\/responses\/(.+)$/.exec(resp.$ref);
    if (m) return componentResponses[m[1]];
  }
  return resp;
}

function validateContent(content, prefix, violations) {
  for (const [mt, media] of Object.entries(content ?? {})) {
    if (media?.schema) {
      violations.push(...validateSchema(media.schema, `${prefix} ${mt}`));
    }
  }
}

const componentParameters = doc.components?.parameters ?? {};
const componentHeaders = doc.components?.headers ?? {};

function resolveParameter(param) {
  if (param && param.$ref) {
    const m = /^#\/components\/parameters\/(.+)$/.exec(param.$ref);
    if (m) return componentParameters[m[1]];
  }
  return param;
}

function resolveHeader(header) {
  if (header && header.$ref) {
    const m = /^#\/components\/headers\/(.+)$/.exec(header.$ref);
    if (m) return componentHeaders[m[1]];
  }
  return header;
}

// D4: a Parameter Object types itself with EITHER `schema` (simple params)
// OR `content: {<media-type>: {schema: ...}}` (complex params, e.g. a
// JSON-in-query filter) — never both. Checking only `schema` let a
// content-typed parameter's schema pass unvalidated.
function validateParameters(params, operationId, violations) {
  for (const raw of params ?? []) {
    const param = resolveParameter(raw);
    if (!param) continue;
    const prefix = `${operationId} param ${param.name ?? '?'}`;
    if (param.schema) violations.push(...validateSchema(param.schema, prefix));
    validateContent(param.content, prefix, violations);
  }
}

// E1: a response Header Object is typed the same way a parameter is
// (`schema` or `content`) and was previously never walked at all.
function validateHeaders(headers, prefix, violations) {
  for (const [name, raw] of Object.entries(headers ?? {})) {
    const header = resolveHeader(raw);
    if (!header) continue;
    const hPrefix = `${prefix} header ${name}`;
    if (header.schema) violations.push(...validateSchema(header.schema, hPrefix));
    validateContent(header.content, hPrefix, violations);
  }
}

function validateOperation(specOp) {
  const violations = [];
  const { op, operationId, params } = specOp;
  for (const [code, respRaw] of Object.entries(op.responses ?? {})) {
    const resp = resolveResponse(respRaw);
    validateContent(resp?.content, `${operationId} ${code}`, violations);
    validateHeaders(resp?.headers, `${operationId} ${code}`, violations);
  }
  validateContent(op.requestBody?.content, `${operationId} request`, violations);
  validateParameters(params, operationId, violations);
  return violations;
}

// --- Manifest loading -------------------------------------------------------------
const manifestRoot = JSON.parse(readFileSync(manifestPath, 'utf8'));
const manifestDir = path.dirname(manifestPath);

/** @type {Array<{id:string, method:string, rawPath:string, opName:string, waiver:any, domain:string}>} */
const apiItems = [];

function parseEndpointTitle(title) {
  const m = /Endpoint\s+(\S+)\s+(\S+)\s*(?:\(([^)]*)\))?/.exec(title ?? '');
  return {
    method: (m?.[1] ?? '').toUpperCase(),
    rawPath: m?.[2] ?? '',
    opName: m?.[3] ?? '',
  };
}

function parseApiItem(it, domain) {
  if (!it || typeof it !== 'object') return null;
  if (!String(it.id ?? '').startsWith('API-')) return null;
  return {
    id: it.id,
    ...parseEndpointTitle(it.title),
    waiver: it.waiver ?? null,
    domain: it.domain ?? domain ?? '',
  };
}

function collectItems(items, domain) {
  for (const it of items ?? []) {
    const parsed = parseApiItem(it, domain);
    if (parsed) apiItems.push(parsed);
  }
}

if (Array.isArray(manifestRoot.shards)) {
  for (const shard of manifestRoot.shards) {
    const shardDoc = JSON.parse(readFileSync(path.join(manifestDir, shard.path), 'utf8'));
    collectItems(shardDoc.items ?? shardDoc, shard.domain);
  }
} else if (Array.isArray(manifestRoot.items)) {
  collectItems(manifestRoot.items, '');
} else if (Array.isArray(manifestRoot.endpoints)) {
  // future endpoints.manifest.json shape (spec §5.3)
  for (const ep of manifestRoot.endpoints) {
    apiItems.push({
      id: ep.id,
      method: (ep.method ?? '').toUpperCase(),
      rawPath: ep.path ?? '',
      opName: ep.operationId ?? '',
      waiver: ep.waiver ?? null,
      domain: '',
    });
  }
}

const censusFloor = checkFloors('contract-coverage', [
  { subject: `API-* items in ${subjectPath(repoRoot, manifestPath)}`, observed: apiItems.length, floor: MIN_CENSUS_ITEMS },
]);
if (!censusFloor.ok) {
  console.error(censusFloor.error);
  process.exit(2);
}

// --- Classification -----------------------------------------------------------------
const results = [];
for (const item of apiItems) {
  if (item.waiver) {
    results.push({ ...item, status: 'waived', operationId: null, violations: [] });
    continue;
  }

  // 1) operationId equality (W1 named spec ops after the SPA's RTK names
  //    where they coincide)
  let matched = null;
  const byId = specOpsById.get(item.opName);
  if (byId && byId.method === item.method) matched = byId;

  // 2) method + path shape
  if (!matched) {
    const clean = cleanManifestPath(item.rawPath);
    if (clean) {
      const manSegs = String(clean).replace(/^\/+/, '').replace(/\/+$/, '')
        .split('/')
        .map((seg) => (seg.startsWith('{') && seg.endsWith('}') ? '{}' : seg));
      matched = specOps.find(
        (o) => o.method === item.method && o.candidates.some((c) => segmentsMatch(c, manSegs)),
      ) ?? null;
    }
  }

  if (!matched) {
    results.push({ ...item, status: 'handwritten', operationId: null, violations: [] });
    continue;
  }

  const violations = validateOperation(matched);
  results.push({
    ...item,
    status: violations.length > 0 ? 'VIOLATION' : 'generated',
    operationId: matched.operationId,
    violations,
  });
}

// Spec-side self-check: every operation in the spec must itself be
// field-level, even if no census item currently maps to it (guards against
// regressions on ops the matcher misses).
const specOnlyViolations = [];
for (const specOp of specOps) {
  const v = validateOperation(specOp);
  if (v.length > 0) specOnlyViolations.push({ operationId: specOp.operationId, violations: v });
}

// --- D2: burn-down ratchet ---------------------------------------------------------
// The lock pins the census ids that are currently `generated`. Losing one
// without updating the lock is a regression, not a silent reclassification.
const generatedIds = results.filter((r) => r.status === 'generated').map((r) => r.id).sort();

// MEDIUM-1: a missing, blank, or malformed lock is NOT a soft "ratchet
// inactive" state — once a lock is expected (i.e. outside --update-lock
// bootstrapping), being unable to read it is exactly as dangerous as the
// regression it exists to catch, and easier to cause by accident (or by an
// attacker who deletes/truncates the file). Every branch below reports a
// reason string instead of silently returning null.
function readLock() {
  let raw;
  try {
    raw = readFileSync(lockPath, 'utf8');
  } catch (err) {
    if (err.code === 'ENOENT') return { ok: false, reason: 'does not exist' };
    throw err;
  }
  if (raw.trim() === '') return { ok: false, reason: 'is empty' };
  try {
    const data = JSON.parse(raw);
    if (!Array.isArray(data?.generated)) {
      return { ok: false, reason: 'has no "generated" array' };
    }
    return { ok: true, data };
  } catch {
    return { ok: false, reason: 'is not valid JSON' };
  }
}

function computeRatchet() {
  const read = readLock();
  if (!read.ok) {
    return { known: false, invalidReason: read.reason, regressions: [] };
  }
  const current = new Set(generatedIds);
  const byId = new Map(results.map((r) => [r.id, r]));
  const regressions = read.data.generated
    .filter((id) => !current.has(id))
    .map((id) => ({ id, now: byId.get(id)?.status ?? 'absent from the census' }));
  return { known: true, invalidReason: null, regressions };
}

const ratchet = computeRatchet();

if (opts.updateLock) {
  writeFileSync(lockPath, `${JSON.stringify({
    $comment: 'Burn-down ratchet for scripts/check-contract-coverage.mjs (spec 5.1/9.6). '
      + 'Ids listed here MUST stay classified `generated`; losing one fails the '
      + '"Contract coverage (§5.1)" step of ci-web.yml\'s gates job. '
      + 'Regenerate deliberately with --update-lock and explain why in the commit message.',
    version: 1,
    generated: generatedIds,
  }, null, 2)}\n`);
  console.log(`contract-coverage: wrote ${path.relative(repoRoot, lockPath)} with ${generatedIds.length} generated ids`);
  process.exit(0);
}

// --- Report ---------------------------------------------------------------------------
const counts = { generated: 0, handwritten: 0, VIOLATION: 0, waived: 0 };
for (const r of results) counts[r.status]++;
const denominator = results.length - counts.waived;
const ratio = denominator > 0 ? counts.generated / denominator : 0;
const ratioLine = `contract-coverage: ${counts.generated}/${denominator} generated (${(ratio * 100).toFixed(1)}%) — handwritten ${counts.handwritten}, violations ${counts.VIOLATION}, waived ${counts.waived}`;

const hasFailures = counts.VIOLATION > 0 || specOnlyViolations.length > 0
  || ratchet.regressions.length > 0 || !!ratchet.invalidReason;

if (opts.json) {
  console.log(JSON.stringify({
    spec: path.relative(repoRoot, specPath),
    manifest: path.relative(repoRoot, manifestPath),
    specOperationCount: specOps.length,
    censusItemCount: apiItems.length,
    totals: { ...counts, denominator },
    ratio: Number(ratio.toFixed(4)),
    ok: !hasFailures,
    lock: {
      path: path.relative(repoRoot, lockPath),
      present: ratchet.known,
      invalidReason: ratchet.invalidReason,
      regressions: ratchet.regressions,
    },
    endpoints: results.map((r) => ({
      id: r.id,
      method: r.method,
      path: r.rawPath,
      status: r.status,
      operationId: r.operationId,
      violations: r.violations,
    })),
    specOnlyViolations,
  }, null, 2));
} else {
  for (const r of results) {
    if (r.status === 'VIOLATION') {
      console.log(`VIOLATION  ${r.id} ${r.method} ${r.rawPath} -> ${r.operationId}`);
      for (const v of r.violations) console.log(`           - ${v}`);
    } else if (opts.verbose) {
      console.log(`${r.status.padEnd(11)}${r.id} ${r.method} ${r.rawPath}${r.operationId ? ` -> ${r.operationId}` : ''}`);
    }
  }
  for (const s of specOnlyViolations) {
    console.log(`VIOLATION  (spec-only) operation ${s.operationId}`);
    for (const v of s.violations) console.log(`           - ${v}`);
  }
  for (const r of ratchet.regressions) {
    console.log(`VIOLATION  (ratchet) ${r.id} left the generated set — now ${r.now}`);
    console.log('           - restore coverage, or re-run with --update-lock and justify it');
  }
  if (ratchet.invalidReason) {
    console.log(`VIOLATION  (ratchet) lock file ${path.relative(repoRoot, lockPath)} ${ratchet.invalidReason}`);
    console.log('           - the burn-down ratchet cannot run without it; restore it or create it deliberately with --update-lock');
  }
  // State both subject sets on the pass path too, so a reader can tell a full
  // comparison from one that measured nothing.
  for (const line of [...specFloor.lines, ...censusFloor.lines]) console.log(line);
  console.log(ratioLine);
  console.log(hasFailures ? 'contract-coverage: FAIL' : 'contract-coverage: OK');
}

process.exit(hasFailures ? 1 : 0);
