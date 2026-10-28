// Tests for scripts/ci/prune-releases.mjs: what the manual release retention may delete.

import assert from "node:assert/strict";
import { test } from "node:test";

import * as p from "./prune-releases.mjs";

const rel = (id, tag, day, prerelease = false) => ({ id, tag_name: tag, prerelease, created_at: `2026-10-${String(day).padStart(2, "0")}T02:07:00Z` });
const ver = (id, tags, day = 1, name = "") => ({ id, name, created_at: `2026-10-${String(day).padStart(2, "0")}T03:00:00Z`, metadata: { container: { tags } } });

const RELEASES = [
  rel(1, "v5.0.0", 1),
  rel(2, "v5.1.0", 2),
  rel(3, "v5.1.1-nightly.20261003.7", 3, true),
  rel(4, "v6.0.0", 4),
  rel(5, "v6.0.1-nightly.20261005.8", 5, true),
  rel(6, "v6.0.1-nightly.20261006.9", 6, true),
  rel(7, "v6.0.1-dev.10", 7, true),
  rel(8, "v7.0.0-rc.1", 8, true),
];

test("the newest stable releases and the newest pre-releases after them are kept", () => {
  const { keptStable, doomedStable, doomedPre, kept, cutoff } = p.plan(RELEASES, 2, 1);
  assert.deepEqual(keptStable.map((r) => r.tag_name), ["v6.0.0", "v5.1.0"]);
  assert.deepEqual(doomedStable.map((r) => r.tag_name), ["v5.0.0"]);
  // nightly.8 is beyond the one kept nightly; nightly.7 was cut before v6.0.0.
  assert.deepEqual(doomedPre.map((r) => r.tag_name), ["v6.0.1-nightly.20261005.8", "v5.1.1-nightly.20261003.7"]);
  assert.deepEqual(kept.map((r) => r.tag_name), ["v5.1.0", "v6.0.0", "v6.0.1-nightly.20261006.9", "v6.0.1-dev.10"]);
  assert.equal(cutoff, "2026-10-02T02:07:00Z");
  // A tag of no shape publish.yml mints is neither kept nor deleted.
  assert.ok(![...doomedStable, ...doomedPre, ...kept].some((r) => r.tag_name === "v7.0.0-rc.1"));
});

test("a deleted stable release takes its <major>.<minor> only when no kept release shares it", () => {
  const tags = p.doomedTags([rel(1, "v5.0.0", 1), rel(2, "v6.0.0", 2), rel(3, "v6.1.0-dev.4", 3, true)], [rel(9, "v6.0.1", 9)]);
  assert.ok(tags.images.has("5.0"));
  assert.ok(!tags.images.has("6.0"));
  assert.ok(tags.images.has("6.0.0-linux-arm64"));
  assert.ok(tags.images.has("6.1.0-dev.4"));
  assert.deepEqual([...tags.charts].sort(), ["5.0.0", "6.0.0", "6.1.0-dev.4"]);
});

test("a package loses doomed versions, their signatures and old untagged versions only", () => {
  const versions = [
    ver(1, ["5.0.0"], 1, `sha256:${"a".repeat(64)}`),
    ver(2, [`sha256-${"a".repeat(64)}`], 1),
    ver(3, [], 1),
    ver(4, [], 3),
    ver(5, ["6.0.0", "latest"], 4),
    ver(6, ["5.0.0-linux-amd64", "keep-me"], 1),
  ];
  const { remove, kept } = p.planPackage(versions, new Set(["5.0.0", "5.0.0-linux-amd64"]), "2026-10-02T02:07:00Z");
  assert.deepEqual(remove.map((v) => v.id), [1, 2, 3]);
  assert.deepEqual(kept.map((v) => v.id), [6]);
  assert.deepEqual(p.planPackage(versions, new Set(), null).remove, []);
});

test("orphaned signatures go; a signature of a live manifest stays", () => {
  const A = "a".repeat(64);
  const B = "b".repeat(64);
  const versions = [ver(1, ["6.0.0"], 4, `sha256:${A}`), ver(2, [`sha256-${A}`], 4), ver(3, [`sha256-${B}`], 4)];
  assert.deepEqual(p.planPackage(versions, new Set(), null).remove.map((v) => v.id), [3]);
});

