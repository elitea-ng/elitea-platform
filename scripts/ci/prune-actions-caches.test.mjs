// Scenarios for prune-actions-caches.mjs: the three rules, in order, and the real
// measurement of 2026-11-19 as the worked example.

import assert from "node:assert/strict";
import { test } from "node:test";

import { CAP_BYTES, MAIN_REF, decide, familyOf, main } from "./prune-actions-caches.mjs";

const GIB = 1024 ** 3;
const NOW = Date.parse("2026-11-19T12:00:00Z");
const iso = (hoursAgo) => new Date(NOW - hoursAgo * 3600 * 1000).toISOString();
let id = 0;
const entry = (key, gib, { ref = MAIN_REF, created = 24, accessed = 1 } = {}) => ({
  id: ++id,
  key,
  ref,
  size_in_bytes: gib * GIB,
  created_at: iso(created),
  last_accessed_at: iso(accessed),
});

test("a key's family drops its hash, date and run id", () => {
  const h = "a".repeat(64);
  assert.equal(familyOf(`go-v1-Linux-X64-go1.26.9-${h}`), "go-v1-Linux-X64-go1.26.9");
  assert.equal(familyOf("windows-host-windows-host-Windows_NT-x64-bc559819-c55e23bc"), "windows-host-windows-host-Windows_NT-x64");
  assert.equal(familyOf(`buildkit-mounts-v3-Linux-go-${h}-123456789-1`), "buildkit-mounts-v3-Linux-go");
  assert.equal(familyOf("cache-trivy-2026-11-19"), "cache-trivy");
  assert.equal(familyOf("trivy-binary-v0.70.0-Linux-X64"), "trivy-binary-v0.70.0-Linux-X64");
});

test("a pull-request ref's entry goes once it is half a day old, a fresh one stays", () => {
  const old = entry("k-1", 1, { ref: "refs/pull/9/merge", created: 20 });
  const fresh = entry("k-2", 1, { ref: "refs/pull/9/merge", created: 2 });
  const { doomed } = decide([old, fresh], { now: NOW });
  assert.deepEqual(doomed.map((d) => d.id), [old.id]);
  assert.match(doomed[0].reason, /pull request ref/);
});

test("an entry of a tag or another branch is not a pull-request copy and stays", () => {
  const tag = entry("tag-aaaaaaaa", 1, { ref: "refs/tags/v1.2.3", created: 40 });
  const branch = entry("branch-bbbbbbbb", 1, { ref: "refs/heads/feature", created: 40 });
  assert.deepEqual(decide([tag, branch], { now: NOW }).doomed, []);
});

test("only the newest entry of a family stays, per ref", () => {
  const h = (c) => c.repeat(64);
  const older = entry(`rust-v0-g-${h("a")}`, 1.2, { created: 72 });
  const newer = entry(`rust-v0-g-${h("b")}`, 1.2, { created: 24 });
  const other = entry(`rust-v0-L-${h("c")}`, 1, { created: 100 });
  const { doomed, kept } = decide([older, newer, other], { now: NOW });
  assert.deepEqual(doomed.map((d) => d.id), [older.id]);
  assert.equal(kept, 2);
});

test("the families that are many by design keep every entry", () => {
  const blobs = Array.from({ length: 5 }, (_, i) => entry(`buildkit-blob-1-sha256:${String(i).repeat(64)}`, 0.02, { created: 30 + i }));
  assert.equal(decide(blobs, { now: NOW }).doomed.length, 0);
});

test("over the cap, the least recently accessed entries go first until it fits", () => {
  const a = entry("a-aaaaaaaa", 4, { accessed: 50 });
  const b = entry("b-bbbbbbbb", 4, { accessed: 10 });
  const c = entry("c-cccccccc", 4, { accessed: 5 });
  const { doomed, keptBytes } = decide([a, b, c], { now: NOW, capBytes: 9 * GIB });
  assert.deepEqual(doomed.map((d) => d.id), [a.id]);
  assert.ok(keptBytes <= 9 * GIB);
  assert.match(doomed[0].reason, /over the cap/);
});

test("the 2026-11-19 measurement: 11.2 GiB with three stale Go entries comes under the cap", () => {
  const h = (c) => c.repeat(64);
  const caches = [
    entry(`go-v1-Linux-X64-go1.26.9-${h("a")}`, 1.3, { created: 90 }),
    entry(`go-v1-Linux-X64-go1.26.9-${h("b")}`, 1.2, { created: 60 }),
    entry(`go-v1-Linux-X64-go1.26.9-${h("c")}`, 1.2, { created: 20 }),
    entry("windows-host-windows-host-Windows_NT-x64-bc559819-c55e23bc", 2.58, { created: 22 }),
    entry("handoff-journey-handoff-journey-Linux-x64-770ca156-cc1d5db9", 1.58, { created: 22 }),
    entry("v0-rust-worker-rust-test-Linux-x64-a74c6f26-23dbcfb8", 1.27, { created: 22 }),
    entry("buildkit-blob-1-sha256:" + h("d"), 0.62, { created: 22 }),
    entry(`buildkit-mounts-v3-Linux-go-${h("e")}-1-1`, 0.57, { created: 22 }),
    entry(`buildkit-mounts-v3-Linux-rust-${h("f")}-1-1`, 0.48, { created: 22 }),
  ];
  const before = caches.reduce((s, c) => s + c.size_in_bytes, 0);
  assert.ok(before > CAP_BYTES);
  const { doomed, keptBytes } = decide(caches, { now: NOW });
  // Two go-v1 entries stay (two go.sum lists); the oldest of three goes.
  assert.equal(doomed.filter((d) => d.reason.startsWith("superseded")).length, 1);
  assert.ok(keptBytes <= CAP_BYTES, `${keptBytes / GIB} GiB kept`);
});

test("main deletes what is decided, tolerates a 404, and a dry run deletes nothing", async () => {
  const caches = [entry("p-aaaaaaaa", 1, { ref: "refs/pull/1/merge", created: 30 }), entry("p-bbbbbbbb", 1, { ref: "refs/pull/2/merge", created: 30 }), entry("m-cccccccc", 1)];
  const deleted = [];
  const github = {
    paginate: async () => caches,
    rest: {
      actions: {
        getActionsCacheList: () => {},
        deleteActionsCacheById: async ({ cache_id }) => {
          deleted.push(cache_id);
          if (cache_id === caches[1].id) throw Object.assign(new Error("gone"), { status: 404 });
        },
      },
    },
  };
  const quiet = () => {};
  assert.equal(await main({ github, owner: "o", repo: "r", dryRun: true, now: NOW, log: quiet }), 0);
  assert.deepEqual(deleted, []);
  assert.equal(await main({ github, owner: "o", repo: "r", now: NOW, log: quiet }), 0);
  assert.deepEqual(deleted.sort(), [caches[0].id, caches[1].id].sort());
});
