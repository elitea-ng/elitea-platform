#!/usr/bin/env node
// Fail when a Rust worker `#[ignore]` test is run by nothing.
//
// An ignored test never runs under a plain `cargo test`. It runs only when a
// `cargo test ... -- --ignored` invocation names it, and nothing reports one that
// no invocation names: it simply never runs, and reads like a passing test.
// Thirty-nine worker tests sat in that state (they needed fixtures no workflow
// provisioned). This check makes that state a CI failure.
//
// Every ignored test the worker compiles must be either
//
//   * selected by a `cargo test ... -- --ignored` invocation that CI will really
//     execute and whose failure fails the job, or by a
//     `scripts/runtime/test_sandbox_ledger.py --test-filter <substring>` run, or
//   * matched by an allowlist entry `<name prefix>  # <where it runs instead>`.
//
// "Really execute" is read from the workflow YAML, not from its text:
//
//   * only `jobs.<id>.steps[].run` scripts count; YAML comments, step names and
//     shell comment lines inside a script select nothing;
//   * a step (or job) whose `if:` is anything but unconditional is not credited
//     (`if: false` never runs; `if: failure()` runs only when something else
//     already failed); `success()`, `always()` and `!cancelled()` are;
//   * a step (or job) with `continue-on-error: true` is not credited, and
//     neither is a command whose line tolerates its failure (`cargo test ... ||
//     true`): a red test there turns nothing red;
//   * cargo's target selection is honoured: `--lib` credits only library unit
//     tests, `--test <name>` only that integration test, `--bins`/`--bin` only
//     binaries; no selector, `--tests` or `--all-targets` credits every target.
//     The test list therefore records which target each name came from.
//
// Inputs:
//   --list       `cargo test ... -- --list --ignored 2>&1` output: the cargo
//                `Running <target>` headers (stderr) and the `name: test` lines
//   --workflow   the workflow whose invocations count (repeatable)
//   --allowlist  the allowlist file
//   --sources    the crate root; its `#[ignore` attributes are counted and must
//                equal the listed count, so an empty or truncated list cannot
//                pass as "nothing to check".
//
// Ported from check_worker_ignored_tests.py (2026-10-20): the same arguments,
// messages and exit codes. The shell lexer is Python's shlex (posix,
// punctuation_chars "|&;<>", whitespace_split, `#` comments), ported state for
// state, so a line splits into the same words. The `yaml` package
// (scripts/ci/package.json) reads the workflow the way PyYAML did.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";

import { loadYaml, pyRepr, pyStr, splitlines } from "./ci-compat.mjs";

/** The Python script's `raise SystemExit("message")`: message to stderr, exit 1. */
export class SystemExit extends Error {
  constructor(message, code = 1) {
    super(message);
    this.code = code;
  }
}

// cargo flags that consume the next token, so it is not a test-name filter.
const CARGO_VALUE_FLAGS = new Set([
  "--features", "-F", "-j", "--jobs",
  "--bench", "--example", "--target", "--target-dir",
  "--profile", "--color", "--message-format", "--config", "-Z", "--exclude",
]);
// libtest flags that consume the next token.
const LIBTEST_VALUE_FLAGS = new Set(["--skip", "--test-threads", "--format", "--color", "-Z", "--logfile"]);

const IGNORE_ATTRIBUTE = /^\s*#\[ignore\b/;
// cargo prints one of these (to stderr) before each test binary's list.
const RUNNING = /^\s*Running (?:unittests )?(\S+)/;
// Step and job conditions under which a step runs whenever the job does.
const UNCONDITIONAL = new Set(["success()", "always()", "!cancelled()"]);
// scripts/runtime/test_sandbox_ledger.py provisions Docker fixtures, then runs
// `cargo test ... --lib <--test-filter> -- --ignored` itself.
const LEDGER_WRAPPER = /test_sandbox_ledger\.py\b.*--test-filter[ =](\S+)/;

/** Path.read_text(): UTF-8, a BOM kept, universal newlines. */
function readText(file) {
  const bytes = fs.readFileSync(file);
  return new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes).replace(/\r\n?/g, "\n");
}

