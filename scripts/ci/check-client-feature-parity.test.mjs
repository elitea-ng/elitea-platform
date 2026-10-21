import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

import { MINIMUM_ROWS, TABLE_PATH, cells, checkParityTable, isStatus } from "./check-client-feature-parity.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

const row = (name, web = "shipped", desktop = "shipped", mobile = "shipped") => `| ${name} | ${web} | ${desktop} | ${mobile} | |`;
const doc = (rows) =>
  ["# T", "", "## Features", "", "| Feature | Web | Desktop | Mobile | Notes |", "| --- | --- | --- | --- | --- |", ...rows, "", "## After", "| not | part |"].join("\n");
const filler = (n) => Array.from({ length: n }, (_, i) => row(`Feature ${i}`));

describe("isStatus", () => {
  for (const ok of ["shipped", "planned (#12)", "n-a: no home screen"]) {
    it(`accepts ${ok}`, () => assert.ok(isStatus(ok)));
  }
  for (const bad of ["", "planned", "planned (#)", "planned (#0)", "n-a", "n-a:", "N/A", "yes", "shipped (#1)"]) {
    it(`refuses "${bad}"`, () => assert.ok(!isStatus(bad)));
  }
});

describe("cells", () => {
  it("splits on the outer and inner pipes", () => {
    assert.deepEqual(cells("| a | b |c|"), ["a", "b", "c"]);
    assert.deepEqual(cells("a | b"), ["a", "b"]);
  });

  it("keeps an escaped pipe in its cell", () => {
    assert.deepEqual(cells("| a \\| b | c |"), ["a \\| b", "c"]);
  });

  it("keeps a pipe inside a code span in its cell", () => {
    assert.deepEqual(cells("| `a|b` | ``x | `y` | z`` | c |"), ["`a|b`", "``x | `y` | z``", "c"]);
  });

  it("treats an unclosed backtick as text, so later pipes still split", () => {
    assert.deepEqual(cells("| a ` b | c |"), ["a ` b", "c"]);
  });

  it("keeps an escaped pipe at the end of the line as text", () => {
    assert.deepEqual(cells("| a | b \\|"), ["a", "b \\|"]);
  });
});

describe("checkParityTable", () => {
  it("does not count a pipe in a code span or an escaped pipe as a column", () => {
    const result = checkParityTable(
      doc([...filler(MINIMUM_ROWS), "| Pipes `a|b` | shipped | n-a: no `x|y` here | shipped | one \\| two |"]),
    );
    assert.deepEqual(result.failures, []);
  });

  it("passes a well-formed table", () => {
    const result = checkParityTable(doc(filler(MINIMUM_ROWS)));
    assert.deepEqual(result.failures, []);
    assert.equal(result.rows, MINIMUM_ROWS);
  });

  it("names a planned row without an issue and an n-a row without a reason", () => {
    const result = checkParityTable(doc([...filler(MINIMUM_ROWS), row("Fork", "shipped", "planned", "n-a")]));
    assert.equal(result.ok, false);
    assert.equal(result.failures.length, 2);
    assert.match(result.failures[0], /Desktop .*planned must name its issue/);
    assert.match(result.failures[1], /Mobile .*n-a must give a reason/);
  });

  it("fails a row with a missing platform cell", () => {
    const result = checkParityTable(doc([...filler(MINIMUM_ROWS), "| Short | shipped | shipped |"]));
    assert.equal(result.ok, false);
    assert.match(result.failures.join("\n"), /3 cells, expected 5/);
  });

  it("fails a repeated feature", () => {
    const result = checkParityTable(doc([...filler(MINIMUM_ROWS), row("Feature 0")]));
    assert.match(result.failures.join("\n"), /"Feature 0" repeats/);
  });

  it("fails a wrong header", () => {
    const text = doc(filler(MINIMUM_ROWS)).replace("| Feature | Web | Desktop | Mobile | Notes |", "| Feature | Web | Mobile | Desktop | Notes |");
    assert.match(checkParityTable(text).failures.join("\n"), /the header must be/);
  });

  it("fails an empty or missing table instead of passing it", () => {
    assert.equal(checkParityTable(doc([])).ok, false);
    assert.equal(checkParityTable("# no section").ok, false);
    assert.equal(checkParityTable("## Features\n\nprose only\n").ok, false);
  });

  it("the committed table passes", () => {
    const result = checkParityTable(fs.readFileSync(path.join(ROOT, TABLE_PATH), "utf8"));
    assert.deepEqual(result.failures, []);
  });
});
