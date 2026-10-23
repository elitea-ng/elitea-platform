// What ./ci-compat.mjs and ./py-compat.mjs promise: YAML read as PyYAML's
// safe_load read it, and Python's repr/splitlines/fnmatchcase. Each expected
// value below is what Python 3.12 + PyYAML 6.0.3 returned for the same input
// when the checks were ported (2026-10-20).

import assert from "node:assert/strict";
import { test } from "node:test";

import { loadYaml, loadYamlNodes, pyyamlTags } from "./ci-compat.mjs";
import { fnmatchcase, pyRepr, pyStr, splitlines } from "./py-compat.mjs";

test("an unquoted on key is PyYAML's True, a quoted one stays a string", () => {
  assert.deepEqual(loadYaml("on:\n  push: {}\n"), { true: { push: {} } });
  assert.deepEqual(loadYaml('"on":\n  push: {}\n'), { on: { push: {} } });
});

test("booleans and floats resolve as PyYAML resolves them, not as YAML 1.2 or the yaml package's 1.1", () => {
  assert.deepEqual(loadYaml("a: yes\nb: No\nc: off\nd: ON\ne: y\nf: n\ng: true\n"), {
    a: true, b: false, c: false, d: true, e: "y", f: "n", g: true,
  });
  assert.deepEqual(loadYaml("a: 1e5\nb: 1.0e5\nc: 1.0e+5\nd: 1.5\ne: 010\nf: 0x1f\n"), {
    a: "1e5", b: "1.0e5", c: 100000, d: 1.5, e: 8, f: 31,
  });
  // `working-directory: .` is a path, not NaN; `09` and `08:30` are strings.
  assert.deepEqual(loadYaml("a: .\nb: -.5\nc: .5\nd: 09\ne: 08:30\nf: 1:30\ng: 010\n"), {
    a: ".", b: "-.5", c: 0.5, d: "09", e: "08:30", f: 90, g: 8,
  });
});

// One scenario per form where the yaml package's 1.1 resolvers and PyYAML's
// disagree. Each value is PyYAML 6.0.3's safe_load of `k: <form>`.
const pyyaml = (form) => loadYaml(`k: ${form}`).k;

test("a y/n/Y/N is a string, not a boolean", () => {
  for (const form of ["y", "n", "Y", "N"]) assert.equal(pyyaml(form), form);
  assert.equal(pyyaml("Yes"), true);
  assert.equal(pyyaml("OFF"), false);
});

test("an exponent without a dot, or without a sign, is a string", () => {
  for (const form of ["1e5", "1E5", ".5e5", "1.0e5"]) assert.equal(pyyaml(form), form);
  assert.equal(pyyaml("1.0e+5"), 100000);
  assert.equal(pyyaml("1.0E-5"), 1e-5);
  assert.equal(pyyaml(".5e+3"), 500);
});

test("a float needs a digit before the dot unless the dot leads one", () => {
  for (const form of [".", "-.5", "+.5"]) assert.equal(pyyaml(form), form);
  assert.equal(pyyaml(".5"), 0.5);
  assert.equal(pyyaml("1."), 1);
  assert.equal(pyyaml("-1."), -1);
  assert.equal(pyyaml("1_000.5"), 1000.5);
  assert.equal(pyyaml(".inf"), Infinity);
  assert.equal(pyyaml("-.Inf"), -Infinity);
  assert.ok(Number.isNaN(pyyaml(".NaN")));
  assert.equal(pyyaml("inf"), "inf");
});

test("a leading zero is octal, or a string when it is not octal", () => {
  for (const form of ["09", "08"]) assert.equal(pyyaml(form), form);
  assert.equal(pyyaml("010"), 8);
  assert.equal(pyyaml("00"), 0);
  assert.equal(pyyaml("0_7"), 7);
  assert.equal(pyyaml("-0"), 0);
  assert.equal(pyyaml("1_000"), 1000);
  assert.equal(pyyaml("-0x1F"), -31);
  assert.equal(pyyaml("-0b1_0"), -2);
  for (const form of ["0x", "0b2"]) assert.equal(pyyaml(form), form);
});

test("a sexagesimal int starts 1-9; a sexagesimal float may start with 0", () => {
  for (const form of ["08:30", "0:30", "1:60"]) assert.equal(pyyaml(form), form);
  assert.equal(pyyaml("1:30"), 90);
  assert.equal(pyyaml("-1:30"), -90);
  assert.equal(pyyaml("190:20:30"), 685230);
  assert.equal(pyyaml("1:30.5"), 90.5);
  assert.equal(pyyaml("08:30.5"), 510.5);
});

