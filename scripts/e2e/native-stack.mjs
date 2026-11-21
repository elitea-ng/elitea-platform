#!/usr/bin/env node
// native-stack.mjs: a compose stack as plain processes, for the self-hosted pool.
//
// The eng-runner pod has no container runtime (deploy/ci-runners/README.md), so a lane that runs there
// starts each compose service as a process of the runner user. The owner's pod template gives every compose
// service name its own 127.0.0.x address ("Native stack name map" in that README), so each process binds its
// own address at its compose port and the hostnames in configs, seeded rows, mTLS SANs and specs stay exactly
// as compose writes them.
//
// A service table (deploy/native/<stack>.mjs) describes the stack; this script runs it. The verbs mirror the
// compose calls the stack scripts make (apps/elitea-web/scripts/lib/stack-backend.sh):
//
//   up [--wait]                  start every service in dependency order; --wait also waits for every
//                                service's health (compose `up -d --wait`). Without it a service still waits
//                                for the conditions its dependents name, as compose does.
//   down [-v]                    stop every service in reverse order: SIGTERM to its process group, SIGKILL
//                                10 s later (E2E_NATIVE_STOP_GRACE_MS; a service may name its own stop
//                                command first: pg_ctl stop -m fast).
//                                -v also removes the data directories (compose's volumes). Logs stay.
//   ps [--json] [--running <s>]  each service's state, exit code and health. --running prints <s> and exits
//                                0 when it runs, and exits 1 otherwise.
//   logs [svc…] [--tail N] [--timestamps]
//   restart <svc> [--wait]       stop one service and start it again (compose `restart`, which does not wait).
//   run-init <svc>               run a one-shot again and wait for its exit; its code is this command's.
//   exec <svc> -- <cmd> [args…]  run a command with the service's environment and working directory
//                                (compose `exec`); its exit code is this command's.
//
// Policy, and why it differs from the compose files: NOTHING IS RESTARTED. `restart: unless-stopped` hid a
// crash after `up`; here a crash is a failure. Each service runs under a small logger process (this script,
// `__run`), the leader of its own process group: it stamps every output line (`<ISO time> <svc> | <line>`)
// into logs/<svc>.log and records the exit code in exit/<svc>, so `ps` and `logs` show a crash that happened
// long after `up` returned.
//
// State: $E2E_NATIVE_STATE_DIR, else $RUNNER_TEMP/native-stack (the pool's work volume: the pod has little
// ephemeral storage outside it), else <os tmpdir>/native-stack.
//
//   run/<svc>.json   what was spawned (pid = process group), written at start
//   exit/<svc>       the exit code, written when the service ends
//   logs/<svc>.log   stamped output
//   data/<svc>/      the service's files and data (removed by `down -v`)
//
// The table: E2E_NATIVE_TABLE or --table <file>, default deploy/native/e2e-standalone.mjs. Its default export
// is `(ctx) => ({ services })`; ctx is { repo, state, data(svc), bin, env }. A service is
//
//   { name, ip?, hostnames?, cmd, args?, env?, cwd?, init?, setup?, files?, stop?,
//     dependsOn?: [{ name, condition: "healthy" | "completed" | "started" }],
//     health?: { kind: "http" | "tcp" | "cmd" | "pg", url | host+port | cmd+args | host+port+user+db,
//                interval, timeout, retries, startPeriod } }   (seconds, as compose writes them)
//
// `setup` is a sh script run (with the service's env, in its data dir) before the first start; `files` are
// written before every start ({ path, content, mode }). Services inherit only a short list of the caller's
// variables (BASE_ENV), as a container inherits none of the host's.
//
// No dependencies beyond node: it runs before `npm ci` and with any node the job installed.

