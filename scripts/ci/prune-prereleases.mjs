// Prune the old pre-releases publish.yml cuts (the nightly and dev channels).
//
//     publish.yml `prune`, through actions/github-script:
//       const { main } = await import(`${process.env.GITHUB_WORKSPACE}/scripts/ci/prune-prereleases.mjs`);
//       const code = await main({ github, owner, repo, org, channel, keep });
//       if (code !== 0) core.setFailed(`the prune exited ${code}`);
//
// WHAT. A nightly a day, and a dev pre-release per dispatch, each publish a
// GitHub pre-release, its tag, every release image at three tags (the
// multi-arch `<version>` and `<version>-linux-{amd64,arm64}`) and every chart.
// Kept forever, they would outnumber the releases a hundred to one. So after a
// pre-release of ONE channel, the newest `keep` of that channel stay, and every
// older one goes, with its images, charts, GitHub release and tag. After a
// stable release (channel `release`, with its `tag`), every pre-release of
// either channel cut before it goes: each previewed a release that now exists.
// Stable releases are never deleted here; release-retention.yml does that, by
// hand (scripts/ci/prune-releases.mjs).
//
// WHICH PRE-RELEASES. A GitHub release flagged `prerelease` whose tag is
// `v<x>.<y>.<z>-<channel>.<digits and dots>` — exactly the forms publish.yml
// mints. A stable release, or a tag of any other shape, is never selected, and
// the rule is checked again immediately before the deletes.
//
// WHICH PACKAGES. The IMAGES and CHARTS arrays of publish.yml's rollback job,
// read from the file: scripts/ci/image-matrix.sh already holds IMAGES to the
// build matrix, so this is not another copy to drift.
//
// WHICH VERSIONS. A package version is deleted only when EVERY tag it carries
// belongs to a doomed pre-release. One also carrying a tag nothing doomed
// minted (`latest`, a stable version) is kept and reported. The cosign
// signature of a deleted manifest (the `sha256-<digest>` tag) goes with it.
//
// ORDER. Packages first, then the tag, then the GitHub release: a run that
// stops half way leaves the release listed, so the next run selects it again
// (and a missing tag is a 404 it ignores). Each manifest's signatures go
// right after it, so a stop never strands them.
//
// BUDGET. A workflow token gets a few thousand calls an hour. The run waits out the
// limit while it has time before `deadline`, and stops cleanly when it has not;
// the next run carries on. A version GitHub refuses to delete (a public one
// pulled more than 5,000 times) is reported and kept, not fatal.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { pyRepr } from "./py-compat.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");
export const CHANNELS = new Set(["nightly", "dev"]);
export const SUFFIXES = ["", "-linux-amd64", "-linux-arm64"];
export const STABLE_TAG = /^v(\d+)\.(\d+)\.\d+$/;

export function isPrereleaseTag(tag, channel) {
  return CHANNELS.has(channel) && new RegExp(`^v\\d+\\.\\d+\\.\\d+-${channel}\\.\\d+(?:\\.\\d+)*$`).test(tag);
}

export const isAnyPrerelease = (r) => r.prerelease && [...CHANNELS].some((c) => isPrereleaseTag(r.tag_name, c));
export const isStable = (r) => !r.prerelease && STABLE_TAG.test(r.tag_name);
export const newestFirst = (a, b) => (a.created_at < b.created_at ? 1 : a.created_at > b.created_at ? -1 : 0);

/** The pre-releases of `channel` beyond the newest `keep`, newest first. */
export function select(releases, channel, keep) {
  const own = releases.filter((r) => r.prerelease && isPrereleaseTag(r.tag_name, channel));
  own.sort(newestFirst);
  return own.slice(keep);
}

/** Every pre-release, of either channel, created before the release `tag`; null when there is no such release. */
export function selectSuperseded(releases, tag) {
  const release = releases.find((r) => r.tag_name === tag && !r.prerelease);
  if (!release) return null;
  return releases.filter((r) => isAnyPrerelease(r) && r.created_at < release.created_at);
}

/** The `NAME=("a" "b" ...)` bash array of publish.yml. */
export function bashArray(text, name) {
  const match = new RegExp(`^ +${name}=\\((.*)\\)\\s*$`, "m").exec(text);
  if (!match) return [];
  return [...match[1].matchAll(/"([^"]+)"/g)].map((m) => m[1]);
}

