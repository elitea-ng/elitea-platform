#!/usr/bin/env node
/**
 * Contract tooling for the pinned server OpenAPI document (contract/).
 *
 *   node scripts/contract.mjs verify   check contract/elitea-v2.yaml against its pinned sha256
 *   node scripts/contract.mjs build    merge the native-gap overlay, prune to client-operations.json,
 *                                      write contract/.build/merged.yaml
 *   node scripts/contract.mjs gen      build, then generate src/api/generated/elitea.gen.ts
 *   node scripts/contract.mjs check    build + generate in memory and fail if the committed
 *                                      generated file differs (CI drift guard)
 *
 * Every command exits non-zero with a one-line reason on failure.
 */
import { createHash } from 'node:crypto';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import YAML from 'yaml';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
export const CONTRACT_DIR = join(ROOT, 'contract');
export const UPSTREAM_SPEC = join(CONTRACT_DIR, 'elitea-v2.yaml');
export const UPSTREAM_SHA = join(CONTRACT_DIR, 'elitea-v2.yaml.sha256');
export const OVERLAY_SPEC = join(CONTRACT_DIR, 'overlay', 'native-gap.yaml');
export const CLIENT_OPERATIONS = join(CONTRACT_DIR, 'client-operations.json');
export const MERGED_SPEC = join(CONTRACT_DIR, '.build', 'merged.yaml');
export const GENERATED_FILE = join(ROOT, 'src', 'api', 'generated', 'elitea.gen.ts');

const HTTP_METHODS = ['get', 'put', 'post', 'delete', 'options', 'head', 'patch', 'trace'];
const OVERLAY_TOP_LEVEL = new Set(['openapi', 'info', 'paths', 'components', 'x-zefir-extend']);
const REQUIRED_OVERLAY_OP_KEYS = ['operationId', 'x-zefir-source', 'x-zefir-until'];
const REQUIRED_EXTENSION_KEYS = ['x-zefir-source', 'x-zefir-until'];
const OPERATION_EXTENSION_KEYS = new Set([...REQUIRED_EXTENSION_KEYS, 'parameters', 'responses']);
const SCHEMA_EXTENSION_KEYS = new Set([...REQUIRED_EXTENSION_KEYS, 'properties']);

export class ContractError extends Error {}

export function sha256Hex(bytes) {
  return createHash('sha256').update(bytes).digest('hex');
}

/**
 * Reads a `shasum -a 256` line (`<64 hex>  <file name>`) and returns the hash.
 */
export function parseShaFile(text) {
  const match = /^([0-9a-f]{64})\s+\*?(\S.*)$/m.exec(text.trim());
  if (!match) throw new ContractError('sha256 file is not in `<hex>  <name>` form');
  return { hash: match[1], name: match[2] };
}

/** Throws ContractError unless the spec bytes hash to the pinned value. */
export function verifyPin(specPath = UPSTREAM_SPEC, shaPath = UPSTREAM_SHA) {
  const expected = parseShaFile(readFileSync(shaPath, 'utf8')).hash;
  const actual = sha256Hex(readFileSync(specPath));
  if (actual !== expected) {
    throw new ContractError(
      `${specPath} does not match its pinned sha256 (expected ${expected}, got ${actual}). ` +
        'The upstream copy must never be edited; see contract/README.md to bump it.',
    );
  }
  return actual;
}

export function parseYaml(text, label = 'document') {
  try {
    return YAML.parse(text, { maxAliasCount: -1 });
  } catch (error) {
    throw new ContractError(`${label} is not valid YAML: ${error.message.split('\n')[0]}`);
  }
}

const isObject = (value) => value !== null && typeof value === 'object' && !Array.isArray(value);

function deepEqual(a, b) {
  return JSON.stringify(a) === JSON.stringify(b);
}

/**
 * Merges the overlay into a deep copy of the upstream document.
 *
 * Fails when the overlay defines an operation (path + method) or a component
 * that upstream already has: once the server documents an operation, the
 * overlay's hand-written copy must be deleted rather than silently shadow it.
 */
