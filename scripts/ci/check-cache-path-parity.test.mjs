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
  // Once as a family whose reader is not its writer, once as one directory spelled two ways.
  assert.equal(errors.length, 2);
  assert.match(errors[0], /^key family node-cache-: .*"\/home\/runner\/\.npm".*its writer.*"~\/\.npm"/);
  assert.match(errors[1], /^the directory ~\/\.npm is spelled 2 ways/);
});

test("the same paths in another order are refused (the version hash is order-sensitive)", () => {
  const errors = errorsOf({
    ".github/workflows/w.yml": workflow(save("go-v1-a", ["~/a", "~/b"]) + restore("go-v1-a", ["~/b", "~/a"])),
  });
  assert.equal(errors.length, 1);
});

test("a trailing slash is a different string and is refused", () => {
  const errors = errorsOf({ ".github/workflows/w.yml": workflow(save("go-v1-a", "~/.npm") + restore("go-v1-a", "~/.npm/")) });
  assert.equal(errors.length, 2);
  assert.match(errors[1], /^the directory ~\/\.npm is spelled 2 ways/);
});

test("indentation and blank lines do not count (actions/cache trims and drops them)", () => {
  const errors = errorsOf({
    ".github/workflows/w.yml": workflow(save("go-v1-a", ["~/a", "", "~/b", ""]) + restore("go-v1-a", ["~/a", "  ~/b  "])),
  });
  assert.deepEqual(errors, []);
});

