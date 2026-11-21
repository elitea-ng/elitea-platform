// node --test scripts/ci/check-native-stack-parity.test.mjs
import assert from "node:assert/strict";
import { test } from "node:test";

import { check, composeDeps, envKeys, main, nameMap } from "./check-native-stack-parity.mjs";

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
    { name: "app", env: { DATABASE_URL: "x", FLAG: "1" }, dependsOn: [{ name: "db", condition: "started" }, { name: "init", condition: "completed" }], health: { kind: "cmd" } },
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
});

test("the floors fail a reader that found too little", async () => {
  const err = [];
  const code = await main({ tables: [{ table: "deploy/native/e2e-standalone.mjs", minServices: 999, minEnvKeys: 1 }], out: () => {}, err: (l) => err.push(l) });
  assert.equal(code, 1);
  assert.match(err.join("\n"), /read 12 service\(s\), expected at least 999/);
});
