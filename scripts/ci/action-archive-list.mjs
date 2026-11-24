#!/usr/bin/env node
// The set of action archives the eng-runner image bakes in.
//
// WHY. Every job on the self-hosted pool `eng-runner` downloads the tarball of
// every action it uses from codeload.github.com in "Prepare all required
// actions". All pool pods leave through one egress address, and codeload
// rate-limits by address: from 2026-11-22 jobs failed in "Set up job" with
// `429 (Too Many Requests)` after the runner's three attempts. The runner
// already sends the job token on that download, so authentication does not
// lift the limit.
//
// THE FIX. actions/runner (2.320+) reads a pre-downloaded archive from the
// directory named by ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE before it downloads
// anything (src/Runner.Worker/ActionManager.cs):
//
//   <cache>/<owner>_<repo>/<commit sha>.tar.gz
//
// <owner>_<repo> is the resolved `owner/repo` with the slash turned into `_`
// (a sub-path action such as actions/cache/restore uses the actions_cache
// archive), and <commit sha> is the commit the ref resolved to (a tag such as
// v7 is resolved by the service first). A missing file is not an error: the
// runner downloads that one action as before. So a Dependabot bump, or a
// moving major tag, costs one download per job until the weekly image rebuild
// (eng-runner-image.yml) bakes the new commit in.
//
// USAGE.
//   node scripts/ci/action-archive-list.mjs
//       the distinct remote `uses:` refs of .github/workflows and
//       .github/actions, one per line (offline).
//   node scripts/ci/action-archive-list.mjs --resolve
//       the archive set: each ref resolved to its commit through the GitHub
//       API, plus the actions a remote composite action uses, recursively;
//       one `owner/repo@sha` per line. Needs GITHUB_TOKEN or GH_TOKEN.
//   node scripts/ci/action-archive-list.mjs --download <dir>
//       resolves, then writes <dir>/<owner>_<repo>/<sha>.tar.gz for each.
//
// It reads the workflow text with regular expressions and uses only node's
// standard library. It fails, rather than passes, when it finds fewer refs
// than MINIMUM_REF_COUNT: a scan that stopped matching must not bake an empty
// cache that looks like a fix.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

export const MINIMUM_REF_COUNT = 20;