/** PurePosixPath(p).parts, without a leading "/". */
function pathParts(p) {
  return p.split("/").filter((part) => part !== "" && part !== ".");
}

/** PurePosixPath(p).name. */
function pathName(p) {
  const parts = pathParts(p);
  return parts.length ? parts[parts.length - 1] : "";
}

/** PurePosixPath(p).stem. */
function pathStem(p) {
  const name = pathName(p);
  const dot = name.lastIndexOf(".");
  return dot > 0 && dot < name.length - 1 ? name.slice(0, dot) : name;
}

export class Test {
  constructor(name, target) {
    this.name = name;
    this.target = target; // "lib", "bin:<name>" or "test:<name>"
  }

  get key() {
    return `${this.target}\0${this.name}`;
  }
}

/** Map a cargo `Running` path to a target id. */
export function targetOf(value) {
  const p = value.trim();
  if (p === "src/lib.rs") return "lib";
  if (p.startsWith("src/") && p.endsWith(".rs")) return "bin:" + pathStem(p);
  if (p.startsWith("tests/") && p.endsWith(".rs")) return "test:" + pathStem(p);
  return "other:" + p;
}

export class Invocation {
  constructor({ filters = [], skips = [], exact = false, targets = null, source = "" } = {}) {
    this.filters = filters;
    this.skips = skips;
    this.exact = exact;
    // null = every target; otherwise the set of target ids it runs.
    this.targets = targets;
    this.source = source;
  }

  runsTarget(target) {
    if (this.targets === null) return true;
    if (this.targets.has(target)) return true;
    return (this.targets.has("bin:*") && target.startsWith("bin:")) ||
      (this.targets.has("test:*") && target.startsWith("test:"));
  }

  selects(test) {
    if (!this.runsTarget(test.target)) return false;
    const name = test.name;
    if (this.skips.some((skip) => name.includes(skip))) return false;
    if (!this.filters.length) return true;
    if (this.exact) return this.filters.includes(name);
    return this.filters.some((f) => name.includes(f));
  }
}

class Uncredited {
  constructor(command, reason) {
    this.command = command;
    this.reason = reason;
  }
}

/** Join shell `\` continuations; drop shell comment lines. */
export function logicalLines(text) {
  const lines = [];
  let pending = "";
  for (const raw of splitlines(text)) {
    const stripped = raw.replace(/\s+$/, "");
    if (!pending && stripped.replace(/^\s+/, "").startsWith("#")) continue;
    if (stripped.endsWith("\\")) {
      pending += stripped.slice(0, -1) + " ";
      continue;
    }
    lines.push(pending + stripped);
    pending = "";
  }
  if (pending) lines.push(pending);
  return lines;
}

/** Normalise an `if:` value; '' means no condition. */
export function condition(value) {
  if (value === null || value === undefined) return "";
  let text = pyStr(value).trim();
  if (text.startsWith("${{") && text.endsWith("}}")) text = text.slice(3, -2).trim();
  return text.split(" ").join("");
}

// Python's `x in (None, False, "false")`: 0 == False there too.
function isOff(value) {
  return value === null || value === undefined || value === false || value === 0 || value === "false";
}

/** null when the step's commands count; otherwise why they don't. */
export function creditedStep(step, job) {
  for (const [owner, scope] of [[job, "job"], [step, "step"]]) {
    if (!isOff(owner["continue-on-error"])) return `${scope} has continue-on-error, so a failure turns nothing red`;
    const guard = condition(owner.if);
    if (guard && !UNCONDITIONAL.has(guard)) return `${scope} runs only \`if: ${pyStr(owner.if === undefined ? null : owner.if)}\``;
  }
  return null;
}

