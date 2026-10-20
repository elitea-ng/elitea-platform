#!/usr/bin/env node
/**
 * Contract tooling: the mobile client is generated from the server's OpenAPI document IN THIS
 * REPOSITORY (services/elitea-main/api/openapi/v2.yaml), pruned to the operations the app
 * declares in contract/client-operations.json.
 *
 *   node scripts/contract.mjs build    prune the spec, write contract/.build/pruned.yaml (inspection)
 *   node scripts/contract.mjs gen      build, then write src/api/generated/elitea.gen.ts
 *   node scripts/contract.mjs check    build + generate in memory and fail if the committed
 *                                      generated file differs (CI drift guard; also
 *                                      scripts/check-generated-client.mjs)
 *
 * There is no pinned copy and no overlay: a gap in the spec is fixed in the spec, in the same
 * change as the app code that needs it. The generated file depends only on the PRUNED document,
 * so a spec change to operations the app does not use leaves it untouched.
 *
 * Every command exits non-zero with a one-line reason on failure.
 */
import { createHash } from 'node:crypto';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import YAML from 'yaml';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
/** The monorepo root (apps/elitea-mobile/../..). */
export const REPO_ROOT = resolve(ROOT, '..', '..');
export const SPEC = join(REPO_ROOT, 'services', 'elitea-main', 'api', 'openapi', 'v2.yaml');
export const CONTRACT_DIR = join(ROOT, 'contract');
export const CLIENT_OPERATIONS = join(CONTRACT_DIR, 'client-operations.json');
export const PRUNED_SPEC = join(CONTRACT_DIR, '.build', 'pruned.yaml');
export const GENERATED_FILE = join(ROOT, 'src', 'api', 'generated', 'elitea.gen.ts');

const HTTP_METHODS = ['get', 'put', 'post', 'delete', 'options', 'head', 'patch', 'trace'];

export class ContractError extends Error {}

export function sha256Hex(bytes) {
  return createHash('sha256').update(bytes).digest('hex');
}

export function parseYaml(text, label = 'document') {
  try {
    return YAML.parse(text, { maxAliasCount: -1 });
  } catch (error) {
    throw new ContractError(`${label} is not valid YAML: ${error.message.split('\n')[0]}`);
  }
}

const isObject = (value) => value !== null && typeof value === 'object' && !Array.isArray(value);

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

export function readClientOperations(path = CLIENT_OPERATIONS) {
  const parsed = JSON.parse(readFileSync(path, 'utf8'));
  if (!Array.isArray(parsed.operations) || parsed.operations.some((id) => typeof id !== 'string')) {
    throw new ContractError('client-operations.json must have an "operations" array of strings');
  }
  return parsed.operations;
}

/**
 * Reads the spec and prunes it to the declared operations. `info` is reduced to the fields the
 * client reads (title and the client contract version), so a version bump or a description
 * edit of the server does not churn the generated file.
 */
export function buildContract({ specPath = SPEC, operationsPath = CLIENT_OPERATIONS } = {}) {
  let specText;
  try {
    specText = readFileSync(specPath, 'utf8');
  } catch {
    throw new ContractError(`${specPath} is missing (the app must sit at apps/elitea-mobile in the monorepo)`);
  }
  const spec = parseYaml(specText, relative(REPO_ROOT, specPath));
  const pruned = prune(spec, readClientOperations(operationsPath));
  pruned.info = {
    title: spec.info?.title,
    version: 'monorepo',
    'x-elitea-client-contract': spec.info?.['x-elitea-client-contract'],
  };
  return { doc: pruned, inputs: { operations: sha256Hex(readFileSync(operationsPath)) } };
}

function writePruned(doc) {
  mkdirSync(dirname(PRUNED_SPEC), { recursive: true });
  writeFileSync(PRUNED_SPEC, YAML.stringify(doc, { lineWidth: 0 }));
}

export async function generateTypes(doc, inputs) {
  const { default: openapiTS, astToString } = await import('openapi-typescript');
  const ast = await openapiTS(structuredClone(doc), { silent: true, alphabetize: true });
  const header = [
    '/**',
    ' * GENERATED FILE: do not edit. Regenerate with `npm run contract:gen`.',
    ' *',
    ' * Types derived from services/elitea-main/api/openapi/v2.yaml (this repository), pruned to',
    ' * contract/client-operations.json. scripts/check-generated-client.mjs fails when it is stale.',
    ' *',
    ` * client contract:    ${doc.info['x-elitea-client-contract']}`,
    ` * operations sha256:  ${inputs.operations}`,
    ' */',
    '',
  ].join('\n');
  return `${header}${astToString(ast)}`;
}

/** The expected generated source and the committed one (`null` when missing). */
export async function compareGenerated() {
  const { doc, inputs } = buildContract();
  const expected = await generateTypes(doc, inputs);
  let actual = null;
  try {
    actual = readFileSync(GENERATED_FILE, 'utf8');
  } catch {
    actual = null;
  }
  return { expected, actual, paths: Object.keys(doc.paths).length };
}

export function writeGenerated(source) {
  mkdirSync(dirname(GENERATED_FILE), { recursive: true });
  writeFileSync(GENERATED_FILE, source);
}

export const GENERATED_RELATIVE = relative(ROOT, GENERATED_FILE);

const COMMANDS = {
  build() {
    const { doc } = buildContract();
    writePruned(doc);
    console.log(`contract: wrote ${relative(ROOT, PRUNED_SPEC)} (${Object.keys(doc.paths).length} paths)`);
  },
  async gen() {
    const { expected } = await compareGenerated();
    writeGenerated(expected);
    console.log(`contract: wrote ${GENERATED_RELATIVE} (${expected.length} bytes)`);
  },
  async check() {
    const { expected, actual } = await compareGenerated();
    if (actual === null) throw new ContractError(`${GENERATED_RELATIVE} is missing; run npm run contract:gen`);
    if (actual !== expected) {
      throw new ContractError(
        `${GENERATED_RELATIVE} is out of date with services/elitea-main/api/openapi/v2.yaml; run npm run contract:gen and commit the result`,
      );
    }
    console.log(`contract: ${GENERATED_RELATIVE} is up to date`);
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
