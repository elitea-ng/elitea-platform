#!/usr/bin/env node
// One Rust version everywhere: cargo, CI and images (PR #1297).
//
// The implementation behind scripts/ci/check-rust-toolchain.sh; see that file
// for the assertions and HOW TO RAISE THE VERSION. Reads only tracked files
// (`git ls-files`), needs no network and no toolchain. Every read that comes
// back empty FAILS: a comparison against nothing must not pass.
//
// Ported from check_rust_toolchain.py (2026-10-20): the same reads, messages
// and exit codes. Its one dependency is the `yaml` package
// (scripts/ci/package.json; `npm ci --prefix scripts/ci`), read the way PyYAML
// read it (scripts/ci/ci-compat.mjs).

import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { isAlias, isMap, isScalar, isSeq } from "yaml";

import { loadYamlNodes } from "./ci-compat.mjs";

// The image index digest of rust:<pin>-trixie (all platforms). Moves with the pin.
export const RUST_IMAGE_DIGEST = "sha256:b1b3c9c0d921d7fa0a6d1f9ec7e4eab87f8c8ec97644c3d791450f131dec813f";
// The minimum supported Rust version of our own crates: the documented floor,
// deliberately below the pin (see the PR #1297 body). Must be <= the pin.
export const MSRV = "1.95";

const MIN_TOOLCHAIN_FILES = 9;
const MIN_CRATES = 30;
const MIN_RELEASE_LITERALS = 6;

// Files that MUST reference the rust image (b). Scanning is exhaustive, so a
// floor would only catch a pattern that stopped matching; this list catches
// that per file instead, and a NEW file that references rust is reported too
// (it must be added here, deliberately, or stop referencing rust).
const RUST_IMAGE_FILES = {
  "libs/rust/workspace-runner/Containerfile": 1,
  "services/elitea-code-runner/Containerfile": 2,
  "services/elitea-deepwiki-engine/Containerfile": 2,
  "services/elitea-doc-extractor/Containerfile": 2,
  "services/elitea-inventory-engine/Containerfile": 2,
  "services/elitea-vector/Containerfile": 2,
  "services/elitea-worker-rust/Containerfile": 2,
};

// Cargo.toml files no Rust toolchain of ours builds as a crate with an MSRV.
const EXEMPT_CRATES = {
  "libs/rust/office-core/fuzz/Cargo.toml": "cargo-fuzz harness built only by nightly cargo-fuzz, never by CI or an image",
  "services/elitea-code-runner/adapters/rust/Cargo.toml": "template compiled inside the sandbox for user code (include_str! in src/rust_profile.rs); not a crate of ours",
  "scripts/runtime/fixtures/code-data-processing/Cargo.toml": "locked acceptance fixture of user code; its manifest is a test input",
};

// (e) Each validator's COMPARISON line, which must carry the pinned release.
// A literal that moves (to a const, a helper, another line) leaves this
// pattern unmatched, so the move fails instead of passing on test data.
const VERSION = String.raw`['"]?(\d+\.\d+\.\d+)['"]?`;
const CHECKER_LINES = {
  "services/elitea-worker-rust/src/sandbox/cargo_dependency_bundle.rs":
    new RegExp(String.raw`\|\|\s*self\.profile\.rust_revision\s*!=\s*` + VERSION),
  "services/elitea-main/internal/infra/storage/sandbox_native_bundle.go":
    new RegExp(String.raw`p\.Profile\.Rust\s*!=\s*` + VERSION),
  "scripts/runtime/rust_compiled_release_manifest.py":
    new RegExp(String.raw`^RUST_RELEASE\s*=\s*` + VERSION + String.raw`\s*\n?$`),
};
// The audit must USE that constant on both of its checks.
const AUDIT_USES = ['profile["rust_revision"] == RUST_RELEASE', 'f"release: {RUST_RELEASE}\\n"'];