// The crate whose ignored tests this check counts. A cargo run in another
// package runs none of them.
const WORKER_PACKAGE = "elitea-worker-rust";
// The directory cargo must run in: its Cargo.toml is the worker's. A run from
// libs/rust or the code runner builds another workspace's tests.
const WORKER_DIRECTORY = "services/elitea-worker-rust";
// Commands that run their arguments as a command: `env X=1 cargo`, `time cargo`.
const PREFIX_COMMANDS = new Set(["env", "time", "nice", "timeout", "command", "exec", "nohup"]);
// cargo test selectors this check does not model; with none of --lib/--test/
// --bins it would read the target set as "every target", which these are not.
const REFUSED_SELECTORS = {
  "--manifest-path": "builds the crate a manifest names, not necessarily the worker; run from its directory",
  "--doc": "runs doctests only",
  "--bench": "runs a benchmark target",
  "--benches": "runs benchmark targets",
  "--example": "runs an example target",
  "--examples": "runs example targets",
};
const ASSIGNMENT = /^[A-Za-z_][A-Za-z0-9_]*=/;
const SEPARATORS = new Set(["&&", "||", ";", "|", "&"]);

// ── Python's shlex.shlex(posix=True, punctuation_chars="|&;<>") ─────────────
const SHLEX_WHITESPACE = " \t\r\n";
const SHLEX_QUOTES = "'\"";
const SHLEX_ESCAPE = "\\";
const SHLEX_ESCAPED_QUOTES = '"';
const SHLEX_COMMENTERS = "#";
const SHLEX_PUNCTUATION = "|&;<>";
const SHLEX_WORDCHARS = (() => {
  const chars = "abcdfeghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_" +
    "ßàáâãäåæçèéêëìíîïðñòóôõöøùúûüýþÿÀÁÂÃÄÅÆÇÈÉÊËÌÍÎÏÐÑÒÓÔÕÖØÙÚÛÜÝÞ" + "~-./*?=";
  return [...chars].filter((c) => !SHLEX_PUNCTUATION.includes(c)).join("");
})();

// `ch in s` for a one-character string (never the empty one: callers test EOF first).
const among = (ch, set) => ch !== "" && set.includes(ch);

/** list(shlex) with whitespace_split=True and `#` comments. Throws ValueError-style Error. */
export function shlexSplit(line) {
  const chars = [...line];
  let position = 0;
  const pushback = [];
  const read = () => {
    if (pushback.length) return pushback.pop();
    return position < chars.length ? chars[position++] : "";
  };
  const readline = () => {
    while (position < chars.length) {
      if (chars[position++] === "\n") break;
    }
  };
  let state = " ";
  let token = "";

  const readToken = () => {
    let quoted = false;
    let escapedstate = " ";
    for (;;) {
      const nextchar = read();
      if (state === null) {
        token = "";
        break;
      } else if (state === " ") {
        if (!nextchar) {
          state = null;
          break;
        } else if (among(nextchar, SHLEX_WHITESPACE)) {
          if (token || quoted) break;
          continue;
        } else if (among(nextchar, SHLEX_COMMENTERS)) {
          readline();
        } else if (among(nextchar, SHLEX_ESCAPE)) {
          escapedstate = "a";
          state = nextchar;
        } else if (among(nextchar, SHLEX_WORDCHARS)) {
          token = nextchar;
          state = "a";
        } else if (among(nextchar, SHLEX_PUNCTUATION)) {
          token = nextchar;
          state = "c";
        } else if (among(nextchar, SHLEX_QUOTES)) {
          state = nextchar;
        } else {
          // whitespace_split
          token = nextchar;
          state = "a";
        }
      } else if (among(state, SHLEX_QUOTES)) {
        quoted = true;
        if (!nextchar) throw new Error("No closing quotation");
        if (nextchar === state) {
          state = "a";
        } else if (among(nextchar, SHLEX_ESCAPE) && among(state, SHLEX_ESCAPED_QUOTES)) {
          escapedstate = state;
          state = nextchar;
        } else {
          token += nextchar;
        }
      } else if (among(state, SHLEX_ESCAPE)) {
        if (!nextchar) throw new Error("No escaped character");
        // In posix shells, only the quote itself or the escape
        // character may be escaped within quotes.
        if (among(escapedstate, SHLEX_QUOTES) && nextchar !== state && nextchar !== escapedstate) {
          token += state;
        }
        token += nextchar;
        state = escapedstate;
      } else if (state === "a" || state === "c") {
        if (!nextchar) {
          state = null;
          break;
        } else if (among(nextchar, SHLEX_WHITESPACE)) {
          state = " ";
          if (token || quoted) break;
          continue;
        } else if (among(nextchar, SHLEX_COMMENTERS)) {
          readline();
          state = " ";
          if (token || quoted) break;
          continue;
        } else if (state === "c") {
          if (among(nextchar, SHLEX_PUNCTUATION)) {
            token += nextchar;
          } else {
            if (!among(nextchar, SHLEX_WHITESPACE)) pushback.push(nextchar);
            state = " ";
            break;
          }
        } else if (among(nextchar, SHLEX_QUOTES)) {
          state = nextchar;
        } else if (among(nextchar, SHLEX_ESCAPE)) {
          escapedstate = "a";
          state = nextchar;
        } else if (among(nextchar, SHLEX_WORDCHARS) || among(nextchar, SHLEX_QUOTES) ||
          !among(nextchar, SHLEX_PUNCTUATION)) {
          token += nextchar;
        } else {
          pushback.push(nextchar);
          state = " ";
          if (token || quoted) break;
          continue;
        }
      }
    }
    const result = token;
    token = "";
    if (!quoted && result === "") return null;
    return result;
  };

  const tokens = [];
  for (;;) {
    const next = readToken();
    if (next === null) break;
    tokens.push(next);
  }
  return tokens;
}

