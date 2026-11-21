// node --test scripts/e2e/native-stack.test.mjs
//
// The supervisor against tiny fake services: node one-liners and sleep, on 127.0.0.1 and free ports, in a
// temporary state directory. Each test writes its own table module.

import assert from "node:assert/strict";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { after, test } from "node:test";

import { groupAlive, groupMembers, main, shortRoot, topoSort, validate } from "./native-stack.mjs";

const NODE = process.execPath;
const roots = [];
after(() => {
  for (const r of roots) fs.rmSync(r, { recursive: true, force: true });
});

async function freePort() {
  return new Promise((resolve) => {
    const s = net.createServer().listen(0, "127.0.0.1", () => {
      const { port } = s.address();
      s.close(() => resolve(port));
    });
  });
}

/** A stack in a fresh directory: `services` is the source of the table's array (it may use `ctx`). */
function stack(services) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "nstest-"));
  roots.push(root);
  const table = path.join(root, "table.mjs");
  fs.writeFileSync(table, `export default (ctx) => ({ services: ${services} });\n`);
  const env = { ...process.env, E2E_NATIVE_STATE_DIR: path.join(root, "state"), E2E_NATIVE_SKIP_NAME_CHECK: "1", E2E_NATIVE_STOP_GRACE_MS: "1500" };
  const run = async (...argv) => {
    const out = [];
    const err = [];
    const code = await main(["--table", table, ...argv], { env, out: (l) => out.push(String(l)), err: (l) => err.push(String(l)) });
    return { code, out: out.join("\n"), err: err.join("\n") };
  };
  return { root, state: env.E2E_NATIVE_STATE_DIR, run, env };
}

const js = (code) => JSON.stringify(code);
const httpServer = (port, delayMs = 0) =>
  `{ cmd: ${js(NODE)}, args: ["-e", ${js(`setTimeout(() => require("http").createServer((q, r) => r.end("ok")).listen(${port}, "127.0.0.1", () => console.log("listening on ${port}")), ${delayMs})`)}] }`;
const pids = (state) => fs.readdirSync(path.join(state, "run")).filter((f) => f.endsWith(".json")).map((f) => JSON.parse(fs.readFileSync(path.join(state, "run", f), "utf8")).pid);

test("dependencies start first, and a healthy condition waits for the health check", async () => {
  const port = await freePort();
  // `b` dials a's port the moment it starts: refused means it started too early.
  const s = stack(`[
    { ...${httpServer(port, 400)}, name: "a", health: { kind: "http", url: "http://127.0.0.1:${port}/", interval: 0.1, retries: 50 } },
    { name: "b", cmd: ${js(NODE)}, args: ["-e", ${js(`require("net").connect(${port}, "127.0.0.1").on("connect", () => { console.log("reached a"); setInterval(() => {}, 1000); }).on("error", () => process.exit(3))`)}],
      dependsOn: [{ name: "a", condition: "healthy" }] },
  ]`);
  const up = await s.run("up", "--wait");
  assert.equal(up.code, 0, up.err);
  assert.match(up.out, /starting a[\s\S]*a is healthy[\s\S]*starting b/);
  await new Promise((r) => setTimeout(r, 300));
  const ps = JSON.parse((await s.run("ps", "--json")).out);
  assert.deepEqual(ps.map((r) => [r.service, r.state, r.health]), [["a", "running", "healthy"], ["b", "running", ""]]);
  const logs = (await s.run("logs", "--timestamps")).out;
  assert.match(logs, /^\d{4}-\d\d-\d\dT\S+Z a \| listening on/m);
  assert.match(logs, /^\S+ b \| reached a$/m);
  assert.doesNotMatch((await s.run("logs", "b")).out, /^\d{4}-/m, "without --timestamps the stamp is dropped");
  assert.equal((await s.run("down")).code, 0);
});

