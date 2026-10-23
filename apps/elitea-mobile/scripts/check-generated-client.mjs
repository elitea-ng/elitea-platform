#!/usr/bin/env node
/**
 * Fails when `src/api/generated/elitea.gen.ts` does not match what openapi-typescript produces
 * from `services/elitea-main/api/openapi/v2.yaml`, pruned to `contract/client-operations.json`.
 * The mobile twin of apps/elitea-web/scripts/check-generated-client.mjs.
 *
 * Generated code is a pure function of its input. A stale client fails OPEN: it keeps
 * type-checking calls to operations or fields the server no longer has, and the app breaks only
 * against a real server. The comparison is in memory against a clean generation, so a dirty
 * working tree cannot hide drift, and the generator writes exactly one file, so there is no
 * orphan to miss.
 *
 * The file only depends on the PRUNED spec: editing an operation the app does not use does not
 * make it stale. Editing one it uses does, on purpose.
 *
 * Usage (from apps/elitea-mobile):
 *   node scripts/check-generated-client.mjs           # check, read-only
 *   node scripts/check-generated-client.mjs --write   # regenerate (same as npm run contract:gen)
 */
import process from 'node:process';

import { ContractError, GENERATED_RELATIVE, compareGenerated, writeGenerated } from './contract.mjs';

/**
 * Floor on the operations kept (2026-10-20: 41 paths). A prune that keeps nothing would
 * generate an empty client that "matches" an empty committed file.
 */
const MIN_PATHS = 30;

async function main() {
  const args = process.argv.slice(2);
  const unknown = args.filter((arg) => arg !== '--write');
  if (unknown.length > 0) {
    // A typo must not read as check mode and pass.
    console.error(`check-generated-client: unknown argument(s): ${unknown.join(', ')}. Use --write or nothing.`);
    process.exit(1);
  }
  const write = args.includes('--write');

  const { expected, actual, paths } = await compareGenerated();
  if (paths < MIN_PATHS) {
    console.error(`check-generated-client: the pruned spec keeps ${paths} paths, under the floor of ${MIN_PATHS}; the gate would compare nothing.`);
    process.exit(1);
  }
  if (actual === expected) {
    console.log(`check-generated-client: OK — ${GENERATED_RELATIVE} matches the spec (${paths} paths).`);
    return;
  }
  if (write) {
    writeGenerated(expected);
    console.log(`check-generated-client: wrote ${GENERATED_RELATIVE} (${paths} paths). Commit the result.`);
    return;
  }
  console.error(
    `check-generated-client: ${GENERATED_RELATIVE} ${actual === null ? 'is missing' : 'is STALE'} against\n` +
      'services/elitea-main/api/openapi/v2.yaml (pruned to contract/client-operations.json).\n\n' +
      'Fix: run `node scripts/check-generated-client.mjs --write` (or `npm run contract:gen`) in\n' +
      'apps/elitea-mobile, then typecheck and commit the result.',
  );
  process.exit(1);
}

try {
  await main();
} catch (error) {
  if (error instanceof ContractError) {
    console.error(`check-generated-client: ${error.message}`);
    process.exit(1);
  }
  throw error;
}