const onlyChars = (token, allowed) => [...token].every((c) => allowed.includes(c));

/**
 * Split one shell line into [words, tolerated] commands, comments dropped.
 *
 * `tolerated` is true when the command's failure is swallowed by a following
 * `|| ...`. Pipes keep failing (the runner's bash uses -o pipefail).
 */
export function commands(line, source) {
  // `<>` are punctuation too, so a redirection (`2>&1`, `>&2`, `&>log`)
  // lexes as one `>&`/`&>` token instead of a background `&`.
  let tokens;
  try {
    tokens = shlexSplit(line);
  } catch (error) {
    throw new SystemExit(`${source}: cannot parse ${pyRepr(line.trim())}: ${error.message}`);
  }
  const found = [];
  let words = [];
  for (let token of [...tokens, ";"]) {
    if (token.includes("<") || token.includes(">")) {
      words.push(token); // a redirection operator, part of this command
      continue;
    }
    if (token === "|&") token = "|"; // pipes stderr too; still a pipe
    if (SEPARATORS.has(token) || onlyChars(token, "|&;")) {
      if (words.length) {
        found.push([words, false]);
        words = [];
      }
      // `a || b` swallows a's failure; `a &` backgrounds it, so the step
      // never sees its exit status either.
      if ((token === "||" || token === "&") && found.length) found[found.length - 1] = [found[found.length - 1][0], true];
      continue;
    }
    words.push(token);
  }
  return found;
}

/**
 * The command's own words: VAR=value assignments and prefix commands removed.
 *
 * `env X=1 cargo …`, `time cargo …` and `timeout 600 cargo …` run cargo, and
 * so does `cargo +1.97.1 test …` (handled by the caller).
 */
export function program(words) {
  let index = 0;
  while (index < words.length) {
    const word = words[index];
    if (ASSIGNMENT.test(word)) {
      index += 1;
    } else if (PREFIX_COMMANDS.has(word)) {
      index += 1;
      // Their own options and operands: `env -u X`, `timeout 600`, `nice -n 5`.
      while (index < words.length) {
        const option = words[index];
        if (["-u", "--unset", "-C", "--chdir", "-n", "--adjustment", "-s", "--signal", "-k", "--kill-after"].includes(option)) {
          index += 2;
        } else if (option.startsWith("-") || ((word === "timeout" || word === "nice") && /^\p{Nd}/u.test(option))) {
          index += 1;
        } else {
          break;
        }
      }
    } else {
      break;
    }
  }
  return words.slice(index);
}