test("retired packages are this repository's, and never a live, CI or kept one", () => {
  const pkg = (name, repo = "elitea-platform") => ({ name, repository: { name: repo } });
  const packages = [pkg("elitea-main"), pkg("charts/elitea"), pkg("ci-elitea-main"), pkg("ci-cache-elitea-web"), pkg("buildkit"), pkg("old-image"), pkg("charts/old"), pkg("other", "elitea-e2e-llm"), { name: "orphan" }];
  assert.deepEqual(p.retiredPackages(packages, "elitea-platform", ["elitea-main"], ["elitea"]), ["charts/old", "old-image"]);
});

class FakeGithub {
  constructor(releases, packages, orgPackages = []) {
    this.releases = releases;
    this.packages = packages;
    this.orgPackages = orgPackages;
    this.calls = [];
    this.headers = {};
    const self = this;
    const record = (call) => {
      self.calls.push(call);
      return { headers: self.headers };
    };
    this.rest = {
      repos: { listReleases: { endpoint: "releases" }, deleteRelease: async ({ release_id }) => record(`release/${release_id}`) },
      git: { deleteRef: async ({ ref }) => record(`ref/${ref}`) },
      rateLimit: { get: async () => ({ data: { resources: { core: { remaining: 5000, reset: 0 } } } }) },
      packages: {
        getAllPackageVersionsForPackageOwnedByOrg: { endpoint: "versions" },
        listPackagesForOrganization: { endpoint: "packages" },
        deletePackageVersionForOrg: async ({ package_name, package_version_id }) => record(`pkg/${package_name}/${package_version_id}`),
        deletePackageForOrg: async ({ package_name }) => record(`package/${package_name}`),
      },
    };
  }

  async paginate(method, params) {
    if (method === this.rest.repos.listReleases) return this.releases;
    if (method === this.rest.packages.listPackagesForOrganization) return this.orgPackages;
    if (!Object.hasOwn(this.packages, params.package_name)) throw Object.assign(new Error("Not Found"), { status: 404 });
    return this.packages[params.package_name];
  }
}

async function run(github, options = {}) {
  const lines = [];
  const code = await p.main({
    github,
    owner: "elitea-ng",
    repo: "elitea-platform",
    org: "elitea-ng",
    keepReleases: 1,
    keepPrereleases: 1,
    dryRun: false,
    log: (l) => lines.push(l),
    sleep: async () => {},
    ...options,
  });
  return [code, lines];
}

const world = () =>
  new FakeGithub([rel(1, "v5.0.0", 1), rel(2, "v6.0.0", 2)], { "elitea-main": [ver(10, ["5.0.0", "5.0"], 1), ver(11, [], 1), ver(12, ["6.0.0", "6.0", "latest"], 2)] }, [
    { name: "old-image", repository: { name: "elitea-platform" } },
  ]);

test("a run deletes packages, retired packages when asked, then releases, then tags", async () => {
  const github = world();
  const [code, lines] = await run(github, { deleteRetired: true });
  assert.equal(code, 0);
  assert.deepEqual(github.calls, ["pkg/elitea-main/10", "pkg/elitea-main/11", "package/old-image", "ref/tags/v5.0.0", "release/1"]);
  assert.equal(lines.at(-1), "1 release(s) and 2 package version(s) deleted");
});

test("a dry run and a run without deleteRetired delete nothing they were not asked to", async () => {
  const dry = world();
  const [code, lines] = await run(dry, { dryRun: true, deleteRetired: true });
  assert.equal(code, 0);
  assert.deepEqual(dry.calls, []);
  assert.match(lines[0], /^DRY RUN/);
  const keep = world();
  await run(keep);
  assert.ok(!keep.calls.includes("package/old-image"));
});

test("a run waits out the rate limit, and stops cleanly when the deadline comes first", async () => {
  const github = world();
  github.headers = { "x-ratelimit-remaining": "3", "x-ratelimit-reset": "100" };
  const waits = [];
  const [code] = await run(github, { now: () => 0, sleep: async (ms) => waits.push(ms) });
  assert.equal(code, 0);
  assert.equal(waits[0], 105000);

  const late = world();
  late.headers = github.headers;
  const [stopped, lines] = await run(late, { now: () => 0, deadline: 50000 });
  assert.equal(stopped, 0);
  assert.deepEqual(late.calls, ["pkg/elitea-main/10"]);
  assert.match(lines.at(-1), /stopped at the deadline after 1 package version/);
});

test("bad arguments are refused before any call", async () => {
  const github = world();
  github.paginate = async () => assert.fail("called the API");
  for (const options of [{ keepReleases: 0 }, { keepPrereleases: 1.5 }, { org: "" }]) {
    const [code, lines] = await run(github, options);
    assert.equal(code, 2, JSON.stringify(options));
    assert.match(lines[0], /^::error::/);
  }
});
