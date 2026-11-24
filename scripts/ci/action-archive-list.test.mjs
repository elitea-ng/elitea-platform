// Scenarios for action-archive-list.mjs: the text reader, the runner's cache
// layout, the resolver with a fake GitHub API, the download guard, and the
// repository itself.

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import {
  archivePath,
  downloadArchives,
  main,
  MINIMUM_REF_COUNT,
  parseRef,
  resolveArchives,
  scanRefs,
  usesRefs,
} from "./action-archive-list.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");
const SHA_A = "3d3c42e5aac5ba805825da76410c181273ba90b1";
const SHA_B = "55cc8345863c7cc4c66a329aec7e433d2d1c52a9";
const SHA_C = "9255dc7a253b0ccc959486e2bca901246202afeb";

test("remote uses lines are read, local, docker and prose are not", () => {
  const text = [
    "jobs:",
    "  a:",
    "    steps:",
    "      - uses: actions/checkout@v7",
    `      - uses: "actions/setup-go@${SHA_A}" # v7.0.0`,
    "        uses: 'actions/cache/restore@v6'",
    "      - uses: ./.github/actions/eng-runner-env",
    "      - uses: docker://alpine:3",
    "  b:",
    "    uses: elitea-ng/other/.github/workflows/x.yml@main",
    "      - run: echo 'the step uses: a token'",
    "      # - uses: actions/old@v1",
    "",
  ].join("\n");
  assert.deepEqual(usesRefs(text), ["actions/checkout@v7", `actions/setup-go@${SHA_A}`, "actions/cache/restore@v6"]);
});

test("a ref splits into repository, sub-path and ref", () => {
  assert.deepEqual(parseRef("actions/cache/restore@v6"), { repo: "actions/cache", path: "restore", ref: "v6" });
  assert.deepEqual(parseRef("github/codeql-action/init@v4"), { repo: "github/codeql-action", path: "init", ref: "v4" });
  assert.deepEqual(parseRef(`actions/checkout@${SHA_A}`), { repo: "actions/checkout", path: "", ref: SHA_A });
  assert.throws(() => parseRef("checkout@v7"));
});

test("the archive path is the runner's <owner>_<repo>/<sha>.tar.gz", () => {
  assert.equal(archivePath("actions/checkout", SHA_A), path.join("actions_checkout", `${SHA_A}.tar.gz`));
  assert.equal(archivePath("Swatinem/rust-cache", SHA_B), path.join("Swatinem_rust-cache", `${SHA_B}.tar.gz`));
  assert.throws(() => archivePath("actions/checkout", "v7"));
});

function fakeApi({ tags = {}, ymls = {} } = {}) {
  const calls = [];
  return {
    calls,
    async commitSha(repo, ref) {
      calls.push(`sha ${repo}@${ref}`);
      return tags[`${repo}@${ref}`];
    },
    async actionYml(repo, sha, subpath) {
      return ymls[`${repo}@${sha}${subpath ? "/" + subpath : ""}`] ?? null;
    },
    async tarball() {
      return Buffer.from([0x1f, 0x8b, 0, 0]);
    },
  };
}

test("tags resolve to commits, and one commit is one archive", async () => {
  const api = fakeApi({ tags: { "actions/checkout@v7": SHA_A, "actions/cache@v6": SHA_B } });
  const got = await resolveArchives(
    ["actions/checkout@v7", `actions/checkout@${SHA_A}`, "actions/cache/restore@v6", "actions/cache/save@v6"],
    api,
  );
  assert.deepEqual(got, [
    { repo: "actions/cache", sha: SHA_B, refs: ["actions/cache/restore@v6", "actions/cache/save@v6"] },
    { repo: "actions/checkout", sha: SHA_A, refs: [`actions/checkout@${SHA_A}`, "actions/checkout@v7"] },
  ]);
  // A sha pin is not looked up, and a tag is looked up once per spelling.
  assert.deepEqual(api.calls.sort(), ["sha actions/cache@v6", "sha actions/cache@v6", "sha actions/checkout@v7"]);
});

test("a composite action's own uses are followed, recursively", async () => {
  const api = fakeApi({
    tags: { "acme/scan@v1": SHA_A, "acme/setup@v2": SHA_B },
    ymls: {
      [`acme/scan@${SHA_A}`]: "runs:\n  using: composite\n  steps:\n    - uses: acme/setup@v2\n    - uses: ./local\n",
      [`acme/setup@${SHA_B}`]: `runs:\n  using: 'composite'\n  steps:\n    - uses: actions/cache@${SHA_C}\n`,
      [`actions/cache@${SHA_C}`]: "runs:\n  using: node24\n  main: dist/index.js\n",
    },
  });
  const got = await resolveArchives(["acme/scan@v1"], api);
  assert.deepEqual(
    got.map((a) => `${a.repo}@${a.sha}`),
    [`acme/scan@${SHA_A}`, `acme/setup@${SHA_B}`, `actions/cache@${SHA_C}`],
  );
});

test("a ref that resolves to no commit fails loudly", async () => {
  await assert.rejects(resolveArchives(["acme/gone@v9"], fakeApi()), /did not resolve to a commit/);
});

test("downloads land in the runner layout, and a non-gzip body is refused", async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "action-archive-"));
  try {
    const bytes = await downloadArchives([{ repo: "actions/checkout", sha: SHA_A }], dir, fakeApi());
    assert.equal(bytes, 4);
    assert.ok(fs.existsSync(path.join(dir, "actions_checkout", `${SHA_A}.tar.gz`)));
    const html = { ...fakeApi(), tarball: async () => Buffer.from("<html>") };
    await assert.rejects(downloadArchives([{ repo: "actions/cache", sha: SHA_B }], dir, html), /not a gzip archive/);
    assert.ok(!fs.existsSync(path.join(dir, "actions_cache")));
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("a scan below the floor fails instead of baking an empty cache", async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "action-archive-root-"));
  try {
    fs.mkdirSync(path.join(dir, ".github", "workflows"), { recursive: true });
    fs.writeFileSync(path.join(dir, ".github", "workflows", "a.yml"), "jobs:\n  a:\n    steps:\n      - uses: actions/checkout@v7\n");
    await assert.rejects(main([], { root: dir, log: () => {} }), /below the floor/);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("resolving without a token is refused", async () => {
  await assert.rejects(main(["--resolve"], { root: ROOT, env: {}, log: () => {} }), /GITHUB_TOKEN or GH_TOKEN/);
});

test("the repository's workflows and actions yield the ref set", async () => {
  const { files, refs } = scanRefs(ROOT);
  assert.ok(files > 50, `only ${files} workflow and action files`);
  assert.ok(refs.length >= MINIMUM_REF_COUNT, `only ${refs.length} refs`);
  assert.ok(refs.includes("actions/checkout@v7"));
  for (const r of refs) assert.doesNotThrow(() => parseRef(r), r);
  const lines = [];
  await main([], { root: ROOT, log: (l) => lines.push(l) });
  assert.deepEqual(lines, refs);
});