// (e) A release literal anywhere: the value next to rust_revision /
// Profile.Rust / RUST_RELEASE in any quoting (or none), on either side of the
// comparison, and the `release: X.Y.Z` / `rustc X.Y.Z` lines of rustc's output.
const V = String.raw`['"]?(?<v>\d+\.\d+\.\d+)['"]?`;
const RELEASE_LITERALS = [
  String.raw`\brust_revision['"]?\]?\s*(?:[:=]|==|!=)\s*` + V,
  String.raw`\bProfile\.Rust\s*(?:==|!=)\s*` + V,
  V + String.raw`\s*(?:==|!=)\s*[\w.]*Profile\.Rust\b`,
  V + String.raw`\s*(?:==|!=)\s*[\w.\[\]'"]*rust_revision`,
  String.raw`\bRUST_RELEASE\s*(?:=|==|!=)\s*` + V,
  String.raw`release: (?<v>\d+\.\d+\.\d+)`,
  String.raw`\brustc (?<v>\d+\.\d+\.\d+)`,
].map((source) => new RegExp(source, "dg"));
const RELEASE_SKIP = /(\.md|\.lock)$|(^|\/)vendor\/|^scripts\/ci\/(check[-_]rust[-_]toolchain|assert-rust-toolchain)/;

// (b) The official rust image after ANY registry or path prefix, with or
// without a tag/digest: rust, rust:1, rust@sha256:..., docker.io/library/rust,
// mirror.gcr.io/library/rust:..., ghcr.io/x/rust:... — but not elitea-rustfs,
// rustfs/rustfs, trust:1 or a stage named rust-runtime.
const RUST_IMAGE = /^(?:[^\s"'@:]+\/)?rust(?:[:@]\S*)?\n?$/;

// ── Python's text model, so line numbers and matches agree ──────────────────
// read_text() decodes UTF-8 (a BOM kept) and turns \r\n and \r into \n;
// splitlines() also breaks on \v \f \x1c-\x1e \x85 \u2028 \u2029.
const SPLITLINES = /\r\n|[\n\r\v\f\x1c\x1d\x1e\x85\u2028\u2029]/;

function splitlines(text) {
  if (text === "") return [];
  const lines = text.split(SPLITLINES);
  if (lines[lines.length - 1] === "" && SPLITLINES.test(text.slice(-2))) lines.pop();
  return lines;
}

function readText(file, { replace = false } = {}) {
  const bytes = fs.readFileSync(file);
  const text = new TextDecoder("utf-8", { fatal: !replace, ignoreBOM: true }).decode(bytes);
  return text.replace(/\r\n?/g, "\n");
}

function isFile(file) {
  try {
    return fs.statSync(file).isFile();
  } catch {
    return false;
  }
}

/** Python's str.strip(chars). */
function stripChars(text, chars) {
  let start = 0;
  let end = text.length;
  while (start < end && chars.includes(text[start])) start += 1;
  while (end > start && chars.includes(text[end - 1])) end -= 1;
  return text.slice(start, end);
}

function words(line) {
  return line.split(/\s+/).filter(Boolean);
}

export const failures = [];

function fail(message) {
  failures.push(message);
  process.stderr.write(`ERROR: ${message}\n`);
}

function print(message = "") {
  process.stdout.write(`${message}\n`);
}

class Exit extends Error {
  constructor(code) {
    super(`exit ${code}`);
    this.code = code;
  }
}

function tracked(...patterns) {
  let out = "";
  try {
    out = execFileSync("git", ["ls-files", "--", ...patterns], {
      encoding: "utf8",
      stdio: ["ignore", "pipe", "pipe"],
      maxBuffer: 1 << 30,
    });
  } catch (error) {
    out = error.stdout ? String(error.stdout) : "";
  }
  return [...new Set(out.split("\n").filter(Boolean))].sort();
}

function versionTuple(value) {
  const parts = value.split(".").map((p) => {
    if (!/^\s*[+-]?\d+\s*$/.test(p)) throw new Error(`invalid literal for int() with base 10: '${p}'`);
    return parseInt(p, 10);
  });
  while (parts.length < 3) parts.push(0);
  return parts;
}

function compareTuples(a, b) {
  for (let i = 0; i < Math.min(a.length, b.length); i += 1) {
    if (a[i] !== b[i]) return a[i] < b[i] ? -1 : 1;
  }
  return a.length - b.length;
}

// ── (a) toolchain files ───────────────────────────────────────────────────────
function channelOf(file) {
  const match = /^\s*channel\s*=\s*"([^"]*)"\s*$/m.exec(readText(file));
  return match ? match[1] : "";
}

