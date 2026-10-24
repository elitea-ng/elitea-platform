#!/usr/bin/env node
/**
 * check-spec-examples.mjs — every `example`/`examples` value in the public
 * API contract (services/elitea-main/api/openapi/v2.yaml) satisfies its
 * schema.
 *
 * The web test mocks are generated from the contract with orval's
 * `useExamples: true` (orval.config.ts), so a response example is the body a
 * test receives. A wrong example would make tests pass against a shape the
 * server never sends; this gate is what stops that. The decision logic and
 * its unit tests live in scripts/lib/spec-examples-core.mjs; the RED/GREEN
 * proof is in scripts/check-gates-selftest.mjs.
 *
 * Usage: node scripts/check-spec-examples.mjs [--spec <path>]
 */
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { dirname, relative, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { checkSpecExamples } from './lib/spec-examples-core.mjs';

const require = createRequire(import.meta.url);
const yaml = require('js-yaml');

const APP_DIR = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const DEFAULT_SPEC = resolve(APP_DIR, '../../services/elitea-main/api/openapi/v2.yaml');

function parseArgs(argv) {
  if (argv.length === 0) return DEFAULT_SPEC;
  if (argv[0] === '--spec' && argv[1] && argv.length === 2) return resolve(argv[1]);
  throw new Error(`usage: check-spec-examples.mjs [--spec <path>] (got: ${argv.join(' ')})`);
}

const specPath = parseArgs(process.argv.slice(2));
const spec = yaml.load(readFileSync(specPath, 'utf8'));
const { checked, findings } = checkSpecExamples(spec);
const name = relative(process.cwd(), specPath);

if (checked === 0) {
  console.error(`check-spec-examples: ${name} has no examples at all — refusing to pass vacuously`);
  process.exit(2);
}
for (const f of findings) console.error(`${name} ${f.pointer} — ${f.message}`);
if (findings.length > 0) {
  console.error(`check-spec-examples: ${findings.length} of ${checked} example(s) invalid — failing`);
  process.exit(1);
}
console.log(`check-spec-examples: OK — ${checked} example(s) in ${name} satisfy their schemas`);
