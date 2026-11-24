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
//       resolves (--resolve is implied), then writes
//       <dir>/<owner>_<repo>/<sha>.tar.gz for each.
//
// It reads the workflow text with regular expressions (skipping block
// scalars such as `run: |`) and uses only node's standard library. It fails,
// rather than passes, when it finds fewer refs than MINIMUM_REF_COUNT or ends
// with fewer archives than MINIMUM_ARCHIVE_COUNT: a scan that stopped matching
// must not bake an empty cache that looks like a fix. One ref that does not
// resolve, or one archive that does not download, is a warning and is left
// out: the runner downloads it as it always has, and the weekly image (with
// its OS updates) must not wait on one stale action.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

export const MINIMUM_REF_COUNT = 20;
export const MINIMUM_ARCHIVE_COUNT = 15;
// The directory the image bakes the archives into and points the runner at
// (Containerfile ENV; deploy/ci-runners/values-eng-runner.yaml names it too).
export const CACHE_DIR = "/opt/action-archive-cache";
const FETCH_TIMEOUT_MS = 120_000;

const USES_LINE = /^\s*(?:-\s+)?uses:\s*["']?([^\s"'#]+)["']?\s*(?:#.*)?$/;
const REMOTE_REF = /^([A-Za-z0-9_.-]+)\/([A-Za-z0-9_.-]+)((?:\/[^@\s]+)?)@([^\s@]+)$/;
const SHA = /^[0-9a-f]{40}$/;
// `key: |`, `- run: >-` ...: the lines after it, indented deeper than the
// key, are text, not YAML.
const BLOCK_SCALAR = /^(\s*)(?:-\s+)?([A-Za-z0-9_.-]+):\s*[|>][-+0-9]*\s*(?:#.*)?$/;

// The remote action refs one workflow or action.yml holds. Local actions
// (./...), docker:// images, block-scalar text (a `run: |` script that says
// "uses:") and anything that is not owner/repo@ref are not archives.
export function usesRefs(text) {
  const refs = [];
  let blockIndent = -1; // the key column of the block scalar we are inside
  for (const line of text.split(/\r?\n/)) {
    const indent = line.length - line.trimStart().length;
    if (blockIndent >= 0) {
      if (line.trim() === "" || indent > blockIndent) continue;
      blockIndent = -1;
    }
    const block = BLOCK_SCALAR.exec(line);
    if (block) {
      blockIndent = line.indexOf(block[2], block[1].length);
      continue;
    }
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

// owner/repo[/path]@ref -> { repo: "owner/repo", path, ref }
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
// downloads those too. A ref that does not resolve goes to `warn` and is
// left out.
export async function resolveArchives(refs, api, warn = () => {}) {
  const archives = new Map(); // "repo@sha" -> { repo, sha, refs: Set }
  const seen = new Set();
  // One lookup per repo@tag in a run, so restore@v6 and save@v6 cannot land
  // on two commits if the tag moves mid-run.
  const shas = new Map();
  const ymls = new Map();
  const once = (memo, key, fn) => {
    if (!memo.has(key)) memo.set(key, fn());
    return memo.get(key);
  };
  const queue = [...refs];
  while (queue.length > 0) {
    const ref = queue.shift();
    if (seen.has(ref)) continue;
    seen.add(ref);
    const { repo, path: subpath, ref: name } = parseRef(ref);
    let sha;
    let yml;
    try {
      sha = SHA.test(name) ? name : await once(shas, `${repo}@${name}`, () => api.commitSha(repo, name));
      if (!SHA.test(sha || "")) throw new Error(`did not resolve to a commit (got ${sha})`);
      yml = await once(ymls, `${repo}@${sha}/${subpath}`, () => api.actionYml(repo, sha, subpath));
    } catch (err) {
      warn(`${ref}: ${err.message}; left out of the cache (the runner downloads it)`);
      continue;
    }
    const key = `${repo}@${sha}`;
    if (!archives.has(key)) archives.set(key, { repo, sha, refs: new Set() });
    archives.get(key).refs.add(ref);
    if (yml && /^\s*using:\s*["']?composite["']?\s*$/m.test(yml)) queue.push(...usesRefs(yml));
  }
  return [...archives.values()]
    .map((a) => ({ repo: a.repo, sha: a.sha, refs: [...a.refs].sort() }))
    .sort((a, b) => (a.repo === b.repo ? a.sha.localeCompare(b.sha) : a.repo.localeCompare(b.repo)));
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// A 403 is a rate limit (to retry) only when GitHub says so.
export function isRetryable(status, headers) {
  if (status === 429 || status >= 500) return true;
  return status === 403 && (headers.get("retry-after") !== null || headers.get("x-ratelimit-remaining") === "0");
}

// fetch with the token and a timeout, retrying network errors and rate
// limits (Retry-After first, then x-ratelimit-reset, capped at 5 minutes).
//
// A 403 that is not a rate limit is retried once WITHOUT the token: an
// organisation with an IP allow list (aquasecurity, measured on #3004)
// refuses the workflow token from a hosted runner's address even for its
// public repositories, and answers the same request anonymously.
async function request(url, token, { raw = false, attempts = 5 } = {}) {
  let auth = token;
  for (let attempt = 1; ; attempt++) {
    let res;
    try {
      res = await fetch(url, {
        headers: {
          ...(auth ? { authorization: `Bearer ${auth}` } : {}),
          accept: raw ? "application/vnd.github.raw" : "application/vnd.github+json",
          "x-github-api-version": "2022-11-28",
          "user-agent": "elitea-platform-action-archive-list",
        },
        redirect: "follow",
        signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
      });
    } catch (err) {
      if (attempt >= attempts) throw new Error(`${url}: ${err.message}`);
      await sleep(2000 * attempt);
      continue;
    }
    if (res.ok || res.status === 404) return res;
    if (res.status === 403 && auth && !isRetryable(res.status, res.headers)) {
      await res.body?.cancel().catch(() => {});
      auth = null;
      continue;
    }
    if (attempt >= attempts || !isRetryable(res.status, res.headers)) {
      throw new Error(`${url}: HTTP ${res.status} ${await res.text().catch(() => "")}`.slice(0, 500));
    }
    await res.body?.cancel().catch(() => {});
    const after = Number(res.headers.get("retry-after"));
    const reset = Number(res.headers.get("x-ratelimit-reset")) * 1000 - Date.now();
    const wait = after > 0 ? after * 1000 : reset > 0 ? reset : 2000 * attempt;
    await sleep(Math.min(wait, 300_000));
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
      if (!res.ok) throw new Error(`tarball HTTP ${res.status}`);
      return Buffer.from(await res.arrayBuffer());
    },
  };
}

// Writes each archive under dir; an archive already there is kept. The gzip
// magic is checked so an HTML error page is never baked in. An archive that
// fails goes to `warn` and is left out.
export async function downloadArchives(archives, dir, api, warn = () => {}) {
  let bytes = 0;
  let count = 0;
  for (const { repo, sha } of archives) {
    const file = path.join(dir, archivePath(repo, sha));
    if (!fs.existsSync(file)) {
      try {
        const body = await api.tarball(repo, sha);
        if (body.length < 2 || body[0] !== 0x1f || body[1] !== 0x8b) throw new Error("not a gzip archive");
        fs.mkdirSync(path.dirname(file), { recursive: true });
        fs.writeFileSync(`${file}.part`, body);
        fs.renameSync(`${file}.part`, file);
      } catch (err) {
        warn(`${repo}@${sha}: ${err.message}; left out of the cache (the runner downloads it)`);
        continue;
      }
    }
    bytes += fs.statSync(file).size;
    count++;
  }
  return { bytes, count };
}

const warnLine = (msg) => console.log(`::warning::action-archive-list: ${msg}`);

export async function main(argv, { root, env = process.env, api, log = console.log, warn = warnLine } = {}) {
  const di = argv.indexOf("--download");
  const dir = di >= 0 ? argv[di + 1] : null;
  if (di >= 0 && (!dir || dir.startsWith("--"))) throw new Error("--download needs a directory");
  for (const [i, a] of argv.entries()) {
    if (di >= 0 && i === di + 1) continue;
    if (a !== "--resolve" && a !== "--download") throw new Error(`unknown argument ${a}`);
  }
  const resolve = argv.includes("--resolve");
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
  const floor = (n, what) => {
    if (n < MINIMUM_ARCHIVE_COUNT) throw new Error(`only ${n} ${what}, below the floor of ${MINIMUM_ARCHIVE_COUNT}`);
  };
  const archives = await resolveArchives(refs, client, warn);
  floor(archives.length, "archives resolved");
  if (!dir) {
    for (const a of archives) log(`${a.repo}@${a.sha}`);
    return { refs, archives };
  }
  const { bytes, count } = await downloadArchives(archives, dir, client, warn);
  floor(count, "archives downloaded");
  log(`${count} archives from ${refs.length} refs, ${(bytes / 1024 / 1024).toFixed(1)} MiB, in ${dir}`);
  return { refs, archives, bytes, count };
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");
  main(process.argv.slice(2), { root }).catch((err) => {
    console.error(`::error::action-archive-list: ${err.message}`);
    process.exit(1);
  });
}