test("a yaml package whose 1.1 number or bool tags change fails the read, not skips it", () => {
  const INT = "tag:yaml.org,2002:int";
  const shipped = [
    { tag: "tag:yaml.org,2002:bool", test: /^y$/ },
    { tag: "tag:yaml.org,2002:bool", test: /^n$/ },
    ...["", "BIN", "HEX", "OCT", "TIME"].map((format) => ({ tag: INT, format: format || undefined, test: /x/ })),
    ...["", "", "EXP", "TIME"].map((format) => ({ tag: "tag:yaml.org,2002:float", format: format || undefined, test: /x/ })),
  ];
  const replaced = pyyamlTags()(shipped);
  assert.equal(replaced.length, 4);
  assert.ok(replaced.every((tag) => !["/x/", "/^y$/", "/^n$/"].includes(String(tag.test))));
  assert.throws(() => pyyamlTags()([...shipped, { tag: INT, format: "DURATION", test: /x/ }]), /int tags with formats/);
  assert.throws(() => pyyamlTags()(shipped.filter((tag) => tag.format !== "EXP")), /float tags with formats/);
});

test("merge keys merge, a repeated key keeps the last value, an empty document is null", () => {
  assert.deepEqual(loadYaml("base: &b {x: 1, y: 1}\nm:\n  <<: *b\n  y: 2\n"), { base: { x: 1, y: 1 }, m: { x: 1, y: 2 } });
  assert.deepEqual(loadYaml("k: 1\nk: 2\n"), { k: 2 });
  assert.equal(loadYaml(""), null);
  assert.equal(loadYaml("# only a comment\n"), null);
});

test("an unknown tag reads as untagged, a scalar as its string (compose's !reset)", () => {
  assert.deepEqual(loadYaml("a: !reset null\nb: !override [1]\nc: !x {k: on}\nd: !reset 5\n"), {
    a: "null", b: [1], c: { k: true }, d: "5",
  });
});

test("a parse error throws instead of reading as an empty document", () => {
  assert.throws(() => loadYaml("a: [1, 2\n"));
  assert.throws(() => loadYamlNodes("a: [1, 2\n"));
  assert.equal(loadYamlNodes("a: 1\n---\nb: 2\n").length, 2);
});

test("pyRepr and pyStr print what Python printed", () => {
  assert.equal(pyRepr(["a'b", "x", null, true, false, { k: 1 }, 1.5, 2]), `["a'b", 'x', None, True, False, {'k': 1}, 1.5, 2]`);
  assert.equal(pyRepr("tab\there\\"), "'tab\\there\\\\'");
  assert.equal(pyRepr(`both ' and "`), `'both \\' and "'`);
  assert.equal(pyStr("plain"), "plain");
  assert.equal(pyStr(null), "None");
  assert.equal(pyStr(false), "False");
});

test("splitlines splits where str.splitlines splits", () => {
  assert.deepEqual(splitlines("a\nb\n"), ["a", "b"]);
  assert.deepEqual(splitlines("a\r\nb"), ["a", "b"]);
  assert.deepEqual(splitlines("a\fb\n\n"), ["a", "b", ""]);
  assert.deepEqual(splitlines(""), []);
  assert.deepEqual(splitlines("\n"), [""]);
  assert.deepEqual(splitlines("x\r"), ["x"]);
});

test("fnmatchcase matches what fnmatch.fnmatchcase matches", () => {
  const cases = [
    ["scripts/ci/a.sh", "scripts/ci/*.sh", true],
    ["scripts/ci/x/a.sh", "scripts/ci/*.sh", true],
    ["scripts/ci/testdata/trivy-a.json", "scripts/ci/testdata/trivy-*.json", true],
    ["a.b", "a?b", true],
    ["ab", "a[!x]", true],
    ["ax", "a[!x]", false],
    ["a]", "a[]]", true],
    ["a[", "a[", true],
    ["A.sh", "*.SH", false],
    ["a.sh.bak", "*.sh", false],
  ];
  for (const [name, pattern, expected] of cases) assert.equal(fnmatchcase(name, pattern), expected, `${name} ~ ${pattern}`);
});
