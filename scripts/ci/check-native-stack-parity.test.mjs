// node --test scripts/ci/check-native-stack-parity.test.mjs
import assert from "node:assert/strict";
import { test } from "node:test";

import standaloneFull, { overlaysOf, rewrite } from "../../deploy/native/standalone-full.mjs";
import { validate } from "../e2e/native-stack.mjs";
import { check, composeDeps, envKeys, main, mergeCompose, nameMap, sameValue, wildcardBinds } from "./check-native-stack-parity.mjs";

const compose = () => ({
  services: {
    db: { restart: "unless-stopped", environment: { POSTGRES_USER: "u" }, healthcheck: { test: ["CMD", "true"] } },
    init: { restart: "no", depends_on: { db: { condition: "service_healthy" } } },
    app: {
      environment: ["DATABASE_URL=postgres://db", "FLAG=1"],
      depends_on: { init: { condition: "service_completed_successfully" }, db: { condition: "service_started" } },
      healthcheck: { test: ["CMD", "/app", "-healthcheck"] },
    },
  },
});
const table = () => ({
  services: [
    { name: "db", ip: "127.0.0.2", hostnames: ["db"], env: { POSTGRES_USER: "u" }, health: { kind: "pg" } },
    { name: "init", init: true, dependsOn: [{ name: "db", condition: "healthy" }] },
    { name: "app", env: { DATABASE_URL: "postgres://db", FLAG: "1" }, dependsOn: [{ name: "db", condition: "started" }, { name: "init", condition: "completed" }], health: { kind: "cmd" } },
  ],
});
const names = { db: "127.0.0.2" };

test("a table that matches its compose file passes", () => {
  assert.deepEqual(check({ compose: compose(), table: table(), names }).errors, []);
});

test("a compose variable missing natively fails, naming the service and the key", () => {
  const c = compose();
  c.services.app.environment.push("NEW_SETTING=on");
  assert.deepEqual(check({ compose: c, table: table(), names }).errors, [
    "app: compose sets NEW_SETTING, the native table does not (add it, or list it in EXEMPT_ENV with the reason it does not apply natively)",
  ]);
});

test("an exemption with a reason lets a compose variable be absent; a stale or reasonless one fails", () => {
  const c = compose();
  c.services.app.environment.push("CONTAINER_ONLY=1");
  assert.deepEqual(check({ compose: c, table: table(), names, exempt: { app: { CONTAINER_ONLY: "no container natively" } } }).errors, []);
  assert.match(check({ compose: c, table: table(), names, exempt: { app: { CONTAINER_ONLY: " " } } }).errors.join("\n"), /EXEMPT_ENV CONTAINER_ONLY has no reason/);
  assert.match(check({ compose: compose(), table: table(), names, exempt: { app: { GONE: "why" } } }).errors.join("\n"), /EXEMPT_ENV lists GONE, which the compose file no longer sets/);
  assert.match(check({ compose: compose(), table: table(), names, exempt: { app: { FLAG: "why" } } }).errors.join("\n"), /FLAG is exempted \(EXEMPT_ENV\) but the native table sets it/);
});

test("a native-only variable needs its reason, and the list cannot go stale", () => {
  const t = table();
  t.services[2].env.BIND = "127.0.0.1:8080";
  assert.match(check({ compose: compose(), table: t, names }).errors.join("\n"), /the native table sets BIND, which the compose file does not/);
  assert.deepEqual(check({ compose: compose(), table: t, names, nativeOnly: { app: { BIND: "the listen address" } } }).errors, []);
  assert.match(check({ compose: compose(), table: table(), names, nativeOnly: { app: { BIND: "x" } } }).errors.join("\n"), /NATIVE_ONLY_ENV lists BIND, which the native table does not set/);
  assert.match(check({ compose: compose(), table: table(), names, nativeOnly: { app: { FLAG: "x" } } }).errors.join("\n"), /NATIVE_ONLY_ENV lists FLAG, which the compose file sets too/);
});