test("a one-shot must exit 0 before its dependents start; a failing one stops them", async () => {
  const s = stack(`[
    { name: "init-ok", init: true, cmd: "sh", args: ["-c", "echo seeded > done.txt"] },
    { name: "after-ok", cmd: "sleep", args: ["30"], dependsOn: [{ name: "init-ok", condition: "completed" }] },
    { name: "init-bad", init: true, cmd: "sh", args: ["-c", "echo nope >&2; exit 4"] },
    { name: "after-bad", cmd: "sleep", args: ["30"], dependsOn: [{ name: "init-bad", condition: "completed" }] },
  ]`);
  const up = await s.run("up");
  assert.equal(up.code, 1);
  assert.match(up.err, /=== init-bad: one-shot exited 4; last 80 log lines ===/);
  assert.match(up.err, /init-bad \| nope/);
  const ps = Object.fromEntries(JSON.parse((await s.run("ps", "--json")).out).map((r) => [r.service, r]));
  assert.equal(ps["init-ok"].state, "exited");
  assert.equal(ps["init-ok"].exit, 0);
  assert.equal(ps["after-ok"].state, "running");
  assert.equal(ps["init-bad"].exit, 4);
  assert.equal(ps["after-bad"].state, "created", "a dependent of a failed one-shot never starts");
  assert.equal(fs.readFileSync(path.join(s.state, "data", "init-ok", "done.txt"), "utf8"), "seeded\n");
  // run-init runs it again and returns its code.
  assert.equal((await s.run("run-init", "init-ok")).code, 0);
  assert.equal((await s.run("run-init", "init-bad")).code, 4);
  await s.run("down");
});

test("a service that never becomes healthy fails up, with its log", async () => {
  const s = stack(`[
    { name: "deaf", cmd: "sh", args: ["-c", "echo booting; exec sleep 30"], health: { kind: "tcp", host: "127.0.0.1", port: ${await freePort()}, interval: 0.1, retries: 3, timeout: 0.2 } },
  ]`);
  const up = await s.run("up", "--wait");
  assert.equal(up.code, 1);
  assert.match(up.err, /SERVICE\s+STATE/);
  assert.match(up.err, /=== deaf: unhealthy after 3 failed check\(s\); last 80 log lines ===/);
  assert.match(up.err, /deaf \| booting/);
  await s.run("down");
});

test("startPeriod failures do not count toward retries", async () => {
  const port = await freePort();
  const s = stack(`[
    { ...${httpServer(port, 700)}, name: "slow", health: { kind: "http", url: "http://127.0.0.1:${port}/", interval: 0.1, retries: 2, startPeriod: 2 } },
  ]`);
  const up = await s.run("up", "--wait");
  assert.equal(up.code, 0, up.err);
  await s.run("down");
});

test("a crash after up is visible to ps and logs, and nothing restarts it", async () => {
  const s = stack(`[
    { name: "flaky", cmd: "sh", args: ["-c", "echo up; sleep 0.4; echo dying >&2; exit 7"] },
  ]`);
  assert.equal((await s.run("up")).code, 0);
  await new Promise((r) => setTimeout(r, 1200));
  const ps = await s.run("ps");
  assert.match(ps.out, /^flaky\s+exited\s+7/m);
  assert.equal((await s.run("ps", "--running", "flaky")).code, 1);
  assert.match((await s.run("logs", "flaky", "--tail", "2")).out, /flaky \| dying\nflaky \| native-stack: flaky exited with 7/);
  assert.equal(JSON.parse((await s.run("ps", "--json")).out)[0].state, "exited");
  await s.run("down");
});

test("a service that exits before its health check passes fails at once", async () => {
  const s = stack(`[
    { name: "dies", cmd: "sh", args: ["-c", "exit 9"], health: { kind: "cmd", cmd: "false", interval: 0.1, retries: 1000 } },
  ]`);
  const t0 = Date.now();
  const up = await s.run("up", "--wait");
  assert.equal(up.code, 1);
  assert.match(up.err, /dies: exited \(9\) before it was healthy/);
  assert.ok(Date.now() - t0 < 5000, "it does not wait out the retries");
});

