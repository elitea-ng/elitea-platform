// Cut the releases, and what they published, down to the newest few.
//
//     release-retention.yml, through actions/github-script, by an admin only:
//       const { main } = await import(`${process.env.GITHUB_WORKSPACE}/scripts/ci/prune-releases.mjs`);
//       const code = await main({ github, owner, repo, org, keepReleases, keepPrereleases, dryRun, retired, deadline });
//
// WHAT. publish.yml's `prune` job only ever deletes pre-releases. Stable
// releases, and the images and charts each one published, are kept until an
// admin runs this. It keeps the newest `keepReleases` stable releases and the
// newest `keepPrereleases` of each pre-release channel cut after the newest
// stable one, and deletes the rest:
//
//   * every other GitHub release (with the desktop app attached to it) and its
//     tag. A tag without a release is not touched.
//   * in every release image and chart, the versions whose tags all belong to
//     a deleted release, with their cosign signatures, and every UNTAGGED
//     version created before the oldest kept release. Untagged versions are
//     what superseded pushes, rolled-back runs and old attestations left;
//     nothing a kept release pushed is older than that release.
//   * the whole container packages named in `retired`, which no workflow
//     publishes any more. Named, not found: a workflow token cannot list the
//     org's packages (GitHub answers "Invalid argument"). Each must be this
//     repository's, and not a release image or chart, a CI image (`ci-*`) or
//     in KEEP_PACKAGES; one that is not refuses the whole run before anything
//     is deleted. A name with no package is warned about and skipped.
//
// The package lists are publish.yml's rollback arrays, read as
// prune-prereleases.mjs reads them.
//
// BUDGET AND ORDER are prune-prereleases.mjs's, through its `deleter`: the
// run waits out the rate limit (the first run had some 11,250 versions to
// delete) and stops cleanly before `deadline`; packages go first, each
// manifest with its signatures, then each tag and then its release; a version
// GitHub refuses to delete is reported and kept. Every step is safe to repeat:
// run it again and it carries on. Signatures a stopped run stranded before
// that order existed are swept as orphans.

import path from "node:path";
import { fileURLToPath } from "node:url";

import {
  CHANNELS,
  STABLE_TAG,
  SUFFIXES,
  OutOfTime,
  deleteRelease,
  deleter,
  isAnyPrerelease,
  isStable,
  listVersions,
  newestFirst,
  orphanSignatures,
  planManifests,
  readPackageLists,
  select,
  selectSuperseded,
  tagsOf,
} from "./prune-prereleases.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

// Published by this repository and still in use, though no release builds it:
// the BuildKit image the CI builders pin by digest (mirror-buildkit.yml).
export const KEEP_PACKAGES = new Set(["buildkit"]);

/** What to keep and what to delete; `cutoff` is the oldest kept release's created_at. */
export function plan(releases, keepReleases, keepPrereleases) {
  const stable = releases.filter(isStable).sort(newestFirst);
  const keptStable = stable.slice(0, keepReleases);
  const doomedStable = stable.slice(keepReleases);
  const doomedPre = new Map();
  for (const channel of CHANNELS) for (const r of select(releases, channel, keepPrereleases)) doomedPre.set(r.id, r);
  if (keptStable.length) for (const r of selectSuperseded(releases, keptStable[0].tag_name)) doomedPre.set(r.id, r);
  const doomed = new Set([...doomedStable, ...doomedPre.values()].map((r) => r.id));
  const kept = releases.filter((r) => !doomed.has(r.id) && (isStable(r) || isAnyPrerelease(r)));
  const cutoff = kept.length ? kept.map((r) => r.created_at).sort()[0] : null;
  return { keptStable, doomedStable, doomedPre: [...doomedPre.values()].sort(newestFirst), kept, cutoff };
}

/**
 * The image and chart tags the doomed releases minted. A stable release also
 * moved `<major>.<minor>`; that goes only when no kept release shares it.
 */
export function doomedTags(doomedReleases, keptStable) {
  const keptMinors = new Set(keptStable.map((r) => r.tag_name.match(STABLE_TAG).slice(1, 3).join(".")));
  const images = new Set();
  const charts = new Set();
  for (const release of doomedReleases) {
    const version = release.tag_name.slice(1);
    for (const suffix of SUFFIXES) images.add(version + suffix);
    charts.add(version);
    const stable = isStable(release) && release.tag_name.match(STABLE_TAG);
    if (stable && !keptMinors.has(`${stable[1]}.${stable[2]}`)) images.add(`${stable[1]}.${stable[2]}`);
  }
  return { images, charts };
}

/** The versions of one package to delete, and the ones kept for a foreign tag. */
export function planPackage(versions, tags, cutoff) {
  const { remove, kept } = planManifests(versions, tags);
  const doomed = new Set(remove.map((v) => v.id));
  const rest = versions.filter((v) => !doomed.has(v.id));
  const orphans = orphanSignatures(rest.filter((v) => tagsOf(v).length > 0)).filter((v) => !doomed.has(v.id));
  const untagged = cutoff ? rest.filter((v) => tagsOf(v).length === 0 && v.created_at < cutoff) : [];
  return { remove: [...remove, ...orphans, ...untagged], kept };
}

/** Why `name` may not be deleted as a retired package, or null. */
export function refuseRetired(name, images, charts) {
  if (!/^[a-z0-9][a-z0-9._/-]*$/.test(name)) return "is not a package name";
  if (images.includes(name) || charts.some((c) => `charts/${c}` === name)) return "is a release image or chart publish.yml still publishes";
  if (name.startsWith("ci-")) return "is a CI image; ci-image-cleanup.yml prunes those";
  if (KEEP_PACKAGES.has(name)) return "is still in use (KEEP_PACKAGES)";
  return null;
}