test("depends_on drift, a lost healthcheck and a one-shot that is not init all fail", () => {
  const t = table();
  t.services[2].dependsOn[0].condition = "healthy";
  t.services[0].health = undefined;
  t.services[1].init = false;
  const errors = check({ compose: compose(), table: t, names }).errors.join("\n");
  assert.match(errors, /app: depends_on differs; compose \[db:started, init:completed\], native \[db:healthy, init:completed\]/);
  assert.match(errors, /db: compose has a healthcheck, the native entry none/);
  assert.match(errors, /init: a one-shot in compose \(restart: "no"\), not init: true natively/);
});

test("a missing or extra service fails, and an address must match the pod's name map", () => {
  const t = table();
  t.services.push({ name: "extra" });
  t.services[0].ip = "127.0.0.9";
  const c = compose();
  c.services.cache = {};
  const errors = check({ compose: c, table: t, names }).errors.join("\n");
  assert.match(errors, /cache: in the compose file, not in the native table/);
  assert.match(errors, /extra: in the native table, not in the compose file/);
  assert.match(errors, /db: binds 127\.0\.0\.9, but the pod's name map gives db 127\.0\.0\.2/);
  assert.match(check({ compose: compose(), table: table(), names: {} }).errors.join("\n"), /hostname db is not in the pod's name map/);
});

test("the readers: env lists and maps, depends_on lists and maps, the README's name map", () => {
  assert.deepEqual(envKeys(["A=1", "B"]), ["A", "B"]);
  assert.deepEqual(envKeys({ A: 1 }), ["A"]);
  assert.deepEqual(composeDeps(["b", "a"]), ["a:started", "b:started"]);
  assert.deepEqual(composeDeps({ a: {} }), ["a:started"]);
  const readme = "## Native stack name map\n\n| Address | Names |\n|---|---|\n| 127.0.0.1 | `elitea-main`, `oidc.localhost` |\n| 127.0.0.2 | `postgres` |\n\n## Next\n| 127.0.0.9 | `not-this` |\n";
  assert.deepEqual(nameMap(readme), { "elitea-main": "127.0.0.1", "oidc.localhost": "127.0.0.1", postgres: "127.0.0.2" });
});

test("the repository's e2e-standalone table matches its compose file", async () => {
  const out = [];
  const err = [];
  assert.equal(await main({ out: (l) => out.push(l), err: (l) => err.push(l) }), 0, err.join("\n"));
  assert.match(out.join("\n"), /e2e-standalone\.mjs matches deploy\/docker-compose\.e2e-standalone\.yml: 12 service/);
  // The standalone-full table: the base and the Rust agent overlay merged, then each optional overlay.
  assert.match(out.join("\n"), /standalone-full\.mjs matches deploy\/docker-compose\.standalone-full\.yml \+ deploy\/docker-compose\.standalone-rust-agent\.yml: 34 service/);
  assert.match(out.join("\n"), /STANDALONE_OVERLAY=deploy\/docker-compose\.native-conformance\.yml matches/);
  assert.match(out.join("\n"), /STANDALONE_OVERLAY=deploy\/docker-compose\.handoff-live\.yml matches/);
});

test("an overlay merges as compose merges it: environment and depends_on per key, command replaced, a new service added", () => {
  const base = { services: { w: { command: ["a"], environment: ["A=1", "B=2"], depends_on: ["db"] } } };
  const over = { services: { w: { command: ["b"], environment: { B: "3", C: "4" }, depends_on: { x: { condition: "service_healthy" } } }, x: { restart: "no" } } };
  const m = mergeCompose(base, over);
  assert.deepEqual(m.services.w.command, ["b"]);
  assert.deepEqual(m.services.w.environment, { A: "1", B: "3", C: "4" });
  assert.deepEqual(composeDeps(m.services.w.depends_on), ["db:started", "x:healthy"]);
  assert.deepEqual(m.services.x, { restart: "no" });
});

test("a service absent natively needs a reason, drops its edges, and cannot go stale", () => {
  const c = compose();
  c.services.real = { profiles: ["x"] };
  c.services.app.depends_on.real = { condition: "service_healthy" };
  assert.match(check({ compose: c, table: table(), names }).errors.join("\n"), /real: in the compose file, not in the native table/);
  assert.deepEqual(check({ compose: c, table: table(), names, absent: { real: "no binary on the pool" } }).errors, []);
  assert.match(check({ compose: c, table: table(), names, absent: { real: " " } }).errors.join("\n"), /real: NATIVE_ABSENT has no reason/);
  assert.match(check({ compose: compose(), table: table(), names, absent: { real: "why" } }).errors.join("\n"), /real: NATIVE_ABSENT lists it, but the compose file has no such service/);
  assert.match(check({ compose: compose(), table: table(), names, absent: { app: "why" } }).errors.join("\n"), /app: NATIVE_ABSENT lists it, but the native table runs it/);
});

test("the standalone-full table refuses what it has no native counterpart for", () => {
  const ctx = (env) => ({ repo: process.cwd(), state: "/s", data: (s) => `/s/data/${s}`, short: (t) => `/tmp/ns-test/${t}`, bin: "/b", env });
  assert.equal(standaloneFull(ctx({})).services.length, 34);
  // The supervisor accepts it, and every rendered file renders from the repository's sources (each rewrite
  // throws when its source stopped containing what it rewrites).
  validate(standaloneFull(ctx({})).services);
  const rendered = standaloneFull(ctx({})).services.flatMap((svc) => (svc.files ?? []).map((f) => [svc.name, typeof f.content === "function" ? f.content() : f.content]));
  assert.ok(rendered.length >= 10, `rendered ${rendered.length} file(s)`);
  const worker = Object.fromEntries(rendered)["elitea-worker"];
  assert.doesNotMatch(worker, /"\/run\/|"\/var\/lib/);
  assert.equal(JSON.parse(worker).nats_url, "nats://nats:4222");
  assert.throws(() => standaloneFull(ctx({ STANDALONE_PORT: "8084" })), /STANDALONE_PORT must be 8080 natively/);
  assert.throws(() => standaloneFull(ctx({ COMPOSE_PROFILES: "real-llm" })), /real-llm has no native counterpart/);
  assert.throws(() => overlaysOf("deploy/docker-compose.sandbox.yml", "/r"), /has no native delta/);
  assert.deepEqual(overlaysOf("/r/deploy/docker-compose.handoff-live.yml", "/r"), ["deploy/docker-compose.handoff-live.yml"]);
  const main = (env) => standaloneFull(ctx(env)).services.find((s) => s.name === "elitea-main").env;
  assert.equal(main({ STANDALONE_OVERLAY: "deploy/docker-compose.handoff-live.yml" }).S3_PRESIGN, "false");
  assert.equal(main({}).S3_PRESIGN, undefined);
  const gw = standaloneFull(ctx({})).services.find((s) => s.name === "elitea-llm-gateway").env;
  assert.match(gw.GATEWAY_EGRESS_ALLOWLIST, /,127\.0\.0\.0\/8$/);
  assert.equal(rewrite("a=1\n", [[/^a=1$/m, "a=2"]], "x"), "a=2\n");
  assert.throws(() => rewrite("b\n", [["a", "c"]], "the source"), /the source no longer contains a/);
});

test("the floors fail a reader that found too little", async () => {
  const err = [];
  const code = await main({ tables: [{ table: "deploy/native/e2e-standalone.mjs", minServices: 999, minEnvKeys: 1 }], out: () => {}, err: (l) => err.push(l) });
  assert.equal(code, 1);
  assert.match(err.join("\n"), /read 12 service\(s\), expected at least 999/);
});

test("a compose value the native table does not carry fails, unless REWRITTEN_ENV says why", () => {
  const c = compose();
  c.services.app.environment = ["DATABASE_URL=postgres://db", "FLAG=2"];
  assert.deepEqual(check({ compose: c, table: table(), names }).errors, [
    'app: FLAG is "2" in compose, "1" natively (carry the compose value, or list it in REWRITTEN_ENV with the reason)',
  ]);
  assert.deepEqual(check({ compose: c, table: table(), names, rewritten: { app: { FLAG: "natively off" } } }).errors, []);
  assert.deepEqual(check({ compose: c, table: table(), names, rewritten: { app: { FLAG: " " } } }).errors, ["app: REWRITTEN_ENV FLAG has no reason"]);
  // A listed key whose values agree again is stale.
  assert.deepEqual(check({ compose: compose(), table: table(), names, rewritten: { app: { FLAG: "natively off" } } }).errors, [
    "app: REWRITTEN_ENV lists FLAG, but the native value is compose's; drop it from the list",
  ]);
});

test("the mechanical rewrites need no entry, and nothing wider passes", () => {
  assert.ok(sameValue("${X:-on}", "on"));
  assert.ok(sameValue("${X-}", ""));
  assert.ok(sameValue("/certs/ca.crt", "/home/runner/_work/deploy/certs/ca.crt"));
  assert.ok(sameValue(":9443", "127.0.0.5:9443"));
  assert.ok(sameValue("0.0.0.0:9470", "127.0.0.12:9470"));
  assert.ok(!sameValue("${X:-on}", "off"));
  assert.ok(!sameValue("/certs/ca.crt", "/certs/other.crt"));
  assert.ok(!sameValue(":9443", "127.0.0.5:9444"));
  assert.ok(!sameValue(":9443", "10.0.0.5:9443"));
  assert.ok(!sameValue("a,b", "a,b,c"));
});

test("every native bind is loopback: a wildcard or bare :port fails, wherever the table spells it", () => {
  const bad = [
    [{ env: { RUSTFS_ADDRESS: ":9000" } }, "env RUSTFS_ADDRESS=:9000"],
    [{ env: { GATEWAY_HTTP_ADDR: "0.0.0.0:8083" } }, "env GATEWAY_HTTP_ADDR=0.0.0.0:8083"],
    [{ env: { MOCK_LLM_HOST: "0.0.0.0" } }, "env MOCK_LLM_HOST=0.0.0.0"],
    [{ env: { WHATEVER: "[::]:80" } }, "env WHATEVER=[::]:80"],
    [{ ip: "0.0.0.0" }, "ip 0.0.0.0"],
    [{ args: ["-a", "0.0.0.0", "-p", "4222"] }, "arg -a 0.0.0.0"],
    [{ args: ["--host=::"] }, "arg --host=::"],
    [{ args: ["-c", "HTTPServer(('0.0.0.0', 4318), H)"] }, "arg HTTPServer(('0.0.0.0'"],
    [{ files: [{ path: "/d/default.conf", content: () => "server {\n  listen 80;\n}" }] }, "file default.conf: listen 80;"],
    [{ files: [{ path: "/d/nats.conf", content: "http: 8222\n" }] }, "file nats.conf: http: 8222"],
    [{ files: [{ path: "/d/pgbouncer.ini", content: "listen_addr = 0.0.0.0\n" }] }, "file pgbouncer.ini: listen_addr = 0.0.0.0"],
  ];
  for (const [entry, want] of bad) assert.ok(wildcardBinds(entry).some((w) => w.startsWith(want)), `${JSON.stringify(entry.env ?? entry.args ?? entry.ip ?? want)} -> ${wildcardBinds(entry)}`);
  const good = {
    ip: "127.0.0.4",
    env: { RUSTFS_ADDRESS: "127.0.0.1:9000", QDRANT__SERVICE__HOST: "127.0.0.9", SMTP_HOST: "", S3_ENDPOINT_URL: "http://127.0.0.1:9000", MOCK_MCP_PORT: "8443" },
    args: ["-a", "127.0.0.3", "--host", "127.0.0.1", "-c", "HTTPServer(('127.0.0.23', 4318), H)"],
    files: [{ path: "/d/default.conf", content: "listen 127.0.0.7:8080;\nhttp: 127.0.0.3:8222\nlisten_addr = 127.0.0.8\n" }],
  };
  assert.deepEqual(wildcardBinds(good), []);
  // check() reports it by service.
  const t = table();
  t.services[2].env.APP_ADDR = ":8080";
  const c = compose();
  c.services.app.environment.push("APP_ADDR=:8080");
  assert.deepEqual(check({ compose: c, table: t, names }).errors, [
    "app: listens on a non-loopback address (env APP_ADDR=:8080); bind its own 127.0.0.x: the pool is a shared cluster",
  ]);
});

test("the rustfs endpoint rewrite is mechanical, and only that one", () => {
  assert.ok(sameValue("http://rustfs:9000", "http://127.0.0.1:9000"));
  assert.ok(!sameValue("http://rustfs:9000", "http://127.0.0.2:9000"));
  assert.ok(!sameValue("http://qdrant:6333", "http://127.0.0.1:6333"));
});