test("down stops every process group, SIGKILLs one that ignores SIGTERM, and -v removes the data", async () => {
  const s = stack(`[
    { name: "polite", cmd: "sleep", args: ["30"], files: [{ path: ctx.data("polite") + "/conf/x.conf", content: "x" }] },
    { name: "stubborn", cmd: "sh", args: ["-c", "trap '' TERM; sleep 30 & wait; sleep 30"], dependsOn: [{ name: "polite", condition: "started" }] },
  ]`);
  assert.equal((await s.run("up")).code, 0);
  await new Promise((r) => setTimeout(r, 200));
  const groups = pids(s.state);
  assert.equal(groups.length, 2);
  assert.ok(groups.every(groupAlive));
  assert.ok(fs.existsSync(path.join(s.state, "data", "polite", "conf", "x.conf")));
  const down = await s.run("down", "-v");
  assert.equal(down.code, 0);
  assert.match(down.out, /stopped stubborn[\s\S]*stopped polite/, "reverse dependency order");
  assert.ok(!groups.some(groupAlive), "every group is gone");
  assert.ok(!fs.existsSync(path.join(s.state, "data", "polite")), "-v removes the data");
  assert.ok(fs.existsSync(path.join(s.state, "logs", "polite.log")), "logs stay for the artifact");
  assert.deepEqual(JSON.parse((await s.run("ps", "--json")).out).map((r) => r.state), ["created", "created"]);
});

test("restart replaces the process; exec runs with the service's environment only", async () => {
  const s = stack(`[
    { name: "svc", cmd: "sleep", args: ["30"], env: { GREETING: "from-the-table" } },
  ]`);
  assert.equal((await s.run("up")).code, 0);
  const [before] = pids(s.state);
  assert.equal((await s.run("restart", "svc")).code, 0);
  const [afterPid] = pids(s.state);
  assert.notEqual(before, afterPid);
  assert.ok(!groupAlive(before));
  const outFile = path.join(s.root, "env.txt");
  process.env.NATIVE_STACK_TEST_LEAK = "leaked";
  const code = await s.run("exec", "svc", "--", "sh", "-c", `printf '%s|%s' "$GREETING" "\${NATIVE_STACK_TEST_LEAK:-}" > ${outFile}`);
  delete process.env.NATIVE_STACK_TEST_LEAK;
  assert.equal(code.code, 0);
  assert.equal(fs.readFileSync(outFile, "utf8"), "from-the-table|");
  assert.equal((await s.run("exec", "svc", "--", "sh", "-c", "exit 5")).code, 5);
  await s.run("down");
});

test("setup runs once per data directory; files are written before every start", async () => {
  const s = stack(`[
    { name: "db", cmd: "sleep", args: ["30"], setup: "echo once >> setup.log",
      files: [{ path: ctx.data("db") + "/conf", content: "v1", mode: 0o600 }] },
  ]`);
  await s.run("up");
  await s.run("restart", "db");
  const data = path.join(s.state, "data", "db");
  assert.equal(fs.readFileSync(path.join(data, "setup.log"), "utf8"), "once\n");
  assert.equal(fs.statSync(path.join(data, "conf")).mode & 0o777, 0o600);
  await s.run("down", "-v");
  await s.run("up");
  assert.equal(fs.readFileSync(path.join(data, "setup.log"), "utf8"), "once\n", "after down -v it runs again, once");
  await s.run("down", "-v");
});