/** publish.yml's rollback IMAGES and CHARTS, or null (logged) when the pattern stopped matching. */
export function readPackageLists(root, log) {
  const publish = fs.readFileSync(path.join(root, ".github", "workflows", "publish.yml"), "utf8");
  const images = bashArray(publish, "IMAGES");
  const charts = bashArray(publish, "CHARTS");
  if (images.length < 5 || charts.length < 1) {
    log(`::error::read ${images.length} image(s) and ${charts.length} chart(s) from publish.yml's rollback arrays — the pattern stopped matching`);
    return null;
  }
  return { images, charts };
}

export const tagsOf = (version) => ((version.metadata || {}).container || {}).tags || [];

/**
 * The versions of one package to delete: every tag they carry is one of
 * `doomedTags`. Returns [delete, keptWithForeignTags].
 */
export function doomedVersions(versions, doomedTags) {
  const remove = [];
  const kept = [];
  for (const version of versions) {
    const tags = tagsOf(version);
    if (!tags.some((t) => doomedTags.has(t))) continue;
    (tags.every((t) => doomedTags.has(t)) ? remove : kept).push(version);
  }
  return [remove, kept];
}

const SIGNATURE = /^sha256-([0-9a-f]{64})(?:\.sig|\.att)?$/;
const signedDigest = (version) => {
  const tags = tagsOf(version);
  const digests = new Set(tags.map((t) => (SIGNATURE.exec(t) || [])[1]));
  return tags.length && digests.size === 1 && !digests.has(undefined) ? [...digests][0] : null;
};

/** The cosign signatures (`sha256-<digest>` tags) of the `removed` manifests. */
export function signaturesOf(versions, removed) {
  const hexes = new Set(removed.map((v) => String(v.name || "").replace(/^sha256:/, "")));
  return versions.filter((v) => hexes.has(signedDigest(v)));
}

/** Signatures whose manifest is gone (a run stopped between the two, before this order). */
export function orphanSignatures(versions) {
  const present = new Set(versions.map((v) => String(v.name || "").replace(/^sha256:/, "")));
  return versions.filter((v) => {
    const hex = signedDigest(v);
    return hex !== null && !present.has(hex);
  });
}

/** Each doomed manifest followed by its own signatures, and the versions kept for a foreign tag. */
export function planManifests(versions, doomedTags) {
  const [manifests, kept] = doomedVersions(versions, doomedTags);
  const remove = manifests.flatMap((m) => [m, ...signaturesOf(versions, [m])]);
  return { remove, kept };
}

