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
// ORDER. Packages first, then the GitHub release, then the tag: a run that
// fails half way leaves the release listed, so the next run selects it again.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { pyRepr } from "./py-compat.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");
export const CHANNELS = new Set(["nightly", "dev"]);
export const SUFFIXES = ["", "-linux-amd64", "-linux-arm64"];

export function isPrereleaseTag(tag, channel) {
  return CHANNELS.has(channel) && new RegExp(`^v\\d+\\.\\d+\\.\\d+-${channel}\\.\\d+(?:\\.\\d+)*$`).test(tag);
}

const isAnyPrerelease = (r) => r.prerelease && [...CHANNELS].some((c) => isPrereleaseTag(r.tag_name, c));

/** The pre-releases of `channel` beyond the newest `keep`, newest first. */
export function select(releases, channel, keep) {
  const own = releases.filter((r) => r.prerelease && isPrereleaseTag(r.tag_name, channel));
  own.sort((a, b) => (a.created_at < b.created_at ? 1 : a.created_at > b.created_at ? -1 : 0));
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

/** The cosign signatures (`sha256-<digest>` tags) of the `removed` manifests, which no other manifest needs. */
export function signaturesOf(versions, removed) {
  const sigs = new Set();
  for (const version of removed) {
    const hex = String(version.name || "").replace(/^sha256:/, "");
    if (hex) for (const suffix of ["", ".sig", ".att"]) sigs.add(`sha256-${hex}${suffix}`);
  }
  return versions.filter((v) => tagsOf(v).length > 0 && tagsOf(v).every((t) => sigs.has(t)));
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

export async function ignore404(call, what) {
  try {
    await call();
  } catch (error) {
    if (error && error.status === 404) return;
    throw new Error(`${what} failed: ${error && error.message}`);
  }
}

/** Returns 0, or 2 for a bad argument. A failed API call throws. */
export async function main({ github, owner, repo, org, channel, keep, tag, root = ROOT, log = console.log }) {
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

  const publish = fs.readFileSync(path.join(root, ".github", "workflows", "publish.yml"), "utf8");
  const images = bashArray(publish, "IMAGES");
  const charts = bashArray(publish, "CHARTS");
  if (images.length < 5 || charts.length < 1) {
    log(`::error::read ${images.length} image(s) and ${charts.length} chart(s) from publish.yml's rollback arrays — the pattern stopped matching`);
    return 2;
  }

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

  const packages = [...images.map((n) => [n, imageTags]), ...charts.map((n) => [`charts/${n}`, chartTags])];
  let deleted = 0;
  for (const [name, tags] of packages) {
    const all = await listVersions(github, org, name);
    if (all === null) {
      log(`${name}: no such package; skipped`);
      continue;
    }
    const [manifests, kept] = doomedVersions(all, tags);
    const remove = [...manifests, ...signaturesOf(all, manifests)];
    for (const version of kept) {
      log(`::warning::${name}: version ${version.id} carries ${pyRepr(tagsOf(version))}, not only doomed pre-release tags; kept`);
    }
    for (const version of remove) {
      await ignore404(
        () => github.rest.packages.deletePackageVersionForOrg({ package_type: "container", package_name: name, org, package_version_id: version.id }),
        `DELETE ${name} version ${version.id}`,
      );
      deleted += 1;
    }
    log(`${name}: ${remove.length} version(s) deleted`);
  }

  for (const release of doomed) {
    if (channel === "release" ? !isAnyPrerelease(release) : !release.prerelease || !isPrereleaseTag(release.tag_name, channel)) {
      // The rule, again, at the point of no return.
      throw new Error(`refusing to delete ${release.tag_name}: not a ${channel === "release" ? "" : `${channel} `}pre-release`);
    }
    await ignore404(() => github.rest.repos.deleteRelease({ owner, repo, release_id: release.id }), `DELETE release ${release.tag_name}`);
    await ignore404(() => github.rest.git.deleteRef({ owner, repo, ref: `tags/${release.tag_name}` }), `DELETE tag ${release.tag_name}`);
    log(`deleted ${release.tag_name}`);
  }
  log(`${doomed.length} pre-release(s) and ${deleted} package version(s) deleted`);
  return 0;
}
