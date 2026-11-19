// Keep the repository's Actions cache under its cap, by key discipline.
//
//     ci-image-cleanup.yml, through actions/github-script:
//       const { main } = await import(`${process.env.GITHUB_WORKSPACE}/scripts/ci/prune-actions-caches.mjs`);
//       const code = await main({ github, owner, repo, dryRun });
//
// WHY (measured 2026-11-19). The Actions cache is capped at 10 GB and evicts
// least-recently-used. It held 11.2 GiB in 43 entries, so GitHub was evicting
// continuously and the entries a pull request reads were the ones that went:
// 61 of 61 sampled Rust jobs of the last day printed `No cache found.` The
// pull-request copies are gone since pull requests stopped writing
// (scripts/ci/check-workflow-cache-writes.mjs); what remains is main's own, and
// main's nightly writers add a NEW entry whenever a lockfile changes, leaving
// the old key behind: three Go caches of one family held 3.7 GiB.
//
// THE POLICY, applied in this order:
//
//   1. an entry of a ref that is not main, older than half a day, goes: a pull
//      request reads main's entries and its own writes are copies nothing reads;
//   2. of every FAMILY of keys (the key without its trailing hash, date or run
//      id) only the newest entry stays, except the families that are many by
//      design (BuildKit layer blobs, buildx indexes);
//   3. above CAP_BYTES, the least recently accessed entries go until it fits,
//      so the eviction GitHub would do anyway removes what is cold, not what a
//      nightly run is about to need.
//
// The cap is below GitHub's 10 GB on purpose: an entry saved while the cache is
// full evicts something else.

import { isEntrypoint } from "./py-compat.mjs";

export const CAP_BYTES = 9 * 1024 ** 3;
export const MAIN_REF = "refs/heads/main";
export const PR_COPY_GRACE_MS = 12 * 60 * 60 * 1000;
/** Families that hold many entries on purpose; the newest-only rule does not apply. */
export const MANY_BY_DESIGN = [/^buildkit-blob-/, /^index-/];

/** The key without its trailing hash segments, date, or run id and attempt. */
export function familyOf(key) {
  let k = key;
  for (let n = 0; n < 8; n += 1) {
    const next = k
      .replace(/-[0-9a-f]{8,}$/, "")
      .replace(/-\d{4}-\d{2}-\d{2}$/, "")
      .replace(/-\d+-\d+$/, "")
      .replace(/#\d+$/, "");
    if (next === k) break;
    k = next;
  }
  return k;
}

/**
 * @param {{id:number,key:string,ref:string,size_in_bytes:number,created_at:string,last_accessed_at:string}[]} caches
 * @returns {{doomed: {id:number,key:string,ref:string,size_in_bytes:number,reason:string}[], kept: number, keptBytes: number}}
 */
export function decide(caches, { now = Date.now(), capBytes = CAP_BYTES } = {}) {
  const doomed = new Map();
  const doom = (c, reason) => {
    if (!doomed.has(c.id)) doomed.set(c.id, { id: c.id, key: c.key, ref: c.ref, size_in_bytes: c.size_in_bytes, reason });
  };
  const created = (c) => Date.parse(c.created_at);
  for (const c of caches) {
    if (c.ref !== MAIN_REF && now - created(c) > PR_COPY_GRACE_MS) doom(c, "a copy written from a pull request ref: nothing reads it");
  }
  const families = new Map();
  for (const c of caches) {
    if (doomed.has(c.id)) continue;
    if (MANY_BY_DESIGN.some((re) => re.test(c.key))) continue;
    const family = `${c.ref}|${familyOf(c.key)}`;
    if (!families.has(family)) families.set(family, []);
    families.get(family).push(c);
  }
  for (const group of families.values()) {
    group.sort((a, b) => created(b) - created(a));
    for (const old of group.slice(1)) doom(old, `superseded: a newer entry of the family ${familyOf(old.key)} exists`);
  }
  let rest = caches.filter((c) => !doomed.has(c.id));
  let total = rest.reduce((s, c) => s + c.size_in_bytes, 0);
  rest.sort((a, b) => Date.parse(a.last_accessed_at) - Date.parse(b.last_accessed_at));
  for (const c of rest) {
    if (total <= capBytes) break;
    doom(c, "over the cap: the least recently accessed goes first");
    total -= c.size_in_bytes;
  }
  rest = caches.filter((c) => !doomed.has(c.id));
  return { doomed: [...doomed.values()], kept: rest.length, keptBytes: rest.reduce((s, c) => s + c.size_in_bytes, 0) };
}

const gib = (bytes) => (bytes / 1024 ** 3).toFixed(2);

export async function main({ github, owner, repo, dryRun = false, now = Date.now(), log = console.log, capBytes = CAP_BYTES }) {
  const caches = await github.paginate(github.rest.actions.getActionsCacheList, { owner, repo, per_page: 100 });
  const before = caches.reduce((s, c) => s + c.size_in_bytes, 0);
  log(`${caches.length} cache entr${caches.length === 1 ? "y" : "ies"}, ${gib(before)} GiB (cap ${gib(capBytes)} GiB)`);
  const { doomed, kept, keptBytes } = decide(caches, { now, capBytes });
  for (const d of doomed) log(`${dryRun ? "would delete" : "delete"}  ${gib(d.size_in_bytes)} GiB  ${d.key}  [${d.ref}]  ${d.reason}`);
  if (!dryRun) {
    for (const d of doomed) {
      try {
        await github.rest.actions.deleteActionsCacheById({ owner, repo, cache_id: d.id });
      } catch (error) {
        // An entry another run deleted first is not a failure.
        if (error.status !== 404) throw error;
      }
    }
  }
  log(`${kept} kept, ${gib(keptBytes)} GiB after ${doomed.length} deletion(s)${dryRun ? " (dry run: nothing deleted)" : ""}`);
  return 0;
}

if (isEntrypoint(import.meta.url)) {
  console.error("run through actions/github-script (ci-image-cleanup.yml)");
  process.exit(2);
}