export async function listVersions(github, org, name) {
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

export class OutOfTime extends Error {}

/**
 * Deletes through `github` within the rate limit and the deadline.
 * `remove` returns "deleted", "absent" (a 404) or "refused" (a 400 or 422:
 * GitHub will not delete it, reported). `budget(n)` waits until n calls are
 * left, for the listing calls a delete's headers say nothing about. A low
 * limit a delete reports is waited out before the NEXT call, so a delete that
 * happened is always returned (and counted). Either throws OutOfTime when the
 * wait would pass the deadline.
 */
export function deleter({ github, dryRun = false, deadline = Infinity, now = Date.now, sleep = (ms) => new Promise((r) => setTimeout(r, ms)), log }) {
  const waitUntil = async (resetSeconds, remaining) => {
    const resetAt = Number(resetSeconds) * 1000 + 5000;
    if (!(resetAt < deadline)) throw new OutOfTime();
    log(`rate limit low (${remaining} left); waiting until ${new Date(resetAt).toISOString()}`);
    await sleep(Math.max(0, resetAt - now()));
  };
  let low = null;
  const ready = async () => {
    if (now() >= deadline) throw new OutOfTime();
    if (low) {
      const { reset, remaining } = low;
      low = null;
      await waitUntil(reset, remaining);
    }
  };
  return {
    async budget(calls) {
      if (dryRun) return;
      await ready();
      const { data } = await github.rest.rateLimit.get();
      const core = data.resources.core;
      if (core.remaining < calls + 25) await waitUntil(core.reset, core.remaining);
    },
    async remove(call, what) {
      if (dryRun) return "deleted";
      await ready();
      let response;
      let outcome = "deleted";
      try {
        response = await call();
      } catch (error) {
        if (error && error.status === 404) outcome = "absent";
        else if (error && (error.status === 400 || error.status === 422)) {
          log(`::warning::${what} refused: ${error.message}; kept`);
          outcome = "refused";
        } else throw new Error(`${what} failed: ${error && error.message}`);
      }
      const headers = (response && response.headers) || {};
      const remaining = Number(headers["x-ratelimit-remaining"]);
      if (remaining < 25) low = { reset: headers["x-ratelimit-reset"], remaining };
      return outcome;
    },
  };
}

/** Tag first, then the release: a stop between them leaves the release listed for the next run. */
export async function deleteRelease(del, github, owner, repo, release) {
  await del.remove(() => github.rest.git.deleteRef({ owner, repo, ref: `tags/${release.tag_name}` }), `DELETE tag ${release.tag_name}`);
  await del.remove(() => github.rest.repos.deleteRelease({ owner, repo, release_id: release.id }), `DELETE release ${release.tag_name}`);
}

/** Returns 0 (also when it stopped at the deadline; it says so), or 2 for a bad argument. A failed API call throws. */
export async function main({ github, owner, repo, org, channel, keep, tag, deadline = Infinity, root = ROOT, log = console.log, now, sleep }) {
  if (!CHANNELS.has(channel) && channel !== "release") {
    log(`::error::channel must be release or one of ${[...CHANNELS].join(", ")}; got ${pyRepr(String(channel))}`);
    return 2;
  }
  if (channel === "release" && !/^v\d+\.\d+\.\d+$/.test(String(tag))) {
    log(`::error::a release prune needs the stable tag just released; got ${pyRepr(String(tag))}`);
    return 2;
  }
  if (channel !== "release" && (!Number.isInteger(keep) || keep < 1)) {
    log(`::error::keep must be an integer of at least 1; got ${pyRepr(String(keep))}`);
    return 2;
  }
  if (!owner || !repo || !org) {
    log("::error::owner, repo and org are required");
    return 2;
  }
  const lists = readPackageLists(root, log);
  if (!lists) return 2;

  const releases = await github.paginate(github.rest.repos.listReleases, { owner, repo, per_page: 100 });
  let doomed;
  if (channel === "release") {
    doomed = selectSuperseded(releases, tag);
    if (doomed === null) {
      log(`::error::no stable release ${pyRepr(String(tag))} is listed`);
      return 2;
    }
    log(`${tag}: deleting the ${doomed.length} pre-release(s) cut before it`);
  } else {
    doomed = select(releases, channel, keep);
    log(`${channel}: ${releases.filter((r) => r.prerelease && isPrereleaseTag(r.tag_name, channel)).length} pre-release(s), keeping ${keep}, deleting ${doomed.length}`);
  }
  if (!doomed.length) return 0;

  const versions = doomed.map((r) => r.tag_name.slice(1));
  const imageTags = new Set(versions.flatMap((v) => SUFFIXES.map((s) => v + s)));
  const chartTags = new Set(versions);

  const del = deleter({ github, deadline, now, sleep, log });
  const packages = [...lists.images.map((n) => [n, imageTags]), ...lists.charts.map((n) => [`charts/${n}`, chartTags])];
  let deleted = 0;
  try {
    for (const [name, tags] of packages) {
      await del.budget(20);
      const all = await listVersions(github, org, name);
      if (all === null) {
        log(`${name}: no such package; skipped`);
        continue;
      }
      const { remove, kept } = planManifests(all, tags);
      for (const version of kept) {
        log(`::warning::${name}: version ${version.id} carries ${pyRepr(tagsOf(version))}, not only doomed pre-release tags; kept`);
      }
      for (const version of remove) {
        const outcome = await del.remove(
          () => github.rest.packages.deletePackageVersionForOrg({ package_type: "container", package_name: name, org, package_version_id: version.id }),
          `DELETE ${name} version ${version.id}`,
        );
        if (outcome !== "refused") deleted += 1;
      }
      log(`${name}: ${remove.length} version(s) deleted`);
    }

    for (const release of doomed) {
      if (channel === "release" ? !isAnyPrerelease(release) : !release.prerelease || !isPrereleaseTag(release.tag_name, channel)) {
        // The rule, again, at the point of no return.
        throw new Error(`refusing to delete ${release.tag_name}: not a ${channel === "release" ? "" : `${channel} `}pre-release`);
      }
      await deleteRelease(del, github, owner, repo, release);
      log(`deleted ${release.tag_name}`);
    }
  } catch (error) {
    if (!(error instanceof OutOfTime)) throw error;
    log(`::warning::stopped at the deadline after ${deleted} package version(s); the next run carries on`);
    return 0;
  }
  log(`${doomed.length} pre-release(s) and ${deleted} package version(s) deleted`);
  return 0;
}