/** The cwd after `cd target` from cwd; '' when it cannot be known statically. */
export function changeDirectory(cwd, target) {
  if (!target || target.startsWith("$") || target.startsWith("~") || target.startsWith("-") || target.includes("${{")) return "";
  if (target.startsWith("/")) return "";
  const parts = [];
  for (const part of [...pathParts(cwd || "."), ...pathParts(target)]) {
    if (part === "..") {
      if (!parts.length) return "";
      parts.pop();
    } else {
      parts.push(part);
    }
  }
  return parts.join("/") || ".";
}

/** Invocations on one logical line, and the cwd the next line starts in. */
export function parseCommand(line, source, cwd = WORKER_DIRECTORY) {
  const found = [];
  const refused = [];
  for (const [words, tolerated] of commands(line, source)) {
    let argv = program(words);
    const text = words.join(" ");
    if (!argv.length) continue;
    if (argv[0] === "cd" || argv[0] === "pushd") {
      cwd = changeDirectory(cwd, argv.length > 1 ? argv[1] : "");
      continue;
    }
    // `env -C dir cargo …` runs cargo elsewhere; follow it for this command only.
    let here = cwd;
    words.slice(0, -1).forEach((word, i) => {
      if ((word === "-C" || word === "--chdir") && words.slice(0, i).includes("env")) {
        here = changeDirectory(cwd, words[i + 1]);
      }
    });
    // `cargo +1.97.1 test`: rustup's toolchain override, still cargo test.
    if (argv[0] === "cargo" && argv.length > 1 && argv[1].startsWith("+")) argv = [argv[0], ...argv.slice(2)];
    // The ledger wrapper, run directly or by a Python interpreter.
    const script = argv.length > 1 && pathName(argv[0]).startsWith("python") ? argv[1] : argv[0];
    if (pathName(script) === "test_sandbox_ledger.py") {
      const wrapper = LEDGER_WRAPPER.exec(argv.join(" "));
      if (!wrapper) continue;
      if (tolerated) {
        refused.push(new Uncredited(text, "its failure is tolerated (`||`)"));
      } else {
        found.push(new Invocation({ filters: [wrapper[1]], targets: new Set(["lib"]), source: `${source}: ${text}` }));
      }
      continue;
    }
    if (argv.includes("cargo") && argv.includes("--ignored") && argv[0] !== "cargo") {
      refused.push(new Uncredited(text, `\`${argv[0]}\` is the command, not cargo`));
      continue;
    }
    if (argv[0] !== "cargo" || argv[1] !== "test") continue;
    const [invocation, whyNotCargo] = parseCargoTest(argv.slice(2), `${source}: ${text}`);
    let whyNot = whyNotCargo;
    if (invocation === null && whyNot === null) continue;
    if (whyNot === null && here !== WORKER_DIRECTORY) {
      whyNot = here
        ? `cargo runs in \`${here}\`, not ${WORKER_DIRECTORY}, so it builds another crate's tests`
        : "cargo runs in a directory this check cannot resolve (a `cd` to a variable or an expression)";
    }
    if (whyNot) refused.push(new Uncredited(text, whyNot));
    else if (tolerated) refused.push(new Uncredited(text, "its failure is tolerated (`||`)"));
    else found.push(invocation);
  }
  return [found, refused, cwd];
}

