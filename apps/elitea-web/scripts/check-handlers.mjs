#!/usr/bin/env node
/**
 * check-handlers.mjs — R-M2 enforcement (spec §6.5, unit M1).
 *
 * R-M2: "Every MSW handler lives in `src/test/msw/handlers/` or is generated
 * by orval. A hand-written handler answers with a generated contract mock
 * (`get<Op>ResponseMock()`, the operation's v2.yaml example) or a typed
 * module constant." The hand-written JSON fixtures that used to sit between
 * the two were retired 2026-10-23: their bodies became response examples in
 * v2.yaml (checked against the schema by scripts/check-spec-examples.mjs) or
 * typed constants next to their handler (checked by tsc).
 *
 * This script walks ONLY `src/test/msw/handlers/**` and runs every `.ts`
 * file's source through `checkHandlerSource` (scripts/lib/mock-rules-core.mjs),
 * which flags an inline object/array literal body
 * (`HttpResponse.json({...})`, `Response.json`/`.text`/`.xml`/`.html`), an
 * untyped object/array module constant, and any `*.json` import.
 *
 * `src/shared/api/generated/**\/*.msw.ts` (orval's own output, "Do not edit
 * manually") is DELIBERATELY OUT OF SCOPE — it is the other side of R-M2.
 *
 * Usage: node scripts/check-handlers.mjs [--root <dir>]
 *
 * RED/GREEN: scripts/check-gates-selftest.mjs section 7 runs the same
 * function over tools/lint-rules/fixtures/msw/handlers-{bad,good}.ts.
 */
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import process from 'node:process';

import { checkHandlerSource } from './lib/mock-rules-core.mjs';

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url));
const APP_DIR = resolve(SCRIPT_DIR, '..');
const HANDLER_FILE_RE = /\.tsx?$/;

function parseArgs(argv) {
  const args = { root: APP_DIR };
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--root') {
      const value = argv[i + 1];
      if (!value) throw new Error('--root requires a directory argument');
      args.root = resolve(value);
      i++;
    } else {
      throw new Error(`unknown argument: ${argv[i]}`);
    }
  }
  return args;
}

function* handlerFiles(dir) {
  let entries;
  try {
    entries = readdirSync(dir);
  } catch {
    return;
  }
  for (const entry of entries) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      yield* handlerFiles(full);
    } else if (HANDLER_FILE_RE.test(entry)) {
      yield full;
    }
  }
}

function main() {
  const { root } = parseArgs(process.argv.slice(2));
  const handlersDir = join(root, 'src', 'test', 'msw', 'handlers');
  let files = 0;
  const findings = [];

  for (const file of handlerFiles(handlersDir)) {
    files++;
    const rel = relative(root, file);
    findings.push(...checkHandlerSource(rel, readFileSync(file, 'utf8')));
  }

  if (files === 0) {
    console.error(`check-handlers: found 0 files under ${relative(root, handlersDir)} — refusing to pass vacuously`);
    process.exit(2);
  }

  for (const f of findings) {
    console.error(`${f.rule} ${f.file}:${f.line} — ${f.message}`);
  }
  if (findings.length > 0) {
    console.error(`check-handlers: ${findings.length} R-M2 violation(s) in ${files} file(s) — failing`);
    process.exit(1);
  }
  console.log(`check-handlers: OK — ${files} handler file(s) under src/test/msw/handlers/ answer with generated mocks or typed constants (R-M2)`);
}

main();
