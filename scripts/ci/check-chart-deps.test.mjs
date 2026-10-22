// Scenarios for check-chart-deps.mjs: `helm dependency list` tables as Helm
// prints them (tab-separated, padded columns).

import assert from "node:assert/strict";
import { test } from "node:test";

import { check } from "./check-chart-deps.mjs";

const HEADER = "NAME                  \tVERSION\tREPOSITORY                                \tSTATUS  ";
const row = (name, status) => `${name.padEnd(22)}\t0.1.0  \thttps://example.invalid/charts\t${status.padEnd(8)}`;
const table = (...rows) => [HEADER, ...rows, ""].join("\n");

test("ok and unpacked rows pass", () => {
  const { rows, problems } = check(table(row("nats", "unpacked"), row("elitea-sandbox-runtime", "ok")));
  assert.equal(rows.length, 2);
  assert.deepEqual(problems, []);
});

test("each known bad status fails, with the dependency named", () => {
  for (const s of ["missing", "wrong version", "misplaced"]) {
    const { problems } = check(table(row("nats", "unpacked"), row("elitea-sandbox-runtime", s)));
    assert.equal(problems.length, 1, s);
    assert.match(problems[0], new RegExp(`elitea-sandbox-runtime .*'${s}'`));
  }
});

test("a status this check has never seen fails", () => {
  assert.match(check(table(row("nats", "stale"))).problems.join("\n"), /'stale'/);
});

test("a chart with no dependencies prints no table and passes", () => {
  assert.deepEqual(check("WARNING: no dependencies at deploy/helm/x/charts\n").problems, []);
});

test("a header with no rows fails", () => {
  assert.match(check(table()).problems.join("\n"), /no rows/);
});
