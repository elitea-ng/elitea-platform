// Scenarios for check-e2e-paths.mjs (new with the port: the Python version
// had none). The matcher and the Containerfile reader decide what the check
// believes the E2E lanes depend on, so they are held here.

import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import { containerfileInputs, covered, importSpecifiers, scriptDependencies, scriptReferences } from "./check-e2e-paths.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

test("a directory input is covered only by a pattern that takes the whole directory", () => {
  assert.equal(covered("libs/go", ["libs/go/**"]), true);
  assert.equal(covered("libs/go", ["libs/**"]), true);
  assert.equal(covered("libs/go", ["libs/go/*.go"]), false);
  assert.equal(covered("scripts/ci/e2e-lane-filter.sh", ["scripts/ci/*.sh"]), true);
  assert.equal(covered("scripts/ci/e2e-lane-filter.sh", ["scripts/ci/e2e-lane-filter.sh"]), true);
  assert.equal(covered("scripts/ci/e2e-lane-filter.sh", ["scripts/**/other.sh"]), false);
});

test("COPY and ADD sources are inputs; --from, heredocs and URLs are not", () => {
  const dir = fs.mkdtempSync(path.join(ROOT, "deploy", ".e2e-paths-test-"));
  try {
    const file = path.join(dir, "Containerfile");
    fs.writeFileSync(
      file,
      [
        "FROM debian AS base",
        "COPY a.txt \\",
        "     b/ /dst/",
        "copy --chown=1:1 c.txt /dst/",
        "COPY --from=base /x /y",
        "ADD https://example.invalid/z.tgz /z",
        "COPY <<EOF /etc/x",
        "EOF",
        "RUN echo COPY d.txt",
      ].join("\n"),
    );
    const rel = path.relative(ROOT, dir);
    assert.deepEqual([...containerfileInputs(dir, file)].sort(), [
      `${rel}/Containerfile`,
      `${rel}/a.txt`,
      `${rel}/b`,
      `${rel}/c.txt`,
    ]);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("a build target reads only the stages it runs", () => {
  const dir = fs.mkdtempSync(path.join(ROOT, "deploy", ".e2e-paths-test-"));
  try {
    const file = path.join(dir, "Containerfile");
    fs.writeFileSync(
      file,
      [
        "FROM node AS app-builder",
        "COPY app/ /app/",
        "FROM node AS docs-builder",
        "COPY docs/ /docs/",
        "FROM node AS assets",
        "COPY assets/ /assets/",
        "FROM nginx AS spa",
        "COPY --from=app-builder /app/dist /html",
        "COPY --from=assets /assets /html/assets",
        "COPY spa.conf /etc/nginx/",
        "FROM spa",
        "COPY --from=docs-builder /docs/dist /html/docs",
      ].join("\n"),
    );
    const rel = path.relative(ROOT, dir);
    const read = (target) => [...containerfileInputs(dir, file, target)].sort();
    // The default (last) stage reaches every stage through FROM spa and --from.
    assert.deepEqual(read(undefined), [`${rel}/Containerfile`, `${rel}/app`, `${rel}/assets`, `${rel}/docs`, `${rel}/spa.conf`]);
    // `spa` never runs docs-builder, so docs/ is not its input.
    assert.deepEqual(read("spa"), [`${rel}/Containerfile`, `${rel}/app`, `${rel}/assets`, `${rel}/spa.conf`]);
    assert.deepEqual(read("docs-builder"), [`${rel}/Containerfile`, `${rel}/docs`]);
    // A target the file does not have is an error, not an empty read.
    assert.throws(() => read("nope"), /has no stage 'nope'/);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("script references are scripts/ paths ending .sh, .py or .mjs", () => {
  const text = 'run: bash scripts/ci/x.sh && node "${GITHUB_WORKSPACE}/scripts/ci/y.mjs" scripts/z.py scripts/ci/notes.md';
  assert.deepEqual([...scriptReferences(text)].sort(), ["scripts/ci/x.sh", "scripts/ci/y.mjs", "scripts/z.py"]);
});

test("import specifiers are read from code, not from comments", () => {
  const text = [
    "#!/usr/bin/env node",
    "// import { x } from \"./in-a-comment.mjs\";",
    " * import(\"./in-jsdoc.mjs\")",
    'import fs from "node:fs";',
    "import {",
    "  a,",
    "  b,",
    '} from "./multi.mjs";',
    'import "./side-effect.mjs";',
    "export { c } from './re-export.mjs';",
    'export * from "./star.mjs";',
    'const late = await import("./dynamic.mjs");',
    'const text = "import nothing from \\"./in-a-string.mjs\\"";',
  ].join("\n");
  assert.deepEqual(importSpecifiers(text), ["node:fs", "./multi.mjs", "./side-effect.mjs", "./re-export.mjs", "./star.mjs", "./dynamic.mjs"]);
});

function tree(files) {
  const root = fs.mkdtempSync(path.join(ROOT, "scripts", ".e2e-deps-test-"));
  for (const [file, text] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true });
    fs.writeFileSync(path.join(root, file), text);
  }
  return root;
}

test("a referenced script's relative imports are inputs, transitively", () => {
  const root = tree({
    "ci/a.mjs": 'import { b } from "./b.mjs";\nimport fs from "node:fs";\n',
    "ci/b.mjs": 'export { c } from "../lib/c.mjs";\n',
    "lib/c.mjs": 'import "./d.mjs";\n',
    "lib/d.mjs": "export const d = 1;\n",
    "ci/package.json": "{}",
  });
  try {
    const deps = scriptDependencies(new Set(["ci/a.mjs", "ci/run.sh"]), root);
    assert.deepEqual([...deps.keys()].sort(), ["ci/b.mjs", "lib/c.mjs", "lib/d.mjs"]);
    assert.ok([...deps.values()].every((via) => via === "ci/a.mjs"));
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("a bare package import makes the governing package.json and lockfile inputs", () => {
  const root = tree({
    "ci/a.mjs": 'import { b } from "./b.mjs";\n',
    "ci/b.mjs": 'import YAML from "yaml";\n',
    "ci/package.json": "{}",
    "ci/package-lock.json": "{}",
    "ci/plain.mjs": 'import path from "node:path";\n',
  });
  try {
    assert.deepEqual([...scriptDependencies(new Set(["ci/a.mjs"]), root).keys()].sort(), ["ci/b.mjs", "ci/package-lock.json", "ci/package.json"]);
    // A script that loads only node built-ins needs no manifest.
    assert.deepEqual([...scriptDependencies(new Set(["ci/plain.mjs"]), root).keys()], []);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("an import that resolves to nothing fails the read", () => {
  const root = tree({ "ci/a.mjs": 'import { gone } from "./gone.mjs";\n', "ci/b.mjs": 'import x from "pkg";\n' });
  try {
    assert.throws(() => scriptDependencies(new Set(["ci/a.mjs"]), root), /ci\/gone\.mjs does not exist/);
    assert.throws(() => scriptDependencies(new Set(["ci/b.mjs"]), root), /no package\.json governs it/);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("on the real tree the stack-image script needs its py-compat helper", () => {
  const deps = scriptDependencies(new Set(["scripts/ci/stack-images.mjs"]));
  assert.deepEqual([...deps.keys()], ["scripts/ci/py-compat.mjs"]);
  const yamlReader = scriptDependencies(new Set(["scripts/ci/check-workflow-cache-writes.mjs"]));
  for (const dep of ["scripts/ci/ci-compat.mjs", "scripts/ci/py-compat.mjs", "scripts/ci/package.json", "scripts/ci/package-lock.json"]) {
    assert.ok(yamlReader.has(dep), dep);
  }
});