/** Parse `cargo test` arguments. [null, null]: not an ignored-test run. */
export function parseCargoTest(tokens, source) {
  if (tokens.some((token) => token.includes("${{"))) {
    return [null, "a GitHub expression (e.g. `${{ matrix.* }}`) chooses its filters at run " +
      "time; name the tests literally so this check can see them"];
  }
  const invocation = new Invocation({ source });
  let cargoNames = 0;
  let libtest = false;
  const targets = new Set();
  let everyTarget = false;
  let listing = false;
  let ignored = false;
  const packages = [];
  let noRun = false;
  let index = 0;
  while (index < tokens.length) {
    let token = tokens[index];
    let value = null;
    if (token.includes("=") && token.startsWith("--")) {
      const at = token.indexOf("=");
      [token, value] = [token.slice(0, at), token.slice(at + 1)];
    } else if (!libtest && token.startsWith("-p") && token.length > 2) {
      // -pNAME and -p=NAME: cargo's attached short-option forms.
      const rest = token.slice(2);
      [token, value] = ["-p", rest.startsWith("=") ? rest.slice(1) : rest];
    }

    const operand = () => {
      if (value !== null) return value;
      index += 1;
      if (index >= tokens.length) throw new SystemExit(`${source}: \`${token}\` takes a value`);
      return tokens[index];
    };

    if (!libtest && token === "--") libtest = true;
    else if (libtest && token === "--skip") invocation.skips.push(operand());
    else if (libtest && token === "--exact") invocation.exact = true;
    else if (libtest && token === "--list") listing = true;
    else if (libtest && (token === "--ignored" || token === "--include-ignored")) ignored = true;
    else if (!libtest && token === "--no-run") noRun = true;
    else if (!libtest && Object.prototype.hasOwnProperty.call(REFUSED_SELECTORS, token)) {
      return [null, `\`${token}\` ${REFUSED_SELECTORS[token]}`];
    } else if (!libtest && (token === "-p" || token === "--package")) packages.push(operand());
    else if (!libtest && token === "--lib") targets.add("lib");
    else if (!libtest && token === "--bins") targets.add("bin:*");
    else if (!libtest && token === "--tests") ["lib", "bin:*", "test:*"].forEach((t) => targets.add(t));
    else if (!libtest && token === "--all-targets") everyTarget = true;
    else if (!libtest && (token === "--test" || token === "--bin")) {
      targets.add((token === "--test" ? "test:" : "bin:") + operand());
    } else if ((libtest ? LIBTEST_VALUE_FLAGS : CARGO_VALUE_FLAGS).has(token)) operand();
    else if (token.startsWith("-")) {
      // another flag
    } else {
      invocation.filters.push(token);
      cargoNames += libtest ? 0 : 1;
    }
    index += 1;
  }
  if (cargoNames > 1) {
    // cargo refuses this ("unexpected argument"), so it selects nothing.
    throw new SystemExit(`${source}: cargo takes one test name before \`--\`; put the other filters after it`);
  }
  // `-- --list --ignored` enumerates the tests (this check's own input); it runs none.
  if (listing || !ignored) return [null, null];
  if (noRun) return [null, "`--no-run` builds the tests and runs none"];
  const others = packages.filter((name) => name !== WORKER_PACKAGE);
  if (others.length) return [null, `\`-p ${others[0]}\` runs another crate's tests, not ${WORKER_PACKAGE}'s`];
  invocation.targets = everyTarget || !targets.size ? null : targets;
  return [invocation, null];
}

const isDict = (value) => value !== null && typeof value === "object" && !Array.isArray(value) && !(value instanceof Date);
const get = (owner, key, fallback = null) =>
  (isDict(owner) && Object.prototype.hasOwnProperty.call(owner, key) ? owner[key] : fallback);

export function parseWorkflow(text, source) {
  const document = loadYaml(text) || {};
  const jobs = isDict(document) ? get(document, "jobs") : null;
  if (!isDict(jobs)) throw new SystemExit(`${source}: no \`jobs:\` mapping; not a workflow`);
  const found = [];
  const refused = [];

  const defaultDirectory = (owner) => {
    if (!isDict(owner)) return null;
    const run = get(get(owner, "defaults") || {}, "run") || {};
    return isDict(run) ? get(run, "working-directory") : null;
  };

  const workflowDirectory = defaultDirectory(document);
  for (const [jobId, job] of Object.entries(jobs)) {
    const jobDirectory = defaultDirectory(job);
    const steps = get(job || {}, "steps") || [];
    // Python iterated whatever `steps:` held: a mapping's keys, a string's characters.
    const list = Array.isArray(steps) ? steps : typeof steps === "string" ? [...steps] : isDict(steps) ? Object.keys(steps) : steps;
    [...list].forEach((step, index) => {
      const number = index + 1;
      const script = isDict(step) ? get(step, "run") : null;
      if (typeof script !== "string") return;
      const where = `${source}: jobs.${jobId}.steps[${number}] (${pyStr(get(step, "name", "unnamed"))})`;
      const whyNot = creditedStep(step, job);
      const directory = get(step, "working-directory") || jobDirectory || workflowDirectory || ".";
      let cwd = changeDirectory(".", pyStr(directory).split("${{ github.workspace }}").join("."));
      for (const line of logicalLines(script)) {
        const [invocations, uncredited, next] = parseCommand(line, where, cwd);
        cwd = next;
        if (whyNot) {
          refused.push(...invocations.map((i) => new Uncredited(i.source, whyNot)));
          refused.push(...uncredited.map((u) => new Uncredited(`${where}: ${u.command}`, whyNot)));
          continue;
        }
        found.push(...invocations);
        refused.push(...uncredited.map((u) => new Uncredited(`${where}: ${u.command}`, u.reason)));
      }
    });
  }
  return [found, refused];
}