export function mergeOverlay(upstream, overlay) {
  if (!isObject(overlay)) throw new ContractError('overlay is not a mapping');
  for (const key of Object.keys(overlay)) {
    if (!OVERLAY_TOP_LEVEL.has(key)) {
      throw new ContractError(`overlay top-level key "${key}" is not allowed`);
    }
  }
  const merged = structuredClone(upstream);
  merged.paths ??= {};
  // Extensions apply to upstream definitions only (before the overlay's own
  // operations and components are merged in): an overlay definition is edited
  // in place instead.
  applyExtensions(merged, overlay['x-zefir-extend']);

  for (const [path, overlayItem] of Object.entries(overlay.paths ?? {})) {
    const upstreamItem = merged.paths[path];
    for (const method of HTTP_METHODS) {
      const op = overlayItem[method];
      if (op === undefined) continue;
      for (const required of REQUIRED_OVERLAY_OP_KEYS) {
        if (!op[required]) {
          throw new ContractError(`overlay ${method.toUpperCase()} ${path} is missing ${required}`);
        }
      }
      if (upstreamItem?.[method] !== undefined) {
        throw new ContractError(
          `upstream now defines ${method.toUpperCase()} ${path}; delete it from the overlay`,
        );
      }
    }
    if (upstreamItem === undefined) {
      merged.paths[path] = structuredClone(overlayItem);
      continue;
    }
    for (const [key, value] of Object.entries(overlayItem)) {
      if (HTTP_METHODS.includes(key)) {
        upstreamItem[key] = structuredClone(value);
      } else if (key in upstreamItem && !deepEqual(upstreamItem[key], value)) {
        throw new ContractError(
          `overlay path-level "${key}" for ${path} differs from upstream; drop it from the overlay`,
        );
      } else if (!(key in upstreamItem)) {
        upstreamItem[key] = structuredClone(value);
      }
    }
  }

  for (const [section, entries] of Object.entries(overlay.components ?? {})) {
    merged.components ??= {};
    merged.components[section] ??= {};
    for (const [name, value] of Object.entries(entries)) {
      if (name in merged.components[section]) {
        throw new ContractError(
          `upstream already defines components.${section}.${name}; rename or delete the overlay copy`,
        );
      }
      merged.components[section][name] = structuredClone(value);
    }
  }

  const seen = new Map();
  for (const [path, item] of Object.entries(merged.paths)) {
    for (const method of HTTP_METHODS) {
      const id = item?.[method]?.operationId;
      if (!id) continue;
      if (seen.has(id)) {
        throw new ContractError(`operationId ${id} is used by both ${seen.get(id)} and ${method.toUpperCase()} ${path}`);
      }
      seen.set(id, `${method.toUpperCase()} ${path}`);
    }
  }
  return merged;
}

function findOperation(doc, operationId) {
  for (const item of Object.values(doc.paths ?? {})) {
    for (const method of HTTP_METHODS) {
      if (item?.[method]?.operationId === operationId) return item[method];
    }
  }
  return undefined;
}

function requireExtensionKeys(entry, label, allowed) {
  if (!isObject(entry)) throw new ContractError(`overlay extension ${label} is not a mapping`);
  for (const required of REQUIRED_EXTENSION_KEYS) {
    if (!entry[required]) throw new ContractError(`overlay extension ${label} is missing ${required}`);
  }
  for (const key of Object.keys(entry)) {
    if (!allowed.has(key)) throw new ContractError(`overlay extension ${label} may not set "${key}"`);
  }
}

/**
 * `x-zefir-extend`: additions to operations and schemas that upstream already
 * defines (a new query parameter, a new status code, new response fields).
 * Only additions are possible, and each one fails the build once upstream
 * defines the same parameter, status or property, so an extension can never
 * shadow the real definition either.
 *
 *   x-zefir-extend:
 *     operations:
 *       <operationId>: { x-zefir-source, x-zefir-until, parameters: [...], responses: {...} }
 *     schemas:
 *       <SchemaName>: { x-zefir-source, x-zefir-until, properties: {...} }
 */
