// Scenarios for check-cache-path-parity.mjs.
//
// Each negative case is a pair of cache steps whose versions differ although
// their keys match, so the restore misses what the save wrote. The first one is
// the shape main had until 2026-11-20: a `~/.npm` writer and an expression
// reader that resolved to `/home/runner/.npm`.

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import { loadYaml } from "./ci-compat.mjs";
import * as checker from "./check-cache-path-parity.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");
const NONE = { requiredFamilies: [] };

/** {"file": "yaml text"} → the Map check() reads. */
const docs = (files) => new Map(Object.entries(files).map(([name, text]) => [name, loadYaml(text)]));

const workflow = (steps) => `on: pull_request\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n${steps.replace(/^(?=.*\S)/gm, "      ")}`;

/** A `path:` value at `indent` spaces: a string as is, an array as a block scalar of those lines (each line is written as given, after the indent). */
const pathValue = (paths, indent) => (Array.isArray(paths) ? `|\n${paths.map((l) => (l === "" ? "" : " ".repeat(indent + 2) + l)).join("\n")}` : paths);
const restore = (key, paths) => `- uses: actions/cache/restore@v6\n  with:\n    key: ${key}\n    path: ${pathValue(paths, 4)}\n`;
const save = (key, paths) => `- uses: actions/cache/save@v6\n  if: github.ref == 'refs/heads/main' && github.event_name != 'pull_request'\n  with:\n    key: ${key}\n    path: ${pathValue(paths, 4)}\n`;

/** A main-write-cache-shaped composite: one writer and one reader per slot, with these paths. */
function slotAction({ goWriter = ["~/.cache/go-build", "~/go/pkg/mod"], goReader = goWriter, npmWriter = "~/.npm", npmReader = npmWriter } = {}) {
  const step = (id, uses, slot, p) => `  - id: ${id}\n    if: inputs.slot == '${slot}'\n    uses: ${uses}\n    with:\n      path: ${pathValue(p, 6)}\n      key: \${{ inputs.key }}\n`;
  return `inputs:\n  slot: {required: true}\n  key: {required: true}\nruns:\n  using: composite\n  steps:\n${step("writer-go", "actions/cache@v6", "go", goWriter)}${step("reader-go", "actions/cache/restore@v6", "go", goReader)}${step("writer-npm", "actions/cache@v6", "npm", npmWriter)}${step("reader-npm", "actions/cache/restore@v6", "npm", npmReader)}`;
}

const callers = workflow(
  "- uses: ./.github/actions/slots\n  with: {slot: go, key: 'go-v1-${{ runner.os }}-x'}\n" +
    "- uses: ./.github/actions/slots\n  with: {slot: npm, key: 'node-cache-${{ runner.os }}-y'}\n",
);

const errorsOf = (files, options = NONE) => checker.check(docs(files), options).errors;

test("the repository's cache steps spell their writers' paths", () => {
  const lines = [];
  assert.equal(checker.main(["--root", ROOT], (l) => lines.push(l)), 0, lines.join("\n"));
});

test("an expression reader path is refused (the shape main had: ~/.npm saved, /home/runner/.npm restored)", () => {
  const errors = errorsOf({
    ".github/actions/slots/action.yml": slotAction({ npmReader: "${{ inputs.path }}" }),
    ".github/workflows/c.yml": callers,
  });
  assert.equal(errors.length, 1);
  assert.match(errors[0], /reader-npm.*expression/);
});

test("an absolute reader path against a ~/ writer of the same key is refused", () => {
  const errors = errorsOf({
    ".github/workflows/w.yml": workflow(save("node-cache-${{ runner.os }}-a", "~/.npm") + restore("node-cache-${{ runner.os }}-a", "/home/runner/.npm")),
  });
  assert.equal(errors.length, 1);
  assert.match(errors[0], /node-cache-.*"~\/\.npm".*"\/home\/runner\/\.npm"/);
});

test("the same paths in another order are refused (the version hash is order-sensitive)", () => {
  const errors = errorsOf({
    ".github/workflows/w.yml": workflow(save("go-v1-a", ["~/a", "~/b"]) + restore("go-v1-a", ["~/b", "~/a"])),
  });
  assert.equal(errors.length, 1);
});

test("a trailing slash is a different string and is refused", () => {
  assert.equal(errorsOf({ ".github/workflows/w.yml": workflow(save("go-v1-a", "~/.npm") + restore("go-v1-a", "~/.npm/")) }).length, 1);
});