import { spawn, spawnSync } from "node:child_process";
import crypto from "node:crypto";
import dns from "node:dns/promises";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const SELF = fileURLToPath(import.meta.url);
export const REPO = path.resolve(path.dirname(SELF), "..", "..");
export const DEFAULT_TABLE = path.join(REPO, "deploy", "native", "e2e-standalone.mjs");
/** The caller's variables a service sees; everything else comes from its table entry. */
export const BASE_ENV = ["PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "LANG", "LC_ALL", "TZ", "GOMAXPROCS", "SSL_CERT_FILE", "SSL_CERT_DIR"];
export const STOP_GRACE_MS = 10_000;
export const TAIL_ON_FAILURE = 80;
const CONDITIONS = new Set(["healthy", "completed", "started"]);

export function stateDir(env = process.env) {
  if (env.E2E_NATIVE_STATE_DIR) return path.resolve(env.E2E_NATIVE_STATE_DIR);
  if (env.RUNNER_TEMP) return path.join(env.RUNNER_TEMP, "native-stack");
  return path.join(os.tmpdir(), "native-stack");
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const secs = (v, d) => Math.round((v ?? d) * 1000);

// ── the table ─────────────────────────────────────────────────────────────────

/**
 * A short directory for one stack's Unix sockets, when the state path is too long for one (~104 bytes):
 * /tmp/ns-<hash of the state dir>/<tag>. Keyed on the state dir, so two stacks (or two jobs on one pod) never
 * share one, and `down -v` removes it with the data.
 */
export const shortRoot = (state) => path.join("/tmp", `ns-${crypto.createHash("sha256").update(path.resolve(state)).digest("hex").slice(0, 12)}`);

/** Loads a table module and validates it: unique names, known dependencies and conditions, no cycle. */
export async function loadTable(file, { state, env = process.env } = {}) {
  const mod = await import(pathToFileURL(path.resolve(file)).href);
  const st = state ?? stateDir(env);
  const ctx = {
    repo: REPO,
    state: st,
    data: (svc) => path.join(st, "data", svc),
    short: (tag) => path.join(shortRoot(st), tag),
    bin: env.E2E_NATIVE_BIN ? path.resolve(env.E2E_NATIVE_BIN) : path.join(REPO, ".native-bin"),
    env,
  };
  const table = typeof mod.default === "function" ? mod.default(ctx) : mod.default;
  const services = table?.services ?? [];
  validate(services);
  return { ...table, services, ctx };
}

export function validate(services) {
  const errors = [];
  const names = new Set();
  for (const s of services) {
    if (!s.name || !/^[a-z0-9][a-z0-9_.-]*$/.test(s.name)) errors.push(`bad service name ${JSON.stringify(s.name)}`);
    if (names.has(s.name)) errors.push(`service ${s.name} is declared twice`);
    names.add(s.name);
    if (!s.cmd) errors.push(`${s.name}: no cmd`);
    if (s.health && !["http", "tcp", "cmd", "pg"].includes(s.health.kind)) errors.push(`${s.name}: unknown health kind ${s.health.kind}`);
    if (s.init && s.health) errors.push(`${s.name}: a one-shot (init) has no health check; dependents wait for "completed"`);
  }
  const byName = new Map(services.map((s) => [s.name, s]));
  for (const s of services) {
    for (const d of s.dependsOn ?? []) {
      const dep = byName.get(d.name);
      if (!dep) errors.push(`${s.name} depends on unknown service ${d.name}`);
      if (!CONDITIONS.has(d.condition)) errors.push(`${s.name} -> ${d.name}: unknown condition ${d.condition}`);
      if (dep && d.condition === "healthy" && !dep.health) errors.push(`${s.name} waits for ${d.name} to be healthy, which has no health check`);
      if (dep && d.condition === "completed" && !dep.init) errors.push(`${s.name} waits for ${d.name} to complete, which is not a one-shot`);
    }
  }
  if (!errors.length) topoSort(services); // throws on a cycle
  if (errors.length) throw new Error(`invalid service table:\n  ${errors.join("\n  ")}`);
}

/** Dependencies first; ties keep table order, so the order is deterministic. */
export function topoSort(services) {
  const byName = new Map(services.map((s) => [s.name, s]));
  const out = [];
  const mark = new Map();
  const visit = (s, trail) => {
    if (mark.get(s.name) === "done") return;
    if (mark.get(s.name) === "active") throw new Error(`dependency cycle: ${[...trail, s.name].join(" -> ")}`);
    mark.set(s.name, "active");
    for (const d of s.dependsOn ?? []) visit(byName.get(d.name), [...trail, s.name]);
    mark.set(s.name, "done");
    out.push(s);
  };
  for (const s of services) visit(s, []);
  return out;
}

// ── state ─────────────────────────────────────────────────────────────────────

const paths = (st, name) => ({
  run: path.join(st, "run", `${name}.json`),
  exit: path.join(st, "exit", name),
  log: path.join(st, "logs", `${name}.log`),
  data: path.join(st, "data", name),
});

const readJson = (f) => {
  try {
    return JSON.parse(fs.readFileSync(f, "utf8"));
  } catch {
    return null;
  }
};

export function exitCode(st, name) {
  try {
    const text = fs.readFileSync(paths(st, name).exit, "utf8").trim();
    return text === "" ? null : Number(text);
  } catch {
    return null;
  }
}

/** The process group still has a live member (a zombie leader alone does not count). */
/** The live (not zombie) processes whose process group is pgid, read from /proc (Linux). */
export function groupMembers(pgid, proc = "/proc") {
  const out = [];
  let entries = [];
  try {
    entries = fs.readdirSync(proc);
  } catch {
    return out;
  }
  for (const name of entries) {
    if (!/^\d+$/.test(name)) continue;
    try {
      const stat = fs.readFileSync(path.join(proc, name, "stat"), "utf8");
      // pid (comm) state ppid pgrp ...: comm may hold spaces and parens, so split after the last ")".
      const fields = stat.slice(stat.lastIndexOf(")") + 2).split(" ");
      if (Number(fields[2]) === pgid && fields[0] !== "Z" && fields[0] !== "X") out.push(Number(name));
    } catch {
      // gone between readdir and read
    }
  }
  return out;
}

export function groupAlive(pgid) {
  if (!pgid) return false;
  try {
    process.kill(-pgid, 0);
  } catch {
    return false;
  }
  // kill(0) succeeds on a zombie, and a detached logger's parent is gone; on Linux read its state.
  try {
    const stat = fs.readFileSync(`/proc/${pgid}/stat`, "utf8");
    const st = stat.slice(stat.lastIndexOf(")") + 2, stat.lastIndexOf(")") + 3);
    if (st === "Z" || st === "X") {
      // The leader is a zombie; another member of the group may still run (and hold its port).
      return groupMembers(pgid).some((p) => p !== pgid);
    }
  } catch {
    // not Linux, or gone: kill(0) said it exists
  }
  return true;
}

/** "running", "exited" or "created" (never started), from the files alone. */
export function serviceState(st, name) {
  const run = readJson(paths(st, name).run);
  if (!run) return { state: "created", exit: null, pid: null };
  const code = exitCode(st, name);
  if (code !== null) return { state: "exited", exit: code, pid: run.pid };
  return groupAlive(run.pid) ? { state: "running", exit: null, pid: run.pid } : { state: "exited", exit: null, pid: run.pid };
}

// ── processes ─────────────────────────────────────────────────────────────────

export function serviceEnv(svc, env = process.env) {
  const out = {};
  for (const k of BASE_ENV) if (env[k] !== undefined) out[k] = env[k];
  for (const [k, v] of Object.entries(svc.env ?? {})) if (v !== undefined && v !== null) out[k] = String(v);
  return out;
}

function prepare(st, svc, env) {
  const p = paths(st, svc.name);
  for (const d of ["run", "exit", "logs"]) fs.mkdirSync(path.join(st, d), { recursive: true });
  fs.mkdirSync(p.data, { recursive: true });
  const senv = serviceEnv(svc, env);
  if (svc.setup && !fs.existsSync(path.join(p.data, ".setup-done"))) {
    const r = spawnSync("sh", ["-ec", svc.setup], { cwd: p.data, env: senv, encoding: "utf8" });
    fs.appendFileSync(p.log, stamp(svc.name, `${r.stdout ?? ""}${r.stderr ?? ""}`));
    if (r.status !== 0) throw new Error(`${svc.name}: setup failed (exit ${r.status}); see ${p.log}`);
    fs.writeFileSync(path.join(p.data, ".setup-done"), "");
  }
  for (const f of svc.files ?? []) {
    fs.mkdirSync(path.dirname(f.path), { recursive: true });
    fs.writeFileSync(f.path, typeof f.content === "function" ? f.content() : f.content);
    fs.chmodSync(f.path, f.mode ?? 0o644);
  }
  return senv;
}

function stamp(name, text) {
  return text
    .split("\n")
    .filter((l, i, a) => !(l === "" && i === a.length - 1))
    .map((l) => `${new Date().toISOString()} ${name} | ${l}\n`)
    .join("");
}

/** Starts one service under its logger and returns at once. */
export function start(st, svc, env = process.env) {
  const senv = prepare(st, svc, env);
  const p = paths(st, svc.name);
  fs.rmSync(p.exit, { force: true });
  const spec = { name: svc.name, cmd: svc.cmd, args: (svc.args ?? []).map(String), env: senv, cwd: svc.cwd ?? p.data, log: p.log, exit: p.exit };
  const specFile = `${p.run}.spec`;
  fs.writeFileSync(specFile, JSON.stringify(spec));
  const child = spawn(process.execPath, [SELF, "__run", specFile], { detached: true, stdio: "ignore", cwd: spec.cwd });
  child.unref();
  fs.writeFileSync(p.run, JSON.stringify({ pid: child.pid, started: new Date().toISOString(), cmd: svc.cmd, args: spec.args }));
  return child.pid;
}

/** The logger: the process-group leader of one service. Stamps its output and records its exit. */
async function runLogger(specFile) {
  const spec = JSON.parse(fs.readFileSync(specFile, "utf8"));
  const out = fs.openSync(spec.log, "a");
  const write = (text) => fs.writeSync(out, stamp(spec.name, text));
  // The group's SIGTERM reaches the service directly; the logger stays to record what it prints on the way
  // out, and its exit. (A handled signal is reset by exec, so the service keeps the default action.)
  for (const sig of ["SIGTERM", "SIGINT", "SIGHUP"]) process.on(sig, () => {});
  const child = spawn(spec.cmd, spec.args, { cwd: spec.cwd, env: spec.env, stdio: ["ignore", "pipe", "pipe"] });
  const pump = (stream) => {
    let buf = "";
    stream.setEncoding("utf8");
    stream.on("data", (chunk) => {
      buf += chunk;
      const i = buf.lastIndexOf("\n");
      if (i >= 0) {
        write(buf.slice(0, i + 1));
        buf = buf.slice(i + 1);
      }
    });
    return new Promise((r) => stream.on("end", () => (buf && write(buf), r())));
  };
  const done = Promise.all([pump(child.stdout), pump(child.stderr)]);
  const code = await new Promise((resolve) => {
    child.on("error", (e) => {
      write(`native-stack: cannot start ${spec.cmd}: ${e.message}`);
      resolve(127);
    });
    child.on("exit", (c, sig) => resolve(c ?? 128 + (os.constants.signals[sig] ?? 0)));
  });
  await Promise.race([done, sleep(2000)]);
  write(`native-stack: ${spec.name} exited with ${code}`);
  fs.writeFileSync(spec.exit, `${code}\n`);
  fs.closeSync(out);
  process.exit(0);
}

/** Stops one service: its own stop command first, then SIGTERM to the group, SIGKILL after the grace. */
export async function stop(st, svc, env = process.env, grace = Number(env.E2E_NATIVE_STOP_GRACE_MS || STOP_GRACE_MS)) {
  const run = readJson(paths(st, svc.name).run);
  if (!run || !groupAlive(run.pid)) return false;
  if (svc.stop) {
    spawnSync(svc.stop.cmd, (svc.stop.args ?? []).map(String), { env: serviceEnv(svc, env), stdio: "ignore", timeout: grace });
  }
  try {
    process.kill(-run.pid, "SIGTERM");
  } catch {
    // already gone
  }
  const deadline = Date.now() + grace;
  while (Date.now() < deadline && groupAlive(run.pid)) await sleep(100);
  if (groupAlive(run.pid)) {
    try {
      process.kill(-run.pid, "SIGKILL");
    } catch {
      // gone between the check and the kill
    }
    while (groupAlive(run.pid)) await sleep(50);
  }
  return true;
}

// ── health ────────────────────────────────────────────────────────────────────

/** One probe. Resolves true or false, never throws; bounded by the check's timeout. */
export async function probe(svc, env = process.env) {
  const h = svc.health;
  const timeout = secs(h.timeout, 5);
  try {
    if (h.kind === "http") {
      const res = await fetch(h.url, { signal: AbortSignal.timeout(timeout), redirect: "follow" });
      await res.arrayBuffer().catch(() => {});
      return res.ok;
    }
    if (h.kind === "tcp") {
      return await new Promise((resolve) => {
        const sock = net.connect({ host: h.host, port: Number(h.port) });
        const done = (ok) => (sock.destroy(), resolve(ok));
        sock.setTimeout(timeout, () => done(false));
        sock.on("connect", () => done(true));
        sock.on("error", () => done(false));
      });
    }
    const [cmd, args] =
      h.kind === "pg"
        ? ["pg_isready", ["-q", "-h", h.host, "-p", String(h.port ?? 5432), "-U", h.user, ...(h.db ? ["-d", h.db] : [])]]
        : [h.cmd, h.args ?? []];
    return await new Promise((resolve) => {
      const c = spawn(cmd, args.map(String), { env: serviceEnv(svc, env), stdio: "ignore" });
      const t = setTimeout(() => (c.kill("SIGKILL"), resolve(false)), timeout);
      c.on("error", () => (clearTimeout(t), resolve(false)));
      c.on("exit", (code) => (clearTimeout(t), resolve(code === 0)));
    });
  } catch {
    return false;
  }
}

/**
 * Waits as compose does: a check every `interval`; failures inside `startPeriod` do not count; `retries`
 * failures after it make the service unhealthy. The first check runs at once (compose waits one interval).
 * A service that exits while it is waited on fails at once.
 */
export async function waitHealthy(st, svc, env = process.env) {
  const h = svc.health;
  const interval = secs(h.interval, 5);
  const startPeriod = secs(h.startPeriod, 0);
  const retries = h.retries ?? 3;
  const began = Date.now();
  let failures = 0;
  for (;;) {
    if (await probe(svc, env)) return { ok: true };
    const s = serviceState(st, svc.name);
    if (s.state !== "running") return { ok: false, why: `exited (${s.exit ?? "unknown"}) before it was healthy` };
    if (Date.now() - began >= startPeriod) failures += 1;
    if (failures >= retries) return { ok: false, why: `unhealthy after ${retries} failed check(s)` };
    await sleep(interval);
  }
}

export async function waitExit(st, name, pollMs = 100) {
  for (;;) {
    const s = serviceState(st, name);
    if (s.state !== "running") return s.exit;
    await sleep(pollMs);
  }
}

// ── verbs ─────────────────────────────────────────────────────────────────────

/** Every hostname the table names must resolve to that service's address (the pod's hostAliases). */
export async function checkNames(services) {
  const wrong = [];
  for (const s of services) {
    for (const host of s.hostnames ?? []) {
      let got;
      try {
        got = (await dns.lookup(host, { family: 4 })).address;
      } catch (e) {
        got = `unresolved (${e.code ?? e.message})`;
      }
      if (got !== s.ip) wrong.push(`${host} -> ${got}, want ${s.ip}`);
    }
  }
  return wrong;
}

function tailLines(file, n) {
  try {
    const lines = fs.readFileSync(file, "utf8").split("\n");
    if (lines.at(-1) === "") lines.pop();
    return n > 0 ? lines.slice(-n) : [];
  } catch {
    return [];
  }
}

export async function up(table, { wait = false, env = process.env, out = console.log, err = console.error } = {}) {
  const st = table.ctx.state;
  const order = topoSort(table.services);
  if (env.E2E_NATIVE_SKIP_NAME_CHECK !== "1") {
    const wrong = await checkNames(order);
    if (wrong.length) {
      err(`native-stack: the service names do not resolve to their addresses:\n  ${wrong.join("\n  ")}`);
      err("  The pool's pod template must carry the hostAliases of deploy/ci-runners/README.md (\"Native stack name map\").");
      return 1;
    }
  }
  const byName = new Map(order.map((s) => [s.name, s]));
  const started = new Map();
  const healthy = new Map();
  const completed = new Map();
  const failures = new Map();
  const fail = (name, why) => {
    if (!failures.has(name)) failures.set(name, why);
    throw new Error(`${name}: ${why}`);
  };
  const once = (map, name, fn) => {
    if (!map.has(name)) map.set(name, fn());
    return map.get(name);
  };
  const ensureStarted = (svc) =>
    once(started, svc.name, async () => {
      for (const d of svc.dependsOn ?? []) await condition(byName.get(d.name), d.condition);
      const s = serviceState(st, svc.name);
      if (s.state === "running") return;
      if (svc.init && s.state === "exited" && s.exit === 0) return;
      out(`native-stack: starting ${svc.name}`);
      try {
        start(st, svc, env);
      } catch (e) {
        fail(svc.name, e.message);
      }
    });
  const condition = (svc, cond) => {
    if (cond === "started") return ensureStarted(svc);
    if (cond === "completed")
      return once(completed, svc.name, async () => {
        await ensureStarted(svc);
        const code = await waitExit(st, svc.name);
        if (code !== 0) fail(svc.name, `one-shot exited ${code ?? "without a code"}`);
      });
    return once(healthy, svc.name, async () => {
      await ensureStarted(svc);
      const r = await waitHealthy(st, svc, env);
      if (!r.ok) fail(svc.name, r.why);
      out(`native-stack: ${svc.name} is healthy`);
    });
  };
  const goal = (svc) => (svc.init ? condition(svc, "completed") : wait && svc.health ? condition(svc, "healthy") : ensureStarted(svc));
  const results = await Promise.allSettled(order.map(goal));
  // A service that started and then died is a failure even when nothing waited on it.
  for (const svc of order) {
    const s = serviceState(st, svc.name);
    if (!svc.init && s.state === "exited" && !failures.has(svc.name)) failures.set(svc.name, `exited (${s.exit ?? "unknown"})`);
  }
  if (failures.size || results.some((r) => r.status === "rejected")) {
    err("✗ The native stack did not come up. Services that are not running and healthy:");
    err(await psText(table, env));
    for (const [name, why] of failures) {
      err(`=== ${name}: ${why}; last ${TAIL_ON_FAILURE} log lines ===`);
      for (const l of tailLines(paths(st, name).log, TAIL_ON_FAILURE)) err(l);
    }
    for (const r of results) if (r.status === "rejected" && ![...failures.keys()].some((n) => r.reason.message.startsWith(`${n}:`))) err(r.reason.message);
    return 1;
  }
  out(`native-stack: ${order.length} service(s) up${wait ? " and healthy" : ""}`);
  return 0;
}

export async function down(table, { volumes = false, env = process.env, out = console.log } = {}) {
  const st = table.ctx.state;
  for (const svc of topoSort(table.services).reverse()) {
    if (await stop(st, svc, env)) out(`native-stack: stopped ${svc.name}`);
    fs.rmSync(paths(st, svc.name).run, { force: true });
    fs.rmSync(`${paths(st, svc.name).run}.spec`, { force: true });
    if (volumes) fs.rmSync(paths(st, svc.name).data, { recursive: true, force: true });
  }
  if (volumes) fs.rmSync(shortRoot(st), { recursive: true, force: true });
  return 0;
}

export async function psRows(table, env = process.env) {
  const st = table.ctx.state;
  return Promise.all(
    topoSort(table.services).map(async (svc) => {
      const s = serviceState(st, svc.name);
      let health = "";
      if (svc.health && s.state === "running") health = (await probe({ ...svc, health: { ...svc.health, timeout: Math.min(svc.health.timeout ?? 5, 2) } }, env)) ? "healthy" : "unhealthy";
      return { service: svc.name, state: s.state, exit: s.exit, health, pid: s.pid, init: !!svc.init };
    }),
  );
}

async function psText(table, env) {
  const rows = await psRows(table, env);
  const head = ["SERVICE", "STATE", "EXIT", "HEALTH", "PID"];
  const cells = rows.map((r) => [r.service, r.state, r.exit ?? "", r.health, r.pid ?? ""].map(String));
  const width = head.map((h, i) => Math.max(h.length, ...cells.map((c) => c[i].length)));
  return [head, ...cells].map((c) => c.map((v, i) => v.padEnd(width[i])).join("  ").trimEnd()).join("\n");
}

export function logs(table, { services = [], tail = null, timestamps = false } = {}) {
  const st = table.ctx.state;
  const names = services.length ? services : table.services.map((s) => s.name);
  const unknown = names.filter((n) => !table.services.some((s) => s.name === n));
  if (unknown.length) throw new Error(`no such service: ${unknown.join(", ")}`);
  const lines = [];
  for (const name of names) {
    const own = tail === null ? tailLines(paths(st, name).log, Infinity) : tailLines(paths(st, name).log, tail);
    lines.push(...own);
  }
  // Interleaved by time, as `compose logs` reads; the stamp sorts as a string.
  lines.sort((a, b) => (a.slice(0, 24) < b.slice(0, 24) ? -1 : a.slice(0, 24) > b.slice(0, 24) ? 1 : 0));
  return timestamps ? lines : lines.map((l) => l.replace(/^\S+ /, ""));
}

export async function restart(table, name, { wait = false, env = process.env, out = console.log, err = console.error } = {}) {
  const svc = table.services.find((s) => s.name === name);
  if (!svc) throw new Error(`no such service: ${name}`);
  const st = table.ctx.state;
  await stop(st, svc, env);
  out(`native-stack: restarting ${name}`);
  start(st, svc, env);
  if (!wait || !svc.health) return 0;
  const r = await waitHealthy(st, svc, env);
  if (r.ok) return 0;
  err(`native-stack: ${name} ${r.why}`);
  for (const l of tailLines(paths(st, name).log, TAIL_ON_FAILURE)) err(l);
  return 1;
}

export async function runInit(table, name, { env = process.env, err = console.error } = {}) {
  const svc = table.services.find((s) => s.name === name);
  if (!svc) throw new Error(`no such service: ${name}`);
  if (!svc.init) throw new Error(`${name} is not a one-shot (init: true)`);
  const st = table.ctx.state;
  start(st, svc, env);
  const code = await waitExit(st, name);
  if (code !== 0) for (const l of tailLines(paths(st, name).log, TAIL_ON_FAILURE)) err(l);
  return code ?? 1;
}

export function execIn(table, name, argv, { env = process.env } = {}) {
  const svc = table.services.find((s) => s.name === name);
  if (!svc) throw new Error(`no such service: ${name}`);
  if (!argv.length) throw new Error("exec needs a command after --");
  const cwd = svc.cwd ?? paths(table.ctx.state, name).data;
  fs.mkdirSync(cwd, { recursive: true });
  const r = spawnSync(argv[0], argv.slice(1), { env: serviceEnv(svc, env), cwd, stdio: "inherit" });
  if (r.error) throw r.error;
  return r.status ?? 1;
}

// ── CLI ───────────────────────────────────────────────────────────────────────

export async function main(argv = process.argv.slice(2), { env = process.env, out = console.log, err = console.error } = {}) {
  if (argv[0] === "__run") return runLogger(argv[1]);
  let tableFile = env.E2E_NATIVE_TABLE || DEFAULT_TABLE;
  const args = [];
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === "--") {
      args.push(...argv.slice(i));
      break;
    }
    if (argv[i] === "--table") tableFile = argv[++i];
    else args.push(argv[i]);
  }
  const [verb, ...rest] = args;
  const flag = (f) => rest.includes(f);
  const value = (f) => (rest.includes(f) ? rest[rest.indexOf(f) + 1] : null);
  const table = await loadTable(tableFile, { env });
  switch (verb) {
    case "up":
      return up(table, { wait: flag("--wait"), env, out, err });
    case "down":
      return down(table, { volumes: flag("-v") || flag("--volumes"), env, out });
    case "ps": {
      const running = value("--running");
      if (running !== null) {
        if (!table.services.some((s) => s.name === running)) throw new Error(`no such service: ${running}`);
        if (serviceState(table.ctx.state, running).state !== "running") return 1;
        out(running);
        return 0;
      }
      if (flag("--json")) out(JSON.stringify(await psRows(table, env)));
      else out(await psText(table, env));
      return 0;
    }
    case "logs": {
      const skip = new Set();
      rest.forEach((a, i) => a === "--tail" && skip.add(i + 1));
      const services = rest.filter((a, i) => !a.startsWith("-") && !skip.has(i));
      const tail = value("--tail");
      for (const l of logs(table, { services, tail: tail === null || tail === "all" ? null : Number(tail), timestamps: flag("--timestamps") || flag("-t") })) out(l);
      return 0;
    }
    case "restart":
      if (!rest[0] || rest[0].startsWith("-")) throw new Error("restart needs a service");
      return restart(table, rest[0], { wait: flag("--wait"), env, out, err });
    case "run-init":
      if (!rest[0]) throw new Error("run-init needs a service");
      return runInit(table, rest[0], { env, err });
    case "exec": {
      const sep = rest.indexOf("--");
      if (sep !== 1) throw new Error("usage: exec <svc> -- <cmd> [args…]");
      return execIn(table, rest[0], rest.slice(sep + 1), { env });
    }
    default:
      err("usage: native-stack.mjs [--table <file>] {up [--wait]|down [-v]|ps [--json|--running <svc>]|logs [svc…] [--tail N] [--timestamps]|restart <svc> [--wait]|run-init <svc>|exec <svc> -- <cmd…>}");
      return 2;
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === SELF) {
  main().then(
    (code) => process.exit(code ?? 0),
    (e) => {
      console.error(`native-stack: ${e.message}`);
      process.exit(1);
    },
  );
}