const USES_LINE = /^\s*(?:-\s+)?uses:\s*["']?([^\s"'#]+)["']?\s*(?:#.*)?$/;
const REMOTE_REF = /^([A-Za-z0-9_.-]+)\/([A-Za-z0-9_.-]+)((?:\/[^@\s]+)?)@([^\s@]+)$/;
const SHA = /^[0-9a-f]{40}$/;

// The remote action refs one workflow or action.yml holds. Local actions
// (./...), docker:// images and anything that is not owner/repo@ref (text in
// a run: block that happens to say "uses:") are not archives.
export function usesRefs(text) {
  const refs = [];
  for (const line of text.split(/\r?\n/)) {
    const m = USES_LINE.exec(line);
    if (!m) continue;
    const ref = m[1];
    if (ref.startsWith("./") || ref.startsWith("docker://")) continue;
    if (!REMOTE_REF.test(ref)) continue;
    // A reusable workflow in another repository is fetched by the service,
    // not by the runner's action download.
    if (/\/\.github\/workflows\//.test(ref)) continue;
    refs.push(ref);
  }
  return refs;
}

// owner/repo[/path]@ref -> { repo: "owner/repo", ref }
export function parseRef(ref) {
  const m = REMOTE_REF.exec(ref);
  if (!m) throw new Error(`not an owner/repo@ref action reference: ${ref}`);
  return { repo: `${m[1]}/${m[2]}`, path: m[3].replace(/^\//, ""), ref: m[4] };
}

// The file the runner looks for, relative to the cache directory.
export function archivePath(repo, sha) {
  if (!SHA.test(sha)) throw new Error(`not a commit sha: ${sha}`);
  return path.join(repo.replace(/\//g, "_"), `${sha}.tar.gz`);
}

function walk(dir, keep) {
  if (!fs.existsSync(dir)) return [];
  const out = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...walk(full, keep));
    else if (keep(entry.name)) out.push(full);
  }
  return out;
}

// Every distinct remote ref under <root>/.github/{workflows,actions}, sorted.
export function scanRefs(root) {
  const files = [
    ...walk(path.join(root, ".github", "workflows"), (n) => /\.ya?ml$/.test(n)),
    ...walk(path.join(root, ".github", "actions"), (n) => /^action\.ya?ml$/.test(n)),
  ];
  const refs = new Set();
  for (const file of files) for (const ref of usesRefs(fs.readFileSync(file, "utf8"))) refs.add(ref);
  return { files: files.length, refs: [...refs].sort() };
}

// Resolves refs to the archive set. `api` is injected:
//   api.commitSha(repo, ref)            -> the commit sha the ref names
//   api.actionYml(repo, sha, subpath)   -> the action.yml text, or null
// A composite action's own `uses:` are followed, because the runner
// downloads those too.
export async function resolveArchives(refs, api) {
  const archives = new Map(); // "repo@sha" -> { repo, sha, refs: Set }
  const seen = new Set();
  const queue = [...refs];
  while (queue.length > 0) {
    const ref = queue.shift();
    if (seen.has(ref)) continue;
    seen.add(ref);
    const { repo, path: subpath, ref: name } = parseRef(ref);
    const sha = SHA.test(name) ? name : await api.commitSha(repo, name);
    if (!SHA.test(sha || "")) throw new Error(`${ref} did not resolve to a commit (got ${sha})`);
    const key = `${repo}@${sha}`;
    if (!archives.has(key)) archives.set(key, { repo, sha, refs: new Set() });
    archives.get(key).refs.add(ref);
    const yml = await api.actionYml(repo, sha, subpath);
    if (yml && /^\s*using:\s*["']?composite["']?\s*$/m.test(yml)) queue.push(...usesRefs(yml));
  }
  return [...archives.values()]
    .map((a) => ({ repo: a.repo, sha: a.sha, refs: [...a.refs].sort() }))
    .sort((a, b) => (a.repo === b.repo ? a.sha.localeCompare(b.sha) : a.repo.localeCompare(b.repo)));
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// fetch with the token, retrying 429 and 5xx (Retry-After first).
async function request(url, token, { raw = false, attempts = 5 } = {}) {
  for (let attempt = 1; ; attempt++) {
    const res = await fetch(url, {
      headers: {
        authorization: `Bearer ${token}`,
        accept: raw ? "application/vnd.github.raw" : "application/vnd.github+json",
        "x-github-api-version": "2022-11-28",
        "user-agent": "elitea-platform-action-archive-list",
      },
      redirect: "follow",
    });
    if (res.ok || res.status === 404) return res;
    if (attempt >= attempts || !(res.status === 429 || res.status >= 500)) {
      throw new Error(`${url}: HTTP ${res.status} ${await res.text().catch(() => "")}`.slice(0, 500));
    }
    const after = Number(res.headers.get("retry-after"));
    await sleep(Number.isFinite(after) && after > 0 ? after * 1000 : 2000 * attempt);
  }
}

export function githubApi(token, base = "https://api.github.com") {
  return {
    async commitSha(repo, ref) {
      const res = await request(`${base}/repos/${repo}/commits/${encodeURIComponent(ref)}`, token);
      if (res.status === 404) throw new Error(`${repo}@${ref}: no such ref`);
      return (await res.json()).sha;
    },
    async actionYml(repo, sha, subpath) {
      for (const name of ["action.yml", "action.yaml"]) {
        const file = subpath ? `${subpath}/${name}` : name;
        const res = await request(`${base}/repos/${repo}/contents/${file}?ref=${sha}`, token, { raw: true });
        if (res.ok) return res.text();
      }
      return null;
    },
    // The same archive the runner fetches (codeload tar.gz of the commit).
    async tarball(repo, sha) {
      const res = await request(`${base}/repos/${repo}/tarball/${sha}`, token);
      if (!res.ok) throw new Error(`${repo}@${sha}: tarball HTTP ${res.status}`);
      return Buffer.from(await res.arrayBuffer());
    },
  };
}

// Writes each archive under dir; an archive already there is kept. The gzip
// magic is checked so an HTML error page is never baked in.
export async function downloadArchives(archives, dir, api) {
  let bytes = 0;
  for (const { repo, sha } of archives) {
    const file = path.join(dir, archivePath(repo, sha));
    if (!fs.existsSync(file)) {
      const body = await api.tarball(repo, sha);
      if (body.length < 2 || body[0] !== 0x1f || body[1] !== 0x8b) throw new Error(`${repo}@${sha}: not a gzip archive`);
      fs.mkdirSync(path.dirname(file), { recursive: true });
      fs.writeFileSync(`${file}.part`, body);
      fs.renameSync(`${file}.part`, file);
    }
    bytes += fs.statSync(file).size;
  }
  return bytes;
}

export async function main(argv, { root, env = process.env, api, log = console.log } = {}) {
  const resolve = argv.includes("--resolve");
  const di = argv.indexOf("--download");
  const dir = di >= 0 ? argv[di + 1] : null;
  if (di >= 0 && !dir) throw new Error("--download needs a directory");
  const { files, refs } = scanRefs(root);
  if (refs.length < MINIMUM_REF_COUNT) {
    throw new Error(`found ${refs.length} action refs in ${files} files, below the floor of ${MINIMUM_REF_COUNT}: the scan stopped matching`);
  }
  if (!resolve && !dir) {
    for (const r of refs) log(r);
    return { refs };
  }
  const token = env.GITHUB_TOKEN || env.GH_TOKEN;
  const client = api || (token ? githubApi(token) : null);
  if (!client) throw new Error("--resolve and --download need GITHUB_TOKEN or GH_TOKEN");
  const archives = await resolveArchives(refs, client);
  if (!dir) {
    for (const a of archives) log(`${a.repo}@${a.sha}`);
    return { refs, archives };
  }
  const bytes = await downloadArchives(archives, dir, client);
  log(`${archives.length} archives from ${refs.length} refs, ${(bytes / 1024 / 1024).toFixed(1)} MiB, in ${dir}`);
  return { refs, archives, bytes };
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");
  main(process.argv.slice(2), { root }).catch((err) => {
    console.error(`::error::action-archive-list: ${err.message}`);
    process.exit(1);
  });
}
