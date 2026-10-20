// Tests for scripts/ci/prune-ci-images.mjs: what the CI-image cleanup may delete.

import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import * as p from "./prune-ci-images.mjs";

const NOW = new Date(Date.UTC(2026, 9, 20));
const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

function v(id, daysOld, tags = []) {
  const updated = new Date(NOW.getTime() - daysOld * 86400000).toISOString().replace(".000Z", "Z");
  return { id, name: `sha256:${String(id).padStart(64, "0")}`, updated_at: updated, metadata: { container: { tags: [...tags] } } };
}

const ids = (versions) => versions.map((x) => x.id).sort((a, b) => a - b);

test("non ci packages are never selected", () => {
  for (const name of ["elitea-main", "elitea-worker-rust", "buildkit", "charts/elitea", "eci-x", "Ci-x"]) {
    assert.deepEqual(p.select(name, [v(1, 90), v(2, 80)], NOW, 7), [], name);
  }
});

test("image versions older than the window go tagged or not", () => {
  const versions = [v(1, 30, ["abc"]), v(2, 8), v(3, 6, ["def"]), v(4, 0, ["ghi"])];
  assert.deepEqual(ids(p.select("ci-elitea-main", versions, NOW, 7)), [1, 2]);
});

test("the live cache tag is kept however old", () => {
  const versions = [v(1, 40, ["linux-amd64"]), v(2, 30), v(3, 3)];
  assert.deepEqual(ids(p.select("ci-cache-elitea-main", versions, NOW, 7)), [2]);
});

test("a package is never emptied", () => {
  const versions = [v(1, 30, ["a"]), v(2, 20, ["b"])];
  assert.deepEqual(ids(p.select("ci-elitea-web", versions, NOW, 7)), [1]);
});

test("nothing inside the window goes", () => {
  assert.deepEqual(p.select("ci-elitea-web", [v(1, 6), v(2, 1)], NOW, 7), []);
});

test("package names come from the cache refs", () => {
  let hcl = 'target "a" {\n  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-main:linux-amd64"]\n}\n';
  hcl += 'target "b" {\n  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-main:linux-amd64"]\n}\n';
  assert.deepEqual(p.ciPackages([hcl]), ["ci-cache-elitea-main", "ci-elitea-main"]);
  const dir = path.join(ROOT, "deploy", "ci");
  const names = p.ciPackages(fs.readdirSync(dir).filter((n) => n.endsWith(".hcl")).map((n) => fs.readFileSync(path.join(dir, n), "utf8")));
  assert.ok(names.includes("ci-elitea-worker-rust"));
  assert.ok(names.every((n) => n.startsWith("ci-")));
});

// ── The run, through a fake github-script client (new with the port: the
// Python suite could not drive `gh`, so these arms had no test) ────────────

class FakeGithub {
  constructor(packages) {
    this.packages = packages; // name -> versions; absent -> 404
    this.deleted = [];
    this.listed = [];
    const self = this;
    this.rest = {
      packages: {
        getAllPackageVersionsForPackageOwnedByOrg: { endpoint: "list" },
        async deletePackageVersionForOrg(params) {
          if (params.package_type !== "container") throw new Error("not a container");
          self.deleted.push(`${params.package_name}/${params.package_version_id}`);
          return { status: 204 };
        },
      },
    };
  }

  async paginate(method, params) {
    assert.equal(method, this.rest.packages.getAllPackageVersionsForPackageOwnedByOrg);
    assert.equal(params.package_type, "container");
    assert.equal(params.org, "elitea-ng");
    this.listed.push(params.package_name);
    if (!Object.hasOwn(this.packages, params.package_name)) {
      const error = new Error("Not Found");
      error.status = 404;
      throw error;
    }
    return this.packages[params.package_name];
  }
}

async function run(github, options = {}) {
  const lines = [];
  const code = await p.main({ github, org: "elitea-ng", days: 7, now: NOW, log: (line) => lines.push(line), ...options });
  return [code, lines];
}

test("a run deletes only what select chose and skips a missing package", async () => {
  const github = new FakeGithub({
    "ci-elitea-main": [v(1, 30, ["abc"]), v(2, 1)],
    "ci-cache-elitea-main": [v(3, 40, ["linux-amd64"]), v(4, 30), v(5, 3)],
  });
  const [code, lines] = await run(github);
  assert.equal(code, 0);
  assert.deepEqual(github.deleted.sort(), ["ci-cache-elitea-main/4", "ci-elitea-main/1"]);
  assert.ok(github.listed.length >= 20);
  assert.ok(github.listed.every((n) => n.startsWith("ci-")));
  assert.ok(lines.includes("ci-elitea-web: no such package yet; skipped"));
  assert.ok(lines.includes("ci-elitea-main: 2 version(s), 1 older than 7 day(s) to delete"));
  assert.ok(lines.includes(`  deleted ci-elitea-main@sha256:000000000000 tags=['abc'] updated=${v(1, 30).updated_at}`));
  assert.equal(lines.at(-1), "2 version(s) deleted");
});

test("a dry run deletes nothing", async () => {
  const github = new FakeGithub({ "ci-elitea-main": [v(1, 30), v(2, 1)] });
  const [code, lines] = await run(github, { dryRun: true });
  assert.equal(code, 0);
  assert.deepEqual(github.deleted, []);
  assert.ok(lines.some((l) => l.startsWith("  would delete ci-elitea-main@")));
  assert.equal(lines.at(-1), "dry run: nothing deleted");
});

test("a window under one day is refused before any call", async () => {
  const github = new FakeGithub({});
  for (const days of [0, "-3", "abc"]) {
    const [code, lines] = await run(github, { days });
    assert.equal(code, 2, String(days));
    assert.match(lines[0], /^::error::--days/);
  }
  assert.deepEqual(github.listed, []);
});

test("an API failure other than 404 fails the run", async () => {
  const github = new FakeGithub({});
  github.paginate = async () => {
    const error = new Error("Server Error");
    error.status = 500;
    throw error;
  };
  await assert.rejects(run(github), /versions failed: Server Error/);
});
