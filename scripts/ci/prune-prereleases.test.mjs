// Tests for scripts/ci/prune-prereleases.mjs: what the pre-release prune may delete.

import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import * as p from "./prune-prereleases.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

const rel = (id, tag, day, prerelease = true) => ({ id, tag_name: tag, prerelease, created_at: `2026-10-${String(day).padStart(2, "0")}T02:07:00Z` });
const ver = (id, tags) => ({ id, metadata: { container: { tags } } });

test("only pre-releases of the channel, in the minted shape, are selected", () => {
  const releases = [
    rel(1, "v6.1.0-nightly.20261001.10", 1),
    rel(2, "v6.1.0-nightly.20261002.11", 2),
    rel(3, "v6.1.0-dev.12", 3),
    rel(4, "v6.0.0", 4, false),
    rel(5, "v6.1.0-nightly.20261005.13", 5, false),
    rel(6, "v6.1.0-nightly-x", 6),
    rel(7, "v6.1.0-nightly.20261007.14", 7),
  ];
  assert.deepEqual(p.select(releases, "nightly", 1).map((r) => r.id), [2, 1]);
  assert.deepEqual(p.select(releases, "dev", 1), []);
  assert.deepEqual(p.select(releases, "nightly", 5), []);
});

test("the newest are the kept ones, whatever order the API lists them", () => {
  const releases = [rel(1, "v1.0.1-dev.3", 3), rel(2, "v1.0.1-dev.1", 1), rel(3, "v1.0.1-dev.2", 2)];
  assert.deepEqual(p.select(releases, "dev", 2).map((r) => r.id), [2]);
});

test("a version carrying a foreign tag is kept", () => {
  const doomed = new Set(["6.1.0-dev.1", "6.1.0-dev.1-linux-amd64"]);
  const [remove, kept] = p.doomedVersions(
    [ver(1, ["6.1.0-dev.1"]), ver(2, ["6.1.0-dev.1-linux-amd64", "latest"]), ver(3, ["6.0.0"]), ver(4, [])],
    doomed,
  );
  assert.deepEqual(remove.map((v) => v.id), [1]);
  assert.deepEqual(kept.map((v) => v.id), [2]);
});

test("the package lists come from publish.yml's rollback arrays", () => {
  const text = fs.readFileSync(path.join(ROOT, ".github", "workflows", "publish.yml"), "utf8");
  const images = p.bashArray(text, "IMAGES");
  assert.ok(images.includes("elitea-main"));
  assert.ok(images.length >= 10);
  assert.deepEqual(p.bashArray(text, "CHARTS"), ["elitea", "elitea-sandbox-runtime"]);
  assert.deepEqual(p.bashArray("nothing here", "IMAGES"), []);
});

class FakeGithub {
  constructor(releases, packages) {
    this.releases = releases;
    this.packages = packages; // name -> versions; absent -> 404
    this.calls = [];
    const self = this;
    this.rest = {
      repos: {
        listReleases: { endpoint: "releases" },
        async deleteRelease({ release_id }) {
          self.calls.push(`release/${release_id}`);
        },
      },
      git: {
        async deleteRef({ ref }) {
          self.calls.push(`ref/${ref}`);
        },
      },
      packages: {
        getAllPackageVersionsForPackageOwnedByOrg: { endpoint: "versions" },
        async deletePackageVersionForOrg({ package_name, package_version_id }) {
          self.calls.push(`pkg/${package_name}/${package_version_id}`);
        },
      },
    };
  }

  async paginate(method, params) {
    if (method === this.rest.repos.listReleases) return this.releases;
    assert.equal(params.org, "elitea-ng");
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
  const code = await p.main({ github, owner: "elitea-ng", repo: "elitea-platform", org: "elitea-ng", channel: "dev", keep: 1, log: (l) => lines.push(l), ...options });
  return [code, lines];
}

test("a run deletes packages first, then the release, then the tag", async () => {
  const github = new FakeGithub([rel(1, "v6.1.0-dev.1", 1), rel(2, "v6.1.0-dev.2", 2), rel(3, "v6.0.0", 3, false)], {
    "elitea-main": [ver(10, ["6.1.0-dev.1"]), ver(11, ["6.1.0-dev.1-linux-amd64"]), ver(12, ["6.1.0-dev.2"]), ver(13, ["6.0.0", "latest"])],
    "charts/elitea": [ver(20, ["6.1.0-dev.1"]), ver(21, ["6.0.0"])],
  });
  const [code, lines] = await run(github);
  assert.equal(code, 0);
  assert.deepEqual(github.calls, ["pkg/elitea-main/10", "pkg/elitea-main/11", "pkg/charts/elitea/20", "release/1", "ref/tags/v6.1.0-dev.1"]);
  assert.ok(lines.includes("elitea-scheduler: no such package; skipped"));
  assert.equal(lines.at(-1), "1 pre-release(s) and 3 package version(s) deleted");
});

test("nothing beyond the kept ones means no package is even listed", async () => {
  const github = new FakeGithub([rel(1, "v6.1.0-dev.1", 1)], {});
  github.paginate = async (method) => {
    assert.equal(method, github.rest.repos.listReleases);
    return github.releases;
  };
  const [code] = await run(github);
  assert.equal(code, 0);
  assert.deepEqual(github.calls, []);
});

test("a bad channel or keep is refused before any call", async () => {
  const github = new FakeGithub([], {});
  github.paginate = async () => assert.fail("called the API");
  for (const options of [{ channel: "release" }, { channel: "rc" }, { keep: 0 }, { keep: 1.5 }, { org: "" }]) {
    const [code, lines] = await run(github, options);
    assert.equal(code, 2, JSON.stringify(options));
    assert.match(lines[0], /^::error::/);
  }
});

test("an API failure other than 404 fails the run", async () => {
  const github = new FakeGithub([rel(1, "v6.1.0-dev.1", 1), rel(2, "v6.1.0-dev.2", 2)], {});
  github.rest.packages.deletePackageVersionForOrg = async () => {
    throw Object.assign(new Error("Server Error"), { status: 500 });
  };
  github.packages["elitea-main"] = [ver(10, ["6.1.0-dev.1"])];
  await assert.rejects(run(github), /DELETE elitea-main version 10 failed: Server Error/);
});