const compareStrings = (a, b) => (a < b ? -1 : a > b ? 1 : 0);

/** Names with the target they were listed under; unlabelled names are the lib. */
export function parseList(text) {
  const tests = new Map();
  let target = "lib";
  for (const line of splitlines(text)) {
    const running = RUNNING.exec(line);
    if (running) {
      target = targetOf(running[1]);
    } else if (line.endsWith(": test")) {
      const test = new Test(line.slice(0, -": test".length), target);
      tests.set(test.key, test);
    }
  }
  return [...tests.values()].sort((a, b) => compareStrings(a.target, b.target) || compareStrings(a.name, b.name));
}

export function parseAllowlist(text, source) {
  const entries = [];
  const errors = [];
  splitlines(text).forEach((raw, index) => {
    const number = index + 1;
    const line = raw.trim();
    if (!line || line.startsWith("#")) return;
    const at = line.indexOf("#");
    const prefix = (at < 0 ? line : line.slice(0, at)).trim();
    const reason = (at < 0 ? "" : line.slice(at + 1)).trim();
    if (!prefix || prefix.includes(" ")) {
      errors.push(`${source}:${number}: expected '<test name prefix>  # <where it runs>'`);
    } else if ([...reason].length < 20) {
      errors.push(`${source}:${number}: ${prefix}: the reason must say where the test runs instead`);
    } else {
      entries.push([prefix, reason]);
    }
  });
  return [entries, errors];
}

// Path.rglob("*.rs"): every file below, symlinked directories not entered.
function rustFiles(directory) {
  const out = [];
  let entries;
  try {
    entries = fs.readdirSync(directory, { withFileTypes: true });
  } catch {
    return out;
  }
  for (const entry of entries) {
    const full = path.join(directory, entry.name);
    if (entry.isDirectory()) out.push(...rustFiles(full));
    if (entry.name.endsWith(".rs") && !entry.isDirectory()) out.push(full);
  }
  return out;
}

export function countIgnoreAttributes(root) {
  let count = 0;
  for (const directory of ["src", "tests", "benches"]) {
    for (const file of rustFiles(path.join(root, directory)).sort()) {
      count += splitlines(readText(file)).filter((line) => IGNORE_ATTRIBUTE.test(line)).length;
    }
  }
  return count;
}

/** str(pathlib.PurePosixPath(p)): `//` collapsed, `.` parts and a trailing `/` dropped. */
export function pyPath(p) {
  const lead = p.startsWith("//") && !p.startsWith("///") ? "//" : p.startsWith("/") ? "/" : "";
  const body = pathParts(p).join("/");
  return lead + body || ".";
}

function label(test) {
  return test.target === "lib" ? test.name : `${test.name} [${test.target}]`;
}