/** The space- or comma-separated package names of the workflow input. */
export const parseRetired = (text) => [...new Set(String(text || "").split(/[\s,]+/).filter(Boolean))].sort();

/** Returns 0 (also when it stopped at the deadline; it says so), or 2 for a bad argument. A failed API call throws. */
export async function main({
  github,
  owner,
  repo,
  org,
  keepReleases,
  keepPrereleases,
  dryRun = true,
  retired = [],
  deadline = Infinity,
  root = ROOT,
  log = console.log,
  now,
  sleep,
}) {
  for (const [name, value] of [["keepReleases", keepReleases], ["keepPrereleases", keepPrereleases]]) {
    if (!Number.isInteger(value) || value < 1) {
      log(`::error::${name} must be an integer of at least 1; got ${String(value)}`);
      return 2;
    }
  }
  if (!owner || !repo || !org) {
    log("::error::owner, repo and org are required");
    return 2;
  }
  const lists = readPackageLists(root, log);
  if (!lists) return 2;
  for (const name of retired) {
    const reason = refuseRetired(name, lists.images, lists.charts);
    if (reason) {
      log(`::error::retired package ${JSON.stringify(name)} ${reason}; nothing deleted`);
      return 2;
    }
  }
  // Checked before anything is deleted: a package of another repository
  // refuses the run. A missing one is only warned about, since a rerun that
  // carries on after a stop finds the packages it already deleted missing;
  // the dry run shows a typo.
  const del = deleter({ github, dryRun, deadline, now, sleep, log });
  const retiredFound = [];
  if (retired.length) await del.budget(retired.length);
  for (const name of retired) {
    let pkg;
    try {
      ({ data: pkg } = await github.rest.packages.getPackageForOrganization({ package_type: "container", package_name: name, org }));
    } catch (error) {
      if (error && error.status === 404) {
        log(`::warning::retired package ${name}: no such package; skipped`);
        continue;
      }
      throw new Error(`GET package ${name} failed: ${error && error.message}`);
    }
    if (!pkg.repository || pkg.repository.name !== repo) {
      log(`::error::retired package ${name} belongs to ${(pkg.repository && pkg.repository.name) || "no repository"}, not ${repo}; nothing deleted`);
      return 2;
    }
    retiredFound.push(name);
  }

  const releases = await github.paginate(github.rest.repos.listReleases, { owner, repo, per_page: 100 });
  const { keptStable, doomedStable, doomedPre, kept, cutoff } = plan(releases, keepReleases, keepPrereleases);
  const doomed = [...doomedStable, ...doomedPre];
  log(`${dryRun ? "DRY RUN: nothing is deleted. " : ""}${releases.length} release(s); keeping ${kept.length}: ${kept.map((r) => r.tag_name).join(", ")}`);
  log(`deleting ${doomedStable.length} stable release(s) and ${doomedPre.length} pre-release(s); untagged package versions older than ${cutoff || "nothing"} go too`);
  const tags = doomedTags(doomed, keptStable);

  let deleted = 0;
  try {
    for (const [name, tagSet] of [...lists.images.map((n) => [n, tags.images]), ...lists.charts.map((n) => [`charts/${n}`, tags.charts])]) {
      await del.budget(40);
      const all = await listVersions(github, org, name);
      if (all === null) {
        log(`${name}: no such package; skipped`);
        continue;
      }
      const { remove: versions, kept: foreign } = planPackage(all, tagSet, cutoff);
      for (const version of foreign) {
        log(`::warning::${name}: version ${version.id} carries ${JSON.stringify(tagsOf(version))}, not only tags of deleted releases; kept`);
      }
      let removed = 0;
      for (const version of versions) {
        const outcome = await del.remove(
          () => github.rest.packages.deletePackageVersionForOrg({ package_type: "container", package_name: name, org, package_version_id: version.id }),
          `DELETE ${name} version ${version.id}`,
        );
        if (outcome !== "refused") {
          removed += 1;
          deleted += 1;
        }
      }
      log(`${name}: ${removed} of ${all.length} version(s) ${dryRun ? "would be " : ""}deleted`);
    }

    if (!retired.length) log("no retired packages named");
    for (const name of retiredFound) {
      const outcome = await del.remove(() => github.rest.packages.deletePackageForOrg({ package_type: "container", package_name: name, org }), `DELETE package ${name}`);
      if (outcome !== "refused") log(`retired package ${name}: ${dryRun ? "would be " : ""}deleted`);
    }

    const keptIds = new Set(kept.map((r) => r.id));
    for (const release of doomed) {
      if (keptIds.has(release.id) || !(isStable(release) || isAnyPrerelease(release))) {
        // The rule, again, at the point of no return.
        throw new Error(`refusing to delete ${release.tag_name}: it is kept, or not a release publish.yml cut`);
      }
      await deleteRelease(del, github, owner, repo, release);
      log(`${dryRun ? "would delete" : "deleted"} ${release.tag_name}`);
    }
  } catch (error) {
    if (!(error instanceof OutOfTime)) throw error;
    log(`::warning::stopped at the deadline after ${deleted} package version(s); run the workflow again to carry on`);
    return 0;
  }
  log(`${doomed.length} release(s) and ${deleted} package version(s) ${dryRun ? "would be " : ""}deleted`);
  return 0;
}
