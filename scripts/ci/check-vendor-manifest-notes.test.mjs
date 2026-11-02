// Scenarios for check-vendor-manifest-notes.mjs.

import assert from "node:assert/strict";
import { test } from "node:test";

import { addedLines, check, README } from "./check-vendor-manifest-notes.mjs";

test("a diff that touches no vendored manifest passes", () => {
  const r = check(["libs/rust/Cargo.toml", "libs/rust/vendor/calamine/src/utils.rs"], []);
  assert.deepEqual(r, { crates: [], problems: [] });
});

test("the #1547 shape fails: a vendored manifest with no note", () => {
  const { problems } = check(["libs/rust/vendor/calamine/Cargo.toml", "libs/rust/Cargo.lock"], []);
  assert.equal(problems.length, 1);
  assert.match(problems[0], /libs\/rust\/vendor\/calamine\/Cargo\.toml changed/);
});

test("Cargo.toml.orig counts as the manifest", () => {
  assert.equal(check(["libs/rust/vendor/zavora-xlsx/Cargo.toml.orig"], []).problems.length, 1);
});

test("a README line that names the crate passes", () => {
  const r = check([`libs/rust/vendor/calamine/Cargo.toml`, README], ["| `Cargo.toml` | `calamine` moves quick-xml |"]);
  assert.deepEqual(r.problems, []);
  assert.deepEqual(r.crates, ["calamine"]);
});

test("a note naming a longer crate does not cover a shorter prefix", () => {
  const { problems } = check(["libs/rust/vendor/zavora-slide/Cargo.toml"], ["`zavora-slide-opc` moves"]);
  assert.equal(problems.length, 1);
});

test("each changed crate needs its own note", () => {
  const { crates, problems } = check(
    ["libs/rust/vendor/calamine/Cargo.toml", "libs/rust/vendor/zavora-xlsx/Cargo.toml"],
    ["calamine: re-vendored"],
  );
  assert.deepEqual(crates, ["calamine", "zavora-xlsx"]);
  assert.equal(problems.length, 1);
  assert.match(problems[0], /zavora-xlsx/);
});

test("a nested manifest inside a vendored crate is not the crate's manifest", () => {
  assert.deepEqual(check(["libs/rust/vendor/leiden-rs/fuzz/Cargo.toml"], []).problems, []);
});

test("addedLines keeps added lines only, not the file header or removals", () => {
  const diff = ["--- a/x", "+++ b/x", "@@ -1 +1 @@", "-old calamine", "+new calamine", " context"].join("\n");
  assert.deepEqual(addedLines(diff), ["new calamine"]);
});
