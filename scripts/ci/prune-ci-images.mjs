// Prune the CI-only `ci-*` container packages of the organisation.
//
//     ci-image-cleanup.yml, through actions/github-script:
//       const { main } = await import(`${process.env.GITHUB_WORKSPACE}/scripts/ci/prune-ci-images.mjs`);
//       const code = await main({ github, org, days, dryRun });
//       if (code !== 0) core.setFailed(`the prune exited ${code}`);
//
// WHICH PACKAGES. Not an org listing: `GET /orgs/{org}/packages` needs an
// org-level read:packages token, and the job's GITHUB_TOKEN cannot enumerate
// the organisation's packages (measured: the first run of this workflow
// failed there). The names come from the image list itself instead — every
// `ci-cache-<image>` ref in deploy/ci/*.hcl (the one-ref-per-image list the
// stack images are built from) and in docker-bake.hcl (the release images,
// whose builds share the same cache packages under `release-<platform>`) — giving `ci-<image>` and `ci-cache-<image>`
// for each. Those packages are linked to this repository (each push labels
// `org.opencontainers.image.source`), so the token may read and delete their
// versions. A package that does not exist yet (a cache main has not written)
// is skipped.
//
// ci-web-e2e.yml pushes every stack image it builds to `ci-<image>:<sha>` (one
// version per commit) and, on a push to main, the layer cache to
// `ci-cache-<image>:linux-amd64` (the tag moves; every move strands the previous
// manifest as an untagged version); publish.yml does the same with
// `ci-cache-<image>:release-<platform>`. Nothing that ships references them
// (scripts/ci/check-ci-image-references.mjs), so they are disposable:
//
//   ci-cache-*   delete UNTAGGED versions older than --days. The tagged version
//                is the live cache every build reads; it is kept however old.
//   ci-*         delete every version older than --days, tagged or not. A lane
//                re-run after that fails its pull, loudly, and a fresh push of
//                the commit (or a re-run of the whole workflow) rebuilds.
//
// Never touched: any package whose name does not start with `ci-` — the
// release images (elitea-main, elitea-worker-rust, ...), the buildkit mirror,
// the charts. The rule is checked twice, at selection and again immediately
// before each DELETE. A package's last remaining version is never deleted (the
// registry refuses it, and an empty package would lose its visibility setting).
//
// Age is `updated_at`: a version re-pushed under the same digest is fresh.
//
// The API calls go through the `github` client actions/github-script hands
// in, so scripts/ci/prune-ci-images.test.mjs drives the whole run with a fake.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { pyRepr } from "./py-compat.mjs";

const PREFIX = "ci-";
const CACHE_PREFIX = "ci-cache-";
const DAY_MS = 24 * 60 * 60 * 1000;
const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

export function isCiPackage(name) {
  return name.startsWith(PREFIX);
}

function tagsOf(version) {
  return ((version.metadata || {}).container || {}).tags || [];
}

/** The versions of package `name` to delete under the policy above. */
export function select(name, versions, now, days) {
  if (!isCiPackage(name)) return [];
  const cutoff = now.getTime() - days * DAY_MS;
  let doomed = [];
  for (const version of versions) {
    const updated = Date.parse(version.updated_at);
    if (updated >= cutoff) continue;
    if (name.startsWith(CACHE_PREFIX) && tagsOf(version).length) continue;
    doomed.push(version);
  }
  if (versions.length && doomed.length >= versions.length) {
    // Keep the newest: never empty a package.
    const newest = versions.reduce((best, v) => (v.updated_at > best.updated_at ? v : best));
    doomed = doomed.filter((v) => v.id !== newest.id);
  }
  return doomed;
}

const CACHE_REF = new RegExp("ghcr\\.io/[A-Za-z0-9_.-]+/" + CACHE_PREFIX + "([A-Za-z0-9_.-]+):", "g");

/** `ci-<image>` and `ci-cache-<image>` for every cache ref in the HCL. */
export function ciPackages(hclTexts) {
  const images = new Set();
  for (const text of hclTexts) {
    for (const match of text.matchAll(CACHE_REF)) images.add(match[1]);
  }
  const sorted = [...images].sort();
  return [...sorted.map((i) => PREFIX + i), ...sorted.map((i) => CACHE_PREFIX + i)].sort();
}

export function hclSources(root = ROOT) {
  const dir = path.join(root, "deploy", "ci");
  const hcl = fs.readdirSync(dir).filter((n) => n.endsWith(".hcl")).sort().map((n) => path.join(dir, n));
  return [...hcl, path.join(root, "docker-bake.hcl")];
}

async function listVersions(github, org, name) {
  try {
    return await github.paginate(github.rest.packages.getAllPackageVersionsForPackageOwnedByOrg, {
      package_type: "container",
      package_name: name,
      org,
      per_page: 100,
    });
  } catch (error) {
    if (error && error.status === 404) return null;
    throw new Error(`GET /orgs/${org}/packages/container/${encodeURIComponent(name)}/versions failed: ${error && error.message}`);
  }
}

async function deleteVersion(github, org, name, id) {
  try {
    await github.rest.packages.deletePackageVersionForOrg({
      package_type: "container",
      package_name: name,
      org,
      package_version_id: id,
    });
  } catch (error) {
    if (error && error.status === 404) return;
    throw new Error(`DELETE /orgs/${org}/packages/container/${encodeURIComponent(name)}/versions/${id} failed: ${error && error.message}`);
  }
}

/**
 * The prune. `days` is the workflow input as given (a string or a number);
 * returns the exit code the Python script had: 0, 1 (too few package names),
 * 2 (a bad --days). A failed API call throws.
 */
export async function main({ github, org, days = 7, dryRun = false, now = new Date(), root = ROOT, log = console.log }) {
  const text = String(days).trim();
  if (!/^[-+]?\d+$/.test(text)) {
    log(`::error::--days: invalid int value: ${pyRepr(String(days))}`);
    return 2;
  }
  const window = Number.parseInt(text, 10);
  if (window < 1) {
    log("::error::--days must be at least 1");
    return 2;
  }
  if (!org) {
    log("::error::--org is required");
    return 2;
  }

  const ci = ciPackages(hclSources(root).map((p) => fs.readFileSync(p, "utf8")));
  if (ci.length < 20) {
    log(`::error::derived ${ci.length} CI package name(s) from deploy/ci/*.hcl and docker-bake.hcl, expected at least 20 — the ref pattern stopped matching`);
    return 1;
  }
  log(`${ci.length} CI-only package name(s) from deploy/ci/*.hcl and docker-bake.hcl: ${ci.join(", ")}`);
  let deleted = 0;
  for (const name of ci) {
    const versions = await listVersions(github, org, name);
    if (versions === null) {
      log(`${name}: no such package yet; skipped`);
      continue;
    }
    const doomed = select(name, versions, now, window);
    log(`${name}: ${versions.length} version(s), ${doomed.length} older than ${window} day(s) to delete`);
    for (const version of doomed) {
      if (!isCiPackage(name)) {
        // The rule, again, at the point of no return.
        throw new Error(`refusing to delete from non-CI package ${name}`);
      }
      const label = `${name}@${String(version.name).slice(0, 19)} tags=${pyRepr(tagsOf(version))} updated=${version.updated_at}`;
      if (dryRun) {
        log(`  would delete ${label}`);
        continue;
      }
      await deleteVersion(github, org, name, version.id);
      log(`  deleted ${label}`);
      deleted += 1;
    }
  }
  log(dryRun ? "dry run: nothing deleted" : `${deleted} version(s) deleted`);
  return 0;
}