export function check(tests, invocations, allowlist, sourceCount) {
  const errors = [];
  if (sourceCount !== null && sourceCount !== tests.length) {
    errors.push(
      `the sources carry ${sourceCount} #[ignore] attribute(s) but the test list names ` +
        `${tests.length} ignored test(s): the list is empty, truncated or built without ` +
        "--all-features, so it cannot vouch for anything",
    );
  }
  const used = new Set();
  for (const test of tests) {
    if (invocations.some((invocation) => invocation.selects(test))) continue;
    const matched = allowlist.filter(([prefix]) => test.name.startsWith(prefix)).map(([prefix]) => prefix);
    if (matched.length) {
      matched.forEach((prefix) => used.add(prefix));
      continue;
    }
    errors.push(
      `${label(test)} is #[ignore] and no CI \`cargo test ... -- --ignored\` invocation ` +
        "selects it; run it in CI, delete it, or allowlist it with where it runs instead",
    );
  }
  for (const [prefix] of allowlist) {
    if (!used.has(prefix) && !tests.some((test) => test.name.startsWith(prefix))) {
      errors.push(`allowlist entry ${prefix} matches no ignored test; remove it`);
    }
  }
  return errors;
}

const USAGE = "usage: check-worker-ignored-tests.mjs [-h] --list LIST --workflow WORKFLOW --allowlist ALLOWLIST [--sources SOURCES]";
/** The command line, read by node:util parseArgs (`--x v` or `--x=v`); exit 2 on misuse. */
export function parseArguments(argv) {
  const usageError = (message) => new SystemExit(`${USAGE}\ncheck-worker-ignored-tests.mjs: error: ${message}`, 2);
  let values;
  try {
    ({ values } = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: false,
      options: {
        help: { type: "boolean", short: "h" },
        list: { type: "string" },
        // argparse's action="append": every --workflow is read.
        workflow: { type: "string", multiple: true },
        allowlist: { type: "string" },
        sources: { type: "string" },
      },
    }));
  } catch (error) {
    if (typeof error?.code === "string" && error.code.startsWith("ERR_PARSE_ARGS_")) throw usageError(error.message);
    throw error;
  }
  if (values.help) {
    throw new SystemExit(`${USAGE}\n\nFail when a Rust worker \`#[ignore]\` test is run by nothing.`, 0);
  }
  const missing = ["list", "workflow", "allowlist"].filter((name) => values[name] === undefined).map((name) => `--${name}`);
  if (missing.length) throw usageError(`the following arguments are required: ${missing.join(", ")}`);
  // `type=Path`: each value is read, and printed, as str(Path(value)).
  return {
    list: pyPath(values.list),
    workflow: values.workflow.map(pyPath),
    allowlist: pyPath(values.allowlist),
    sources: values.sources === undefined ? null : pyPath(values.sources),
  };
}

/** The check. `write` receives each stdout line; a SystemExit is thrown as the Python one was raised. */
export function main(argv = process.argv.slice(2), write = (line) => process.stdout.write(`${line}\n`)) {
  const args = parseArguments(argv);
  const tests = parseList(readText(args.list));
  const invocations = [];
  const refused = [];
  for (const workflow of args.workflow) {
    const [found, skipped] = parseWorkflow(readText(workflow), workflow);
    invocations.push(...found);
    refused.push(...skipped);
  }
  const [allowlist, errors] = parseAllowlist(readText(args.allowlist), args.allowlist);
  const sourceCount = args.sources ? countIgnoreAttributes(args.sources) : null;
  errors.push(...check(tests, invocations, allowlist, sourceCount));
  if (!invocations.length) errors.push("no `cargo test ... -- --ignored` invocation found in the workflow(s)");
  for (const item of refused) write(`::notice::not credited (${item.reason}): ${item.command}`);
  for (const error of errors) write(`::error::${error}`);
  const selected = tests.filter((t) => invocations.some((i) => i.selects(t)));
  const allowed = tests.filter((t) => !selected.includes(t) && allowlist.some(([p]) => t.name.startsWith(p)));
  write(
    `${tests.length} ignored test(s): ${selected.length} run by ${invocations.length} CI ` +
      `invocation(s), ${allowed.length} allowlisted, ` +
      `${tests.length - selected.length - allowed.length} run by nothing; ` +
      `${refused.length} invocation(s) not credited; ${errors.length} error(s)`,
  );
  return errors.length ? 1 : 0;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    process.exitCode = main();
  } catch (error) {
    if (!(error instanceof SystemExit)) throw error;
    (error.code === 0 ? process.stdout : process.stderr).write(`${error.message}\n`);
    process.exitCode = error.code;
  }
}
