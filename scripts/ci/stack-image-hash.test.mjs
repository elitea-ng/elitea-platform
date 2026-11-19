// Scenarios for stack-image-hash.mjs: the content address of a CI image moves
// with every input of the image and with nothing else.

import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import { HASH_LENGTH, addHashes, contentHash, main } from "./stack-image-hash.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

/** A stand-in for `git ls-files -s -z -- <path>` over the files on disk under it. */
function gitOverDisk(workspace) {
  return (cmd, args) => {
    const target = args.at(-1);
    const abs = path.join(workspace, target);
    const files = [];
    const walk = (p) => {
      if (!fs.existsSync(p)) return;
      if (fs.statSync(p).isDirectory()) fs.readdirSync(p).forEach((n) => walk(path.join(p, n)));
      else files.push(p);
    };
    walk(abs);
    const stdout = files
      .map((f) => `100644 ${crypto.createHash("sha1").update(fs.readFileSync(f)).digest("hex")} 0\t${path.relative(workspace, f)}`)
      .join("\0");
    return { status: 0, stdout };
  };
}

function fixture(callback) {
  const dir = fs.mkdtempSync(path.join(ROOT, "deploy", ".cas-test-"));
  try {
    fs.writeFileSync(path.join(dir, "Containerfile"), "FROM base AS build\nCOPY src/ /src\nFROM build AS other\nCOPY other.txt /o\nFROM scratch AS final\nCOPY --from=build /src /src\n");
    fs.mkdirSync(path.join(dir, "src"));
    fs.writeFileSync(path.join(dir, "src", "main.go"), "package main\n");
    fs.writeFileSync(path.join(dir, "other.txt"), "other\n");
    fs.writeFileSync(path.join(dir, "unrelated.txt"), "unrelated\n");
    return callback(dir, path.relative(ROOT, dir));
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

const hashOf = (rel, over = {}) => contentHash({ workspace: ROOT, context: rel, dockerfile: "Containerfile", target: "final", run: gitOverDisk(ROOT), ...over });

test("the hash is stable, and a change to a COPY source or the Containerfile moves it", () => {
  fixture((dir, rel) => {
    const first = hashOf(rel);
    assert.equal(first.length, HASH_LENGTH);
    assert.equal(hashOf(rel), first);
    fs.writeFileSync(path.join(dir, "src", "main.go"), "package main // changed\n");
    const edited = hashOf(rel);
    assert.notEqual(edited, first);
    fs.appendFileSync(path.join(dir, "Containerfile"), "# a comment\n");
    assert.notEqual(hashOf(rel), edited);
  });
});

test("a file no stage of the target copies does not move it", () => {
  fixture((dir, rel) => {
    const first = hashOf(rel);
    fs.writeFileSync(path.join(dir, "unrelated.txt"), "changed\n");
    // `other` is not a stage the `final` target runs, so other.txt is not an input.
    fs.writeFileSync(path.join(dir, "other.txt"), "changed\n");
    assert.equal(hashOf(rel), first);
    // ...but it is an input of the `other` target.
    const before = hashOf(rel, { target: "other" });
    fs.writeFileSync(path.join(dir, "other.txt"), "changed again\n");
    assert.notEqual(hashOf(rel, { target: "other" }), before);
  });
});

test("the target and the build args are part of the address, and a .dockerignore is an input", () => {
  fixture((dir, rel) => {
    const base = hashOf(rel);
    assert.notEqual(hashOf(rel, { target: "build" }), base);
    assert.notEqual(hashOf(rel, { args: { A: "1" } }), base);
    assert.equal(hashOf(rel, { args: { B: "2", A: "1" } }), hashOf(rel, { args: { A: "1", B: "2" } }));
    fs.writeFileSync(path.join(dir, ".dockerignore"), "*.md\n");
    assert.notEqual(hashOf(rel), base);
  });
});

test("an input that no tracked file matches is 'absent' and the hash moves when it appears", () => {
  fixture((dir, rel) => {
    fs.rmSync(path.join(dir, "src"), { recursive: true });
    const absent = hashOf(rel);
    fs.mkdirSync(path.join(dir, "src"));
    fs.writeFileSync(path.join(dir, "src", "main.go"), "package main\n");
    assert.notEqual(hashOf(rel), absent);
  });
});

test("addHashes names the content tag, and the plan form rewrites the plan in place", () => {
  fixture((dir, rel) => {
    const entries = [{ service: "svc", image: "img", ref: "ghcr.io/x/ci-img", tag: "t", build: { context: rel, dockerfile: "Containerfile", target: "final", args: {} } }];
    const [withHash] = addHashes(entries, ROOT, gitOverDisk(ROOT));
    assert.equal(withHash.cas, `cas-${withHash.hash}`);
    const plan = path.join(dir, "plan.json");
    fs.writeFileSync(plan, JSON.stringify(entries));
    const lines = [];
    // The plan form runs the real git over the real tree: the fixture is untracked, so every input is absent,
    // which still yields an address, and the entry gains its fields.
    assert.equal(main(["plan", ROOT, plan], (s) => lines.push(s), () => {}), 0);
    const written = JSON.parse(fs.readFileSync(plan, "utf8"));
    assert.match(written[0].cas, /^cas-[0-9a-f]{24}$/);
    assert.equal(lines[0], `ghcr.io/x/ci-img:${written[0].cas}\n`);
  });
});

test("a real build of the repository hashes, and is not the same address for two images", () => {
  const worker = contentHash({ workspace: ROOT, context: ".", dockerfile: "services/elitea-worker-rust/Containerfile", target: "supervisor" });
  const mainImage = contentHash({ workspace: ROOT, context: ".", dockerfile: "services/elitea-main/Containerfile", target: "e2e" });
  assert.match(worker, /^[0-9a-f]{24}$/);
  assert.notEqual(worker, mainImage);
});