function checkToolchains() {
  if (!isFile("rust-toolchain.toml")) {
    process.stderr.write("ERROR: rust-toolchain.toml is missing at the repository root; there is no pin\n");
    throw new Exit(1);
  }
  const pin = channelOf("rust-toolchain.toml");
  if (!/^\d+\.\d+\.\d+$/.test(pin)) {
    process.stderr.write(`ERROR: read no exact 'channel = "X.Y.Z"' from rust-toolchain.toml (got '${pin}').\n`);
    throw new Exit(1);
  }
  print(`pin: rust ${pin} (rust-toolchain.toml)`);
  const files = tracked("rust-toolchain.toml", "*/rust-toolchain.toml", "rust-toolchain", "*/rust-toolchain");
  if (files.length < MIN_TOOLCHAIN_FILES) {
    fail(`found ${files.length} rust-toolchain file(s), expected at least ${MIN_TOOLCHAIN_FILES}; the pattern stopped matching`);
  }
  for (const file of files) {
    if (!file.endsWith(".toml")) {
      fail(`${file}: a legacy rust-toolchain file without .toml; use rust-toolchain.toml`);
      continue;
    }
    const channel = channelOf(file);
    if (!channel) fail(`${file}: read no 'channel = "..."'`);
    else if (channel !== pin) fail(`${file}: channel ${channel}, the pin is ${pin}`);
    else print(`  ${file}: ${channel}`);
  }
  return pin;
}