test("a composite's inputs.key is followed to each caller's key, kept to the step's slot condition", () => {
  // The go reader drops a line; the npm steps are right and must not be compared with go's.
  const errors = errorsOf({
    ".github/actions/slots/action.yml": slotAction({ goReader: ["~/.cache/go-build"] }),
    ".github/workflows/c.yml": callers,
  });
  assert.equal(errors.length, 1);
  assert.match(errors[0], /^key family go-v1-: .*reader-go.*its writer.*writer-go/);
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
      "- id: gocache\n  uses: ./.github/actions/gokey\n" + save("${{ steps.gocache.outputs.key }}", "~/.cache/go-build") + restore("go-v1-${{ runner.os }}-z", "~/.cache/go-build-other"),
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

test("one wrong reader is one error, against the family's writer, however many steps share the family", () => {
  const k = "go-v1-${{ runner.os }}";
  const steps = save(k, "~/go") + restore(k, "~/go") + restore(k, "~/go") + restore(k, "~/other");
  const errors = errorsOf({ ".github/workflows/w.yml": workflow(steps) });
  assert.equal(errors.length, 1);
  assert.match(errors[0], /"~\/other" but its writer .*actions\/cache\/save/);
});

test("a directory spelled two ways is refused even when the keys never resolve to one family", () => {
  // A matrix key resolves to no literal, so the family rule never meets go-v1-; the directory rule does.
  const files = {
    ".github/workflows/a.yml": workflow(save("go-v1-a", "~/go/pkg/mod")),
    ".github/workflows/b.yml": workflow(restore("${{ matrix.key }}", "/home/runner/go/pkg/mod")),
  };
  const errors = errorsOf(files);
  assert.equal(errors.length, 1);
  assert.match(errors[0], /^the directory ~\/go\/pkg\/mod is spelled 2 ways: "~\/go\/pkg\/mod" .*"\/home\/runner\/go\/pkg\/mod"/);
});

test("a composite key built from an input it cannot follow is refused, not guessed", () => {
  const action = "inputs:\n  key: {required: true}\nruns:\n  using: composite\n  steps:\n  - uses: actions/cache/restore@v6\n    with:\n      path: ~/.npm\n      key: ${{ runner.os }}-${{ inputs.key }}\n";
  const errors = errorsOf({
    ".github/actions/k/action.yml": action,
    ".github/workflows/c.yml": workflow("- uses: ./.github/actions/k\n  with: {key: 'node-cache-x'}\n"),
  });
  assert.equal(errors.length, 1);
  assert.match(errors[0], /cannot follow to a literal start/);
});

test("directoryOf reads every home spelling as one directory", () => {
  for (const line of ["~/.npm", "$HOME/.npm", "${HOME}/.npm", "/home/runner/.npm", "/Users/runner/.npm", "C:\\Users\\runneradmin\\.npm", "~/.npm/"]) {
    assert.equal(checker.directoryOf(line), "~/.npm", line);
  }
  assert.equal(checker.directoryOf(".buildkit-mounts"), ".buildkit-mounts");
});

/** A workflow whose one job runs in a container with these steps. */
const containerWorkflow = (steps) => `on: schedule\njobs:\n  sb:\n    runs-on: ubuntu-latest\n    container:\n      image: mcr.microsoft.com/playwright:v1.62.1-noble\n    steps:\n${steps.replace(/^(?=.*\S)/gm, "      ")}`;

/** A main-write-cache-shaped composite with a `save` input (its writer's gate is left out: the rule reads the declared input, not the `if:`). */
const gatedAction = `inputs:\n  key: {required: true}\n  save: {required: false, default: "true"}\nruns:\n  using: composite\n  steps:\n  - id: writer\n    uses: actions/cache@v6\n    with:\n      path: ~/.npm\n      key: \${{ inputs.key }}\n  - id: reader\n    uses: actions/cache/restore@v6\n    with:\n      path: ~/.npm\n      key: \${{ inputs.key }}\n`;
/** npm-cache-shaped: a wrapper with no `save` input around the gated action. */
const wrapper = `inputs:\n  lock: {required: true}\nruns:\n  using: composite\n  steps:\n  - uses: ./.github/actions/gated\n    with:\n      key: node-cache-x\n`;

test("a containerised job that saves through local actions is refused (the gzip Storybook writer of 2026-11-21)", () => {
  const errors = errorsOf({
    ".github/actions/gated/action.yml": gatedAction,
    ".github/actions/npm/action.yml": wrapper,
    ".github/workflows/web.yml": containerWorkflow("- uses: ./.github/actions/npm\n  with: {lock: package-lock.json}\n"),
  });
  assert.equal(errors.length, 1, errors.join("\n"));
  assert.match(errors[0], /web\.yml \[sb\]: runs in a job `container:` and saves a cache \(\.github\/actions\/npm\/action\.yml → \.github\/actions\/gated\/action\.yml → 'writer'\)/);
});

test("a containerised job with a direct actions/cache save is refused", () => {
  const errors = errorsOf({ ".github/workflows/web.yml": containerWorkflow(save("node-cache-a", "~/.npm")) });
  assert.equal(errors.length, 1, errors.join("\n"));
  assert.match(errors[0], /container/);
});

test("a containerised job that restores only, or passes save: \"false\" to an action that declares it, passes", () => {
  assert.deepEqual(
    errorsOf({
      ".github/actions/gated/action.yml": gatedAction,
      ".github/workflows/web.yml": containerWorkflow(`- uses: ./.github/actions/gated\n  with: {key: node-cache-x, save: "false"}\n${restore("node-cache-x", "~/.npm")}`),
    }),
    [],
  );
});

test("save: \"false\" to an action that declares no save input is not trusted", () => {
  const errors = errorsOf({
    ".github/actions/gated/action.yml": gatedAction,
    ".github/actions/npm/action.yml": wrapper,
    ".github/workflows/web.yml": containerWorkflow(`- uses: ./.github/actions/npm\n  with: {lock: package-lock.json, save: "false"}\n`),
  });
  assert.equal(errors.length, 1, errors.join("\n"));
});

test("the same saving job on the host passes", () => {
  assert.deepEqual(errorsOf({ ".github/workflows/web.yml": workflow(save("node-cache-a", "~/.npm")) }), []);
});
