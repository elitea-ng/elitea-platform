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
  CACHE_DIR,
  downloadArchives,
  isRetryable,
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

test("uses lines inside a block scalar are text, not steps", () => {
  const text = [
    "    steps:",
    "      - run: |",
    "          cat > wf.yml <<EOF",
    "            - uses: example/fixture@v1",
    "",
    "          EOF",
    "      - uses: actions/checkout@v7",
    "      - name: script",
    "        with:",
    "          script: >-",
    "            uses: example/other@v2",
    "        uses: actions/github-script@v9",
    "",
  ].join("\n");
  assert.deepEqual(usesRefs(text), ["actions/checkout@v7", "actions/github-script@v9"]);
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
      calls.push(`yml ${repo}@${sha}/${subpath}`);
      return ymls[`${repo}@${sha}${subpath ? "/" + subpath : ""}`] ?? null;
    },
    async tarball() {
      return Buffer.from([0x1f, 0x8b, 0, 0]);
    },
  };
}

test("tags resolve to commits once per repo, and one commit is one archive", async () => {
  const api = fakeApi({ tags: { "actions/checkout@v7": SHA_A, "actions/cache@v6": SHA_B } });
  const got = await resolveArchives(
    ["actions/checkout@v7", `actions/checkout@${SHA_A}`, "actions/cache/restore@v6", "actions/cache/save@v6"],
    api,
  );
  assert.deepEqual(got, [
    { repo: "actions/cache", sha: SHA_B, refs: ["actions/cache/restore@v6", "actions/cache/save@v6"] },
    { repo: "actions/checkout", sha: SHA_A, refs: [`actions/checkout@${SHA_A}`, "actions/checkout@v7"] },
  ]);
  // A sha pin is not looked up; restore@v6 and save@v6 share one lookup.
  assert.deepEqual(
    api.calls.filter((c) => c.startsWith("sha ")).sort(),
    ["sha actions/cache@v6", "sha actions/checkout@v7"],
  );
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

test("a ref that does not resolve is a warning and is left out", async () => {
  const api = fakeApi({ tags: { "actions/checkout@v7": SHA_A } });
  api.commitSha = async (repo, ref) => {
    if (repo === "acme/gone") throw new Error(`${repo}@${ref}: no such ref`);
    return SHA_A;
  };
  const warnings = [];
  const got = await resolveArchives(["acme/gone@v9", "acme/empty@v1", "actions/checkout@v7"], {
    ...api,
    commitSha: async (repo, ref) => (repo === "acme/empty" ? undefined : api.commitSha(repo, ref)),
  }, (w) => warnings.push(w));
  assert.deepEqual(got.map((a) => a.repo), ["actions/checkout"]);
  assert.equal(warnings.length, 2);
  assert.match(warnings[0], /acme\/gone@v9: .*no such ref; left out/);
  assert.match(warnings[1], /acme\/empty@v1: did not resolve to a commit/);
});

test("downloads land in the runner layout, and a non-gzip body is left out", async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "action-archive-"));
  try {
    const got = await downloadArchives([{ repo: "actions/checkout", sha: SHA_A }], dir, fakeApi());
    assert.deepEqual(got, { bytes: 4, count: 1 });
    assert.ok(fs.existsSync(path.join(dir, "actions_checkout", `${SHA_A}.tar.gz`)));
    const html = { ...fakeApi(), tarball: async () => Buffer.from("<html>") };
    const warnings = [];
    const bad = await downloadArchives([{ repo: "actions/cache", sha: SHA_B }], dir, html, (w) => warnings.push(w));
    assert.deepEqual(bad, { bytes: 0, count: 0 });
    assert.match(warnings[0], /not a gzip archive/);
    assert.ok(!fs.existsSync(path.join(dir, "actions_cache")));
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("only rate limits and server errors are retried", () => {
  const h = (o = {}) => new Headers(o);
  assert.ok(isRetryable(429, h()));
  assert.ok(isRetryable(502, h()));
  assert.ok(isRetryable(403, h({ "retry-after": "60" })));
  assert.ok(isRetryable(403, h({ "x-ratelimit-remaining": "0" })));
  assert.ok(!isRetryable(403, h({ "x-ratelimit-remaining": "12" })));
  assert.ok(!isRetryable(401, h()));
});

test("a scan or a resolution below its floor fails instead of baking an empty cache", async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "action-archive-root-"));
  try {
    fs.mkdirSync(path.join(dir, ".github", "workflows"), { recursive: true });
    fs.writeFileSync(path.join(dir, ".github", "workflows", "a.yml"), "jobs:\n  a:\n    steps:\n      - uses: actions/checkout@v7\n");
    await assert.rejects(main([], { root: dir, log: () => {} }), /below the floor/);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
  const nothing = { commitSha: async () => undefined, actionYml: async () => null };
  await assert.rejects(main(["--resolve"], { root: ROOT, api: nothing, log: () => {}, warn: () => {} }), /archives resolved, below the floor/);
});

test("arguments: a token is needed, --download needs a directory, unknown flags are refused", async () => {
  const quiet = { root: ROOT, env: {}, log: () => {} };
  await assert.rejects(main(["--resolve"], quiet), /GITHUB_TOKEN or GH_TOKEN/);
  await assert.rejects(main(["--download"], quiet), /needs a directory/);
  await assert.rejects(main(["--download", "--resolve"], quiet), /needs a directory/);
  await assert.rejects(main(["--resolv"], quiet), /unknown argument/);
});

test("the image and the reference values name the same cache directory", () => {
  const containerfile = fs.readFileSync(path.join(ROOT, "deploy/ci-runners/image/Containerfile"), "utf8");
  assert.match(containerfile, new RegExp(`^ENV ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE=${CACHE_DIR}$`, "m"));
  assert.match(containerfile, new RegExp(`^COPY action-archive/ ${CACHE_DIR}/$`, "m"));
  const values = fs.readFileSync(path.join(ROOT, "deploy/ci-runners/values-eng-runner.yaml"), "utf8");
  assert.match(values, new RegExp(`- name: ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE\\n\\s+value: ${CACHE_DIR}\\n`));
});

test("the repository's workflows and actions yield the ref set", async () => {
  const { files, refs } = scanRefs(ROOT);
  assert.ok(files > 0);
  assert.ok(refs.length >= MINIMUM_REF_COUNT, `only ${refs.length} refs`);
  assert.ok(refs.some((r) => r.startsWith("actions/checkout@")));
  for (const r of refs) assert.doesNotThrow(() => parseRef(r), r);
  const lines = [];
  await main([], { root: ROOT, log: (l) => lines.push(l) });
  assert.deepEqual(lines, refs);
});