export function applyExtensions(doc, extend) {
  if (extend === undefined) return;
  if (!isObject(extend)) throw new ContractError('overlay x-zefir-extend is not a mapping');
  for (const key of Object.keys(extend)) {
    if (key !== 'operations' && key !== 'schemas') {
      throw new ContractError(`overlay x-zefir-extend key "${key}" is not allowed`);
    }
  }

  for (const [operationId, entry] of Object.entries(extend.operations ?? {})) {
    const label = `operations.${operationId}`;
    requireExtensionKeys(entry, label, OPERATION_EXTENSION_KEYS);
    const op = findOperation(doc, operationId);
    if (op === undefined) throw new ContractError(`overlay extension target operation ${operationId} does not exist`);
    for (const parameter of entry.parameters ?? []) {
      if (!isObject(parameter) || typeof parameter.name !== 'string' || typeof parameter.in !== 'string') {
        throw new ContractError(`overlay extension ${label} has a parameter without name and in`);
      }
      const exists = (op.parameters ?? []).some((p) => p?.name === parameter.name && p?.in === parameter.in);
      if (exists) {
        throw new ContractError(
          `upstream now defines ${parameter.in} parameter ${parameter.name} on ${operationId}; delete it from the overlay extension`,
        );
      }
      op.parameters = [...(op.parameters ?? []), structuredClone(parameter)];
    }
    for (const [status, response] of Object.entries(entry.responses ?? {})) {
      op.responses ??= {};
      if (status in op.responses) {
        throw new ContractError(
          `upstream now defines response ${status} on ${operationId}; delete it from the overlay extension`,
        );
      }
      op.responses[status] = structuredClone(response);
    }
  }

  for (const [name, entry] of Object.entries(extend.schemas ?? {})) {
    const label = `schemas.${name}`;
    requireExtensionKeys(entry, label, SCHEMA_EXTENSION_KEYS);
    const schema = doc.components?.schemas?.[name];
    if (!isObject(schema)) throw new ContractError(`overlay extension target schema ${name} does not exist`);
    for (const [property, value] of Object.entries(entry.properties ?? {})) {
      schema.properties ??= {};
      if (property in schema.properties) {
        throw new ContractError(
          `upstream now defines ${name}.${property}; delete it from the overlay extension`,
        );
      }
      schema.properties[property] = structuredClone(value);
    }
  }
}

function decodePointerToken(token) {
  return token.replace(/~1/g, '/').replace(/~0/g, '~');
}

function collectRefs(node, out) {
  if (Array.isArray(node)) {
    for (const child of node) collectRefs(child, out);
  } else if (isObject(node)) {
    for (const [key, value] of Object.entries(node)) {
      if (key === '$ref' && typeof value === 'string') out.push(value);
      else collectRefs(value, out);
    }
  }
}

/**
 * Keeps only the allowlisted operations and the transitive closure of the
 * components they reference. Security schemes are always kept (the document's
 * top-level `security` names them without a $ref).
 */
export function prune(doc, operationIds) {
  const wanted = new Set(operationIds);
  const found = new Set();
  const paths = {};
  const usedTags = new Set();
  const pending = [];

  for (const [path, item] of Object.entries(doc.paths ?? {})) {
    const kept = {};
    for (const method of HTTP_METHODS) {
      const op = item[method];
      if (op && wanted.has(op.operationId)) {
        kept[method] = op;
        found.add(op.operationId);
        for (const tag of op.tags ?? []) usedTags.add(tag);
      }
    }
    if (Object.keys(kept).length === 0) continue;
    for (const [key, value] of Object.entries(item)) {
      if (!HTTP_METHODS.includes(key)) kept[key] = value;
    }
    paths[path] = kept;
    collectRefs(kept, pending);
  }

  const missing = [...wanted].filter((id) => !found.has(id));
  if (missing.length > 0) {
    throw new ContractError(`client-operations.json lists unknown operationIds: ${missing.join(', ')}`);
  }

  const components = {};
  if (doc.components?.securitySchemes) {
    components.securitySchemes = structuredClone(doc.components.securitySchemes);
  }
  const visited = new Set();
  while (pending.length > 0) {
    const ref = pending.pop();
    if (visited.has(ref)) continue;
    visited.add(ref);
    const match = /^#\/components\/([^/]+)\/([^/]+)$/.exec(ref);
    if (!match) throw new ContractError(`unsupported $ref "${ref}" (only local component refs are allowed)`);
    const section = decodePointerToken(match[1]);
    const name = decodePointerToken(match[2]);
    const target = doc.components?.[section]?.[name];
    if (target === undefined) throw new ContractError(`dangling $ref "${ref}"`);
    components[section] ??= {};
    components[section][name] = target;
    collectRefs(target, pending);
  }

  const sortedComponents = {};
  for (const section of Object.keys(components).sort()) {
    sortedComponents[section] = {};
    for (const name of Object.keys(components[section]).sort()) {
      sortedComponents[section][name] = structuredClone(components[section][name]);
    }
  }

  const sortedPaths = {};
  for (const path of Object.keys(paths).sort()) sortedPaths[path] = structuredClone(paths[path]);

  const out = {
    openapi: doc.openapi,
    info: structuredClone(doc.info),
    servers: structuredClone(doc.servers),
    security: structuredClone(doc.security),
  };
  if (Array.isArray(doc.tags)) {
    out.tags = doc.tags.filter((tag) => usedTags.has(tag.name)).map((tag) => structuredClone(tag));
  }
  out.paths = sortedPaths;
  out.components = sortedComponents;
  return out;
}