// ── (b) images ────────────────────────────────────────────────────────────────
/** Dockerfile lines with `\` continuations joined; comments dropped. */
export function logicalLines(text) {
  const out = [];
  let pending = "";
  let start = 0;
  splitlines(text).forEach((raw, index) => {
    const number = index + 1;
    const line = raw.replace(/\s+$/, "");
    if (!pending && /^\s*(#|\/\/)/.test(line)) return;
    if (!pending) start = number;
    if (line.endsWith("\\")) {
      pending += line.slice(0, -1) + " ";
      return;
    }
    out.push([start, pending + line]);
    pending = "";
  });
  if (pending) out.push([start, pending]);
  return out;
}

/** Expand ${NAME}, ${NAME:-default} and $NAME from ARG defaults. */
export function substitute(value, args) {
  return value.replace(
    /\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)/g,
    (whole, braced, fallback, bare) => {
      const name = braced !== undefined ? braced : bare;
      if (Object.prototype.hasOwnProperty.call(args, name) && args[name]) return args[name];
      return fallback !== undefined ? fallback : whole;
    },
  );
}

/** (line, image operand) for FROM, --from=, --mount=...,from= and bake contexts. */
export function dockerfileOperands(text) {
  const lines = logicalLines(text);
  const stages = new Set();
  for (const [, line] of lines) {
    const match = /^\s*FROM\s+(?:--\S+\s+)*\S+\s+AS\s+(\S+)/i.exec(line);
    if (match) stages.add(match[1].toLowerCase());
  }
  const args = {};
  const found = [];
  for (const [number, line] of lines) {
    const arg = /^\s*ARG\s+([A-Za-z_][A-Za-z0-9_]*)(?:=(\S*))?/i.exec(line);
    if (arg) {
      const value = stripChars(arg[2] || "", "\"'");
      args[arg[1]] = substitute(value, args);
      continue;
    }
    const ws = words(line);
    if (ws.length && ws[0].toUpperCase() === "FROM") {
      const rest = ws.slice(1).filter((w) => !w.startsWith("--"));
      if (rest.length) found.push([number, substitute(rest[0], args)]);
    }
    for (const word of ws) {
      for (const match of word.matchAll(/(?:^|,|--)from=([^,\s]+)/g)) {
        found.push([number, substitute(stripChars(match[1], "\"'"), args)]);
      }
    }
    for (const match of line.matchAll(/docker-image:\/\/([^"\s]+)/g)) {
      found.push([number, substitute(match[1], args)]);
    }
  }
  return found
    .filter(([, op]) => !stages.has(stripChars(op, "\"'").toLowerCase()))
    .map(([n, op]) => [n, stripChars(op, "\"'")]);
}

// PyYAML counts a line at \n, \x85, \u2028 and \u2029 (\r is gone by then).
function lineAt(text, offset) {
  let line = 1;
  for (let i = 0; i < offset && i < text.length; i += 1) {
    const ch = text[i];
    if (ch === "\n" || ch === "\x85" || ch === "\u2028" || ch === "\u2029") line += 1;
  }
  return line;
}

function scalarText(node) {
  if (typeof node.source === "string") return node.source;
  return node.value === null || node.value === undefined ? "" : String(node.value);
}

/**
 * (line, value) for every `image:` and `container:` key in a YAML file.
 *
 * Every document of a multi-document file is walked. A file that does not
 * parse (a Helm template) is read line by line instead, so it is never
 * skipped in silence.
 */
export function yamlOperands(text) {
  const found = [];
  let roots;
  try {
    roots = loadYamlNodes(text);
  } catch {
    splitlines(text).forEach((line, index) => {
      const match = /^\s*(?:-\s+)?(?:image|container)\s*:\s*['"]?([^'"\s#]+)/.exec(line);
      if (match) found.push([index + 1, match[1]]);
    });
    return found;
  }

  const walk = (doc, node) => {
    if (isAlias(node)) node = node.resolve(doc);
    if (!node) return;
    if (isMap(node)) {
      for (const pair of node.items) {
        let key = pair.key;
        let value = pair.value;
        if (isAlias(key)) key = key.resolve(doc);
        if (isAlias(value)) value = value.resolve(doc);
        if (isScalar(key) && ["image", "container"].includes(scalarText(key)) && value && isScalar(value)) {
          found.push([lineAt(text, value.range[0]), scalarText(value)]);
        }
        walk(doc, value);
      }
    } else if (isSeq(node)) {
      for (const item of node.items) walk(doc, item);
    }
  };

  for (const doc of roots) {
    if (doc.contents) walk(doc, doc.contents);
  }
  return found;
}

function checkImages(pin) {
  const expected = `rust:${pin}-trixie@${RUST_IMAGE_DIGEST}`;
  const perFile = {};
  for (const file of tracked("*Containerfile*", "*Dockerfile*", "*.hcl", "*.yml", "*.yaml")) {
    const text = readText(file, { replace: true });
    const operands = file.endsWith(".yml") || file.endsWith(".yaml") ? yamlOperands(text) : dockerfileOperands(text);
    for (const [number, image] of operands) {
      if (!RUST_IMAGE.test(image)) continue;
      perFile[file] = (perFile[file] || 0) + 1;
      if (image !== expected) {
        fail(`${file}:${number}: ${image}, expected exactly ${expected} (no registry prefix; tag and digest both)`);
      } else {
        print(`  ${file}:${number}: rust:${pin}-trixie@…${RUST_IMAGE_DIGEST.slice(-8)}`);
      }
    }
  }
  for (const file of Object.keys(perFile).sort()) {
    if (!(file in RUST_IMAGE_FILES)) {
      fail(`${file}: references the rust image (${perFile[file]}x) but is not in RUST_IMAGE_FILES; add it there deliberately`);
    }
  }
  for (const file of Object.keys(RUST_IMAGE_FILES).sort()) {
    const count = perFile[file] || 0;
    const minimum = RUST_IMAGE_FILES[file];
    if (count < minimum) {
      fail(`${file}: found ${count} rust image reference(s), expected at least ${minimum}; the pattern stopped matching or the file moved`);
    }
  }
}

// ── (c) MSRV ──────────────────────────────────────────────────────────────────
function checkMsrv(pin) {
  if (!/^\d+\.\d+(\.\d+)?$/.test(MSRV)) {
    fail(`MSRV '${MSRV}' is not a version`);
    return;
  }
  if (compareTuples(versionTuple(MSRV), versionTuple(pin)) > 0) {
    fail(`MSRV ${MSRV} is above the pin ${pin}; the pinned compiler could not build our crates`);
  }
  print(`MSRV: ${MSRV} (floor of our crates; vendored crates keep their own, <= ${pin})`);

  const valueOf = (file) => {
    const match = /^\s*rust-version\s*=\s*"([^"]*)"\s*$/m.exec(readText(file));
    return match ? match[1] : "";
  };

  const workspace = isFile("libs/rust/Cargo.toml") ? valueOf("libs/rust/Cargo.toml") : "";
  let crates = 0;
  for (const file of tracked("*Cargo.toml")) {
    if (!(file.endsWith("/Cargo.toml") || file === "Cargo.toml" || file.endsWith(".Cargo.toml"))) continue;
    const text = readText(file);
    if (!/^\[package\]/m.test(text)) continue;
    crates += 1;
    if (file in EXEMPT_CRATES) {
      print(`  ${file}: exempt`);
      continue;
    }
    if (file.includes("/vendor/")) {
      const value = valueOf(file);
      if (!value) fail(`${file}: vendored crate with no readable rust-version`);
      else if (compareTuples(versionTuple(value), versionTuple(pin)) > 0) {
        fail(`${file}: vendored crate needs rust ${value}, above the pin ${pin}`);
      } else print(`  ${file}: ${value} (vendored, <= ${pin})`);
      continue;
    }
    let value;
    let how;
    if (/^\s*rust-version\.workspace\s*=\s*true/m.test(text)) {
      if (!file.startsWith("libs/rust/")) {
        fail(`${file}: inherits rust-version from a workspace this check does not read`);
        continue;
      }
      [value, how] = [workspace, "inherited from libs/rust"];
    } else {
      [value, how] = [valueOf(file), "stated"];
    }
    if (!value) fail(`${file}: no readable rust-version (state "${MSRV}" or inherit the workspace value)`);
    else if (value !== MSRV) fail(`${file}: rust-version ${value} (${how}), the MSRV is ${MSRV}`);
    else print(`  ${file}: ${value} (${how})`);
  }
  if (crates < MIN_CRATES) {
    fail(`found ${crates} crate manifest(s), expected at least ${MIN_CRATES}; the pattern stopped matching`);
  }
  for (const file of Object.keys(EXEMPT_CRATES)) {
    if (!isFile(file)) fail(`MSRV exemption ${file} names no file; remove it`);
  }
}

// ── (e) release literals ──────────────────────────────────────────────────────
function checkReleaseLiterals(pin) {
  let count = 0;
  for (const file of tracked("*")) {
    if (RELEASE_SKIP.test(file)) continue;
    let text;
    try {
      text = readText(file);
    } catch (error) {
      if (error instanceof TypeError || ["EISDIR", "ENOENT"].includes(error.code)) continue;
      throw error;
    }
    splitlines(text).forEach((line, index) => {
      const number = index + 1;
      const seen = new Set();
      for (const pattern of RELEASE_LITERALS) {
        for (const match of line.matchAll(pattern)) {
          const start = match.indices.groups.v[0];
          if (seen.has(start)) continue;
          seen.add(start);
          count += 1;
          const value = match.groups.v;
          if (value !== pin) {
            fail(`${file}:${number}: toolchain release ${value}, the pin is ${pin} (a native bundle and its validators would disagree with the compiler)`);
          } else {
            print(`  ${file}:${number}: release ${value}`);
          }
        }
      }
    });
  }
  if (count < MIN_RELEASE_LITERALS) {
    fail(`found ${count} toolchain release literal(s), expected at least ${MIN_RELEASE_LITERALS}; one moved or the pattern stopped matching`);
  }
  for (const [file, pattern] of Object.entries(CHECKER_LINES)) {
    const text = isFile(file) ? readText(file) : "";
    const hits = [];
    splitlines(text).forEach((line, index) => {
      const match = pattern.exec(line);
      if (match) hits.push([index + 1, match]);
    });
    if (!hits.length) {
      fail(`${file}: carries no toolchain release literal on its comparison line; the validator moved, so this check cannot see what it compares`);
    }
    for (const [number, match] of hits) {
      if (match[1] !== pin) fail(`${file}:${number}: the comparison checks release ${match[1]}, the pin is ${pin}`);
    }
  }
  const audit = "scripts/runtime/rust_compiled_release_manifest.py";
  if (isFile(audit)) {
    const source = readText(audit);
    for (const use of AUDIT_USES) {
      if (!source.includes(use)) fail(`${audit}: no longer checks \`${use}\`; the RUST_RELEASE constant is not what it compares`);
    }
  }
  return count;
}

export function main() {
  const root = process.env.RUST_TOOLCHAIN_ROOT || path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");
  process.chdir(root);
  if (!tracked().length) {
    process.stderr.write(`ERROR: ${root} is not a git work tree with tracked files; there is nothing to check\n`);
    return 1;
  }
  let pin;
  try {
    pin = checkToolchains();
  } catch (error) {
    if (error instanceof Exit) return error.code;
    throw error;
  }
  checkImages(pin);
  checkMsrv(pin);
  const literals = checkReleaseLiterals(pin);
  if (failures.length) {
    process.stderr.write(
      `\n${failures.length} Rust toolchain disagreement(s). See 'HOW TO RAISE THE VERSION' in ` +
        "scripts/ci/check-rust-toolchain.sh.\n",
    );
    return 1;
  }
  print(
    `One Rust everywhere: rust ${pin}, image digest …${RUST_IMAGE_DIGEST.slice(-12)}, MSRV ${MSRV}, ` +
      `${literals} release literal(s).`,
  );
  return 0;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.exitCode = main();
}