test("indentation and blank lines do not count (actions/cache trims and drops them)", () => {
  const errors = errorsOf({
    ".github/workflows/w.yml": workflow(save("go-v1-a", ["~/a", "", "~/b", ""]) + restore("go-v1-a", ["~/a", "  ~/b  "])),
  });
  assert.deepEqual(errors, []);
});

test("a composite's inputs.key is followed to each caller's key, kept to the step's slot condition", () => {
  // The go reader is wrong; the npm steps are right and must not be compared with go's.
  const errors = errorsOf({
    ".github/actions/slots/action.yml": slotAction({ goReader: ["/home/runner/.cache/go-build", "/home/runner/go/pkg/mod"] }),
    ".github/workflows/c.yml": callers,
  });
  assert.equal(errors.length, 1);
  assert.match(errors[0], /^key family go-v1-: .*writer-go.*reader-go|^key family go-v1-: .*reader-go.*writer-go/);
});

test("the repository's slot shape with matching paths passes, and resolves both required families", () => {
  const errors = errorsOf(
    { ".github/actions/slots/action.yml": slotAction(), ".github/workflows/c.yml": callers },
    { requiredFamilies: ["go-v1-", "node-cache-"] },
  );
  assert.deepEqual(errors, []);
});

test("a steps.<id>.outputs.key is followed into the local action's output", () => {
  const keyAction = "outputs:\n  key:\n    value: go-v1-${{ runner.os }}-${{ steps.x.outputs.h }}\nruns:\n  using: composite\n  steps:\n  - id: x\n    run: echo\n    shell: bash\n";
  const files = {
    ".github/actions/gokey/action.yml": keyAction,
    ".github/workflows/w.yml": workflow(
      "- id: gocache\n  uses: ./.github/actions/gokey\n" + save("${{ steps.gocache.outputs.key }}", "~/.cache/go-build") + restore("go-v1-${{ runner.os }}-z", "/home/runner/.cache/go-build"),
    ),
  };
  const errors = errorsOf(files);
  assert.equal(errors.length, 1);
  assert.match(errors[0], /^key family go-v1-:/);
});

test("overlapping families (one key starts with the other) are compared", () => {
  const errors = errorsOf({
    ".github/workflows/w.yml": workflow(save("node-cache-Linux-${{ hashFiles('x') }}", "~/.npm") + restore("node-cache-${{ runner.os }}", "~/npm")),
  });
  assert.equal(errors.length, 1);
});

test("unrelated families may cache different paths", () => {
  assert.deepEqual(errorsOf({ ".github/workflows/w.yml": workflow(save("go-v1-a", "~/go") + restore("node-cache-a", "~/.npm")) }), []);
});

test("an enableCrossOsArchive that differs is refused (it changes the version on Windows)", () => {
  const writer = save("node-cache-a", "~/.npm") + "    enableCrossOsArchive: true\n";
  assert.equal(errorsOf({ ".github/workflows/w.yml": workflow(writer + restore("node-cache-a", "~/.npm")) }).length, 1);
});

test("unresolvable keys group by file and the context they read", () => {
  const sameFile = workflow(save("${{ steps.policy.outputs.a }}", ".m") + restore("${{ steps.policy.outputs.b }}-", ".mounts"));
  assert.equal(errorsOf({ ".github/workflows/w.yml": sameFile }).length, 1);
  const otherFiles = {
    ".github/workflows/a.yml": workflow(save("${{ steps.policy.outputs.a }}", ".m")),
    ".github/workflows/b.yml": workflow(restore("${{ steps.policy.outputs.a }}", ".mounts")),
  };
  assert.deepEqual(errorsOf(otherFiles), []);
});

test("a required family with no writer or no reader fails (the resolution stopped working)", () => {
  const errors = errorsOf({ ".github/workflows/w.yml": workflow(restore("go-v1-a", "~/go")) }, { requiredFamilies: ["go-v1-"] });
  assert.equal(errors.length, 1);
  assert.match(errors[0], /go-v1-: found 0 saving and 1 restoring/);
});

test("fewer cache steps than the floor fails instead of passing", () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "cache-parity-"));
  try {
    fs.mkdirSync(path.join(root, ".github", "workflows"), { recursive: true });
    fs.writeFileSync(path.join(root, ".github", "workflows", "w.yml"), workflow(restore("x", "~/x")));
    const lines = [];
    assert.equal(checker.main(["--root", root], (l) => lines.push(l)), 1);
    assert.match(lines.join("\n"), /found 1 actions\/cache step\(s\), expected at least/);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});