function readClientOperations(path = CLIENT_OPERATIONS) {
  const parsed = JSON.parse(readFileSync(path, 'utf8'));
  if (!Array.isArray(parsed.operations) || parsed.operations.some((id) => typeof id !== 'string')) {
    throw new ContractError('client-operations.json must have an "operations" array of strings');
  }
  return parsed.operations;
}

/** verify → merge → prune. Returns the pruned document and the input hashes. */
export function buildContract() {
  const upstreamHash = verifyPin();
  const overlayText = readFileSync(OVERLAY_SPEC, 'utf8');
  const operationsText = readFileSync(CLIENT_OPERATIONS, 'utf8');
  const upstream = parseYaml(readFileSync(UPSTREAM_SPEC, 'utf8'), 'contract/elitea-v2.yaml');
  const overlay = parseYaml(overlayText, 'contract/overlay/native-gap.yaml');
  const merged = mergeOverlay(upstream, overlay);
  const pruned = prune(merged, readClientOperations());
  return {
    doc: pruned,
    inputs: {
      upstream: upstreamHash,
      overlay: sha256Hex(overlayText),
      operations: sha256Hex(operationsText),
    },
  };
}

function writeMerged(doc) {
  mkdirSync(dirname(MERGED_SPEC), { recursive: true });
  writeFileSync(MERGED_SPEC, YAML.stringify(doc, { lineWidth: 0 }));
}

export async function generateTypes(doc, inputs) {
  const { default: openapiTS, astToString } = await import('openapi-typescript');
  const ast = await openapiTS(structuredClone(doc), { silent: true, alphabetize: true });
  const header = [
    '/**',
    ' * GENERATED FILE: do not edit. Regenerate with `npm run contract:gen`.',
    ' *',
    ' * Types derived from the pinned server contract (contract/elitea-v2.yaml,',
    ' * Apache-2.0, see contract/LICENSE.elitea) merged with the Agent Zefir',
    ' * native-gap overlay and pruned to contract/client-operations.json.',
    ' *',
    ` * upstream sha256:    ${inputs.upstream}`,
    ` * overlay sha256:     ${inputs.overlay}`,
    ` * operations sha256:  ${inputs.operations}`,
    ' */',
    '',
  ].join('\n');
  return `${header}${astToString(ast)}`;
}

const COMMANDS = {
  verify() {
    const hash = verifyPin();
    console.log(`contract: elitea-v2.yaml matches pinned sha256 ${hash}`);
  },
  build() {
    const { doc } = buildContract();
    writeMerged(doc);
    console.log(`contract: wrote ${relative(ROOT, MERGED_SPEC)} (${Object.keys(doc.paths).length} paths)`);
  },
  async gen() {
    const { doc, inputs } = buildContract();
    writeMerged(doc);
    const source = await generateTypes(doc, inputs);
    mkdirSync(dirname(GENERATED_FILE), { recursive: true });
    writeFileSync(GENERATED_FILE, source);
    console.log(`contract: wrote ${relative(ROOT, GENERATED_FILE)} (${source.length} bytes)`);
  },
  async check() {
    const { doc, inputs } = buildContract();
    const expected = await generateTypes(doc, inputs);
    let actual = '';
    try {
      actual = readFileSync(GENERATED_FILE, 'utf8');
    } catch {
      throw new ContractError(`${relative(ROOT, GENERATED_FILE)} is missing; run npm run contract:gen`);
    }
    if (actual !== expected) {
      throw new ContractError(
        `${relative(ROOT, GENERATED_FILE)} is out of date with contract/; run npm run contract:gen and commit the result`,
      );
    }
    console.log(`contract: ${relative(ROOT, GENERATED_FILE)} is up to date`);
  },
};

async function main(argv) {
  const command = argv[2];
  const run = COMMANDS[command];
  if (!run) {
    console.error(`usage: node scripts/contract.mjs <${Object.keys(COMMANDS).join('|')}>`);
    process.exit(2);
  }
  try {
    await run();
  } catch (error) {
    if (error instanceof ContractError) {
      console.error(`contract ${command}: ${error.message}`);
      process.exit(1);
    }
    throw error;
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main(process.argv);
}