test("the table is validated: unknown dependency, bad condition, a cycle, a healthy wait on no check", () => {
  const svc = (name, dependsOn = [], extra = {}) => ({ name, cmd: "true", dependsOn, ...extra });
  assert.throws(() => validate([svc("a", [{ name: "nope", condition: "started" }])]), /unknown service nope/);
  assert.throws(() => validate([svc("a"), svc("b", [{ name: "a", condition: "ready" }])]), /unknown condition ready/);
  assert.throws(() => validate([svc("a", [{ name: "b", condition: "started" }]), svc("b", [{ name: "a", condition: "started" }])]), /dependency cycle: a -> b -> a/);
  assert.throws(() => validate([svc("a"), svc("b", [{ name: "a", condition: "healthy" }])]), /no health check/);
  assert.throws(() => validate([svc("a"), svc("b", [{ name: "a", condition: "completed" }])]), /not a one-shot/);
  assert.deepEqual(
    topoSort([svc("web", [{ name: "main", condition: "started" }]), svc("main", [{ name: "db", condition: "started" }]), svc("db")]).map((s) => s.name),
    ["db", "main", "web"],
  );
});

test("up refuses when a table hostname does not resolve to its address", async () => {
  const s = stack(`[{ name: "x", ip: "127.0.0.42", hostnames: ["localhost"], cmd: "sleep", args: ["1"] }]`);
  const out = [];
  const err = [];
  const code = await main(["--table", path.join(s.root, "table.mjs"), "up"], { env: { ...s.env, E2E_NATIVE_SKIP_NAME_CHECK: "" }, out: (l) => out.push(l), err: (l) => err.push(l) });
  assert.equal(code, 1);
  assert.match(err.join("\n"), /localhost -> 127\.0\.0\.1, want 127\.0\.0\.42[\s\S]*hostAliases/);
  assert.ok(!fs.existsSync(path.join(s.state, "run", "x.json")), "nothing started");
});

test("logs --tail 0 prints no history, as compose's does", async () => {
  const s = stack(`[
    { name: "talker", cmd: "sh", args: ["-c", "echo one; echo two; sleep 30"] },
  ]`);
  assert.equal((await s.run("up")).code, 0);
  await new Promise((r) => setTimeout(r, 400));
  assert.equal((await s.run("logs", "talker", "--tail", "0")).out, "");
  assert.match((await s.run("logs", "talker", "--tail", "1")).out, /talker \| two/);
  await s.run("down");
});

test("exec passes everything after -- to the command, --table included", async () => {
  const s = stack(`[
    { name: "svc", cmd: "sleep", args: ["30"] },
  ]`);
  assert.equal((await s.run("up")).code, 0);
  const outFile = path.join(s.root, "args.txt");
  const r = await s.run("exec", "svc", "--", "sh", "-c", `printf '%s ' "$@" > ${outFile}`, "sh", "--table", "foo");
  assert.equal(r.code, 0, r.err);
  assert.equal(fs.readFileSync(outFile, "utf8"), "--table foo ");
  await s.run("down");
});

test("down -v removes the stack's short socket directory; each state dir has its own", async () => {
  assert.notEqual(shortRoot("/a/state"), shortRoot("/b/state"));
  const s = stack(`[
    { name: "svc", cmd: "sleep", args: ["30"] },
  ]`);
  assert.equal((await s.run("up")).code, 0);
  const dir = path.join(shortRoot(s.state), "pg");
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, ".s.PGSQL.5432.lock"), "stale");
  assert.equal((await s.run("down", "-v")).code, 0);
  assert.ok(!fs.existsSync(shortRoot(s.state)));
});

test("a zombie leader's group is found from /proc, without pgrep", () => {
  const proc = fs.mkdtempSync(path.join(os.tmpdir(), "nsproc-"));
  roots.push(proc);
  const add = (pid, comm, state, pgrp) => {
    fs.mkdirSync(path.join(proc, String(pid)));
    fs.writeFileSync(path.join(proc, String(pid), "stat"), `${pid} (${comm}) ${state} 1 ${pgrp} ${pgrp} 0 -1\n`);
  };
  add(100, "node", "Z", 100);
  add(101, "nginx: worker (x)", "S", 100);
  add(102, "other", "S", 200);
  add(103, "dead", "Z", 100);
  fs.mkdirSync(path.join(proc, "self"));
  assert.deepEqual(groupMembers(100, proc).sort(), [101]);
  assert.deepEqual(groupMembers(300, proc), []);
});
