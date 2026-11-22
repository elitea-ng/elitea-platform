#!/usr/bin/env node
// A native stack table runs the same services as its compose file.
//
// The pool runs a compose stack as processes (scripts/e2e/native-stack.mjs over a table in deploy/native/).
// The table is written by hand from the compose file, and a hand-kept copy drifts: a variable added to the
// compose file and not to the table would leave the native legs running a service without it, and nothing
// would say so. This fails, naming the service and the key, when:
//
//   * a compose service has no table entry, or the table has a service compose does not;
//   * a native service would listen on a non-loopback address: its address, a bind-shaped env value
//     (`*_ADDR`, `*_ADDRESS`, `*_HOST`, `*_LISTEN`, `*_BIND`), an argument or a config file it is given names
//     0.0.0.0, ::, * or a bare :port. The pool is a shared cluster: a CI service with dev credentials must
//     never listen on the pod's network address (wildcardBinds below; native-stack.mjs checks the sockets
//     themselves after `up`, for binds a program hard-codes);
//   * a key both set carries a different VALUE natively, unless the difference is one of the mechanical
//     rewrites (sameValue below: compose interpolation resolved to its default, a container path moved to a
//     host path with the same file name, a `:port` or `0.0.0.0:port` listen address bound to a loopback address on that port)
//     or the table's REWRITTEN_ENV lists the key with a reason (and, as with the other lists, a listed key
//     whose values now agree fails, so the list cannot go stale);
//   * a compose `environment:` key is missing from the entry's env, unless the table's EXEMPT_ENV lists it
//     with a reason; or the entry's env has a key compose does not, unless NATIVE_ONLY_ENV lists it with a
//     reason (an exemption whose key left the compose file, or that the table sets anyway, fails too: the
//     lists cannot go stale);
//   * the `depends_on` edges differ (service and condition: service_healthy is "healthy",
//     service_completed_successfully "completed", service_started or a list entry "started");
//   * compose has a healthcheck and the entry has none, or the reverse;
//   * a one-shot (`restart: "no"`) is not `init: true` natively, or the reverse;
//   * a table address disagrees with the pod's name map (deploy/ci-runners/README.md, "Native stack name
//     map"), which is what the hostnames resolve to on the pool;
//   * it reads fewer services or keys than its floors: a reader that found nothing must not pass.
//
// A table may also export COMPOSE_OVERLAYS (files its stack script always adds: compared against the MERGE,
// as compose merges them: `environment:`, `depends_on:` and `healthcheck:` per key, anything else replaced,
// a new service added), OVERLAYS (optional overlay files, each with its native delta: the table built with
// STANDALONE_OVERLAY=<file> is compared against the merge with that file too), and NATIVE_ABSENT (compose
// services with no native process, each with its reason: their depends_on edges are dropped before the
// comparison, and an entry for a service the compose files no longer have, or that the table runs, fails).
//
// Run with the runner's node after `npm ci --prefix scripts/ci` (the `yaml` package).

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

import { isEntrypoint, loadYaml } from "./ci-compat.mjs";

const SELF = fileURLToPath(import.meta.url);
const ROOT = path.resolve(path.dirname(SELF), "..", "..");

/** The native tables and the floors each must clear. */
export const TABLES = [
  { table: "deploy/native/e2e-standalone.mjs", minServices: 10, minEnvKeys: 80 },
  { table: "deploy/native/standalone-full.mjs", minServices: 30, minEnvKeys: 240 },
];
export const README = "deploy/ci-runners/README.md";

const CONDITION = { service_healthy: "healthy", service_completed_successfully: "completed", service_started: "started" };

/** compose `environment:` as a key list (map or KEY=value list). */
export function envKeys(environment) {
  if (!environment) return [];
  if (Array.isArray(environment)) return environment.map((e) => String(e).split("=")[0]);
  return Object.keys(environment);
}

/** compose `depends_on` as sorted "name:condition" strings. */
export function composeDeps(dependsOn) {
  if (!dependsOn) return [];
  const pairs = Array.isArray(dependsOn) ? dependsOn.map((n) => [n, "started"]) : Object.entries(dependsOn).map(([n, v]) => [n, CONDITION[v?.condition ?? "service_started"] ?? `unknown(${v?.condition})`]);
  return pairs.map(([n, c]) => `${n}:${c}`).sort();
}

/**
 * Endpoints a native client reaches somewhere other than the service's name-map address, with the reason;
 * sameValue treats the rewrite as mechanical.
 */
export const ENDPOINT_REWRITES = [
  {
    from: "//rustfs:9000",
    to: "//127.0.0.1:9000",
    why: "rustfs binds 127.0.0.1 only; its server-address check rejects 127.0.0.x aliases (RUSTFS_BIND in deploy/native/e2e-standalone.mjs)",
  },
];

/** compose `${VAR:-default}` / `${VAR-default}` / `${VAR}` with an empty environment, as compose reads it. */
export function interpolate(value) {
  return String(value ?? "").replace(/\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?-([^}]*))?\}/g, (_, _name, def) => def ?? "");
}

/**
 * Whether a native value is compose's value up to a mechanical rewrite: compose interpolation resolved with an
 * empty environment; an absolute container path moved to an absolute host path ending in the same file name; a
 * `:port` listen address bound to a loopback address on the same port.
 */
export function sameValue(composeValue, nativeValue) {
  let c = interpolate(composeValue);
  const n = String(nativeValue ?? "");
  if (c === n) return true;
  for (const r of ENDPOINT_REWRITES) c = c.split(r.from).join(r.to);
  if (c === n) return true;
  if (/^\/[^\s,;]*$/.test(c) && /^\/[^\s,;]*$/.test(n) && path.posix.basename(c) === path.posix.basename(n)) return true;
  const port = /^(?::|0\.0\.0\.0:)(\d+)$/.exec(c);
  if (port && new RegExp(`^127\\.\\d+\\.\\d+\\.\\d+:${port[1]}$`).test(n)) return true;
  return false;
}

const WILDCARD = /^(?:0\.0\.0\.0|::|\[::\]|\*)?(?::\d+)?$/;
const BIND_KEY = /(?:^|_)(?:ADDR|ADDRESS|HOST|LISTEN|BIND)$/;

/** Where a native service entry would listen on a non-loopback address: a list of "where: what" strings. */
export function wildcardBinds(entry) {
  const found = [];
  if (entry.ip !== undefined && !/^127\.\d+\.\d+\.\d+$/.test(String(entry.ip))) found.push(`ip ${entry.ip}`);
  for (const [k, v] of Object.entries(entry.env ?? {})) {
    const value = String(v ?? "");
    if (value === "") continue;
    if ((BIND_KEY.test(k) && WILDCARD.test(value)) || /^(?:0\.0\.0\.0|\[::\])(?::\d+)?$/.test(value)) found.push(`env ${k}=${value}`);
  }
  const args = (entry.args ?? []).map(String);
  args.forEach((a, i) => {
    const flag = /^-{1,2}(?:a|addr|address|host|bind|listen)(?:=(.*))?$/.exec(a);
    const value = flag ? (flag[1] ?? args[i + 1] ?? "") : a;
    if ((flag && WILDCARD.test(value) && value !== "") || /(?:^|[^\d.])0\.0\.0\.0(?![\d.])|\[::\]/.test(a)) found.push(`arg ${a}${flag && flag[1] === undefined ? ` ${value}` : ""}`);
  });
  for (const f of entry.files ?? []) {
    let text = "";
    try {
      text = typeof f.content === "function" ? String(f.content()) : String(f.content ?? "");
    } catch {
      continue;
    }
    for (const line of text.split("\n")) {
      const l = line.trim();
      if (/^listen\s+(?:\d+|\*:\d+|\[::\]:\d+|0\.0\.0\.0:\d+)(?:[;\s]|$)/.test(l) || /(?:^|[^\d.])0\.0\.0\.0(?![\d.])/.test(l) || /^(?:listen|http|listen_addr)\s*[:=]\s*(?:\*|::)?(?::?\d+)?\s*$/.test(l))
        found.push(`file ${path.basename(f.path ?? "?")}: ${l}`);
    }
  }
  return found;
}

const asMap = (environment) =>
  Array.isArray(environment) ? Object.fromEntries(environment.map((e) => [String(e).split("=")[0], String(e).split("=").slice(1).join("=")])) : { ...(environment ?? {}) };
const depsMap = (dependsOn) => (Array.isArray(dependsOn) ? Object.fromEntries(dependsOn.map((n) => [n, { condition: "service_started" }])) : { ...(dependsOn ?? {}) });

/** compose files merged as compose merges them, for the keys this check reads. */
export function mergeCompose(...docs) {
  const services = {};
  for (const doc of docs) {
    for (const [name, svc] of Object.entries(doc?.services ?? {})) {
      const base = services[name];
      if (!base) {
        services[name] = { ...svc };
        continue;
      }
      const merged = { ...base, ...svc };
      if (base.environment || svc.environment) merged.environment = { ...asMap(base.environment), ...asMap(svc.environment) };
      if (base.depends_on || svc.depends_on) merged.depends_on = { ...depsMap(base.depends_on), ...depsMap(svc.depends_on) };
      if (base.healthcheck && svc.healthcheck) merged.healthcheck = { ...base.healthcheck, ...svc.healthcheck };
      services[name] = merged;
    }
  }
  return { services };
}

/** The README's name map: hostname -> address. */
export function nameMap(readme) {
  const start = readme.indexOf("## Native stack name map");
  if (start < 0) return {};
  const section = readme.slice(start, readme.indexOf("\n## ", start + 1) > 0 ? readme.indexOf("\n## ", start + 1) : undefined);
  const out = {};
  for (const m of section.matchAll(/^\|\s*(127\.\d+\.\d+\.\d+)\s*\|([^|]+)\|/gm)) {
    for (const n of m[2].matchAll(/`([^`]+)`/g)) out[n[1]] = m[1];
  }
  return out;
}

/**
 * @param {{compose: any, table: {services: any[]}, nativeOnly?: object, exempt?: object, names?: object}} input
 * @returns {{errors: string[], services: number, keys: number}}
 */
export function check({ compose, table, nativeOnly = {}, exempt = {}, names = {}, absent = {}, rewritten = {} }) {
  const errors = [];
  const native = new Map((table?.services ?? []).map((s) => [s.name, s]));
  for (const [name, why] of Object.entries(absent)) {
    if (!(name in (compose?.services ?? {}))) errors.push(`${name}: NATIVE_ABSENT lists it, but the compose file has no such service; drop it`);
    else if (native.has(name)) errors.push(`${name}: NATIVE_ABSENT lists it, but the native table runs it; drop it from the list`);
    else if (!String(why ?? "").trim()) errors.push(`${name}: NATIVE_ABSENT has no reason`);
  }
  const composeServices = Object.fromEntries(
    Object.entries(compose?.services ?? {})
      .filter(([name]) => !(name in absent))
      .map(([name, svc]) => {
        if (!svc.depends_on) return [name, svc];
        const deps = depsMap(svc.depends_on);
        for (const gone of Object.keys(absent)) delete deps[gone];
        return [name, { ...svc, depends_on: deps }];
      }),
  );
  let keys = 0;
  // REWRITTEN_ENV keys both sides set in this variant; main() fails one that no variant sets (an overlay may be
  // the only file that sets it).
  const rewrittenSeen = new Set();
  for (const [name, svc] of Object.entries(composeServices)) {
    const entry = native.get(name);
    if (!entry) {
      errors.push(`${name}: in the compose file, not in the native table`);
      continue;
    }
    const want = new Set(envKeys(svc.environment));
    const have = new Set(Object.keys(entry.env ?? {}));
    const ex = exempt[name] ?? {};
    const only = nativeOnly[name] ?? {};
    keys += want.size;
    const values = asMap(svc.environment);
    const rw = rewritten[name] ?? {};
    for (const k of want) {
      if (have.has(k)) {
        if (k in ex) errors.push(`${name}: ${k} is exempted (EXEMPT_ENV) but the native table sets it; drop the exemption`);
        const same = sameValue(values[k], entry.env[k]);
        if (!same && !(k in rw)) errors.push(`${name}: ${k} is ${JSON.stringify(interpolate(values[k]))} in compose, ${JSON.stringify(String(entry.env[k]))} natively (carry the compose value, or list it in REWRITTEN_ENV with the reason)`);
        else if (!same && !String(rw[k] ?? "").trim()) errors.push(`${name}: REWRITTEN_ENV ${k} has no reason`);
        else if (same && k in rw) errors.push(`${name}: REWRITTEN_ENV lists ${k}, but the native value is compose's; drop it from the list`);
        continue;
      }
      if (!(k in ex)) errors.push(`${name}: compose sets ${k}, the native table does not (add it, or list it in EXEMPT_ENV with the reason it does not apply natively)`);
      else if (!String(ex[k] ?? "").trim()) errors.push(`${name}: EXEMPT_ENV ${k} has no reason`);
    }
    for (const k of Object.keys(ex)) if (!want.has(k)) errors.push(`${name}: EXEMPT_ENV lists ${k}, which the compose file no longer sets; drop it`);
    for (const k of Object.keys(rw)) if (want.has(k) && have.has(k)) rewrittenSeen.add(`${name}/${k}`);
    for (const k of have) {
      if (want.has(k)) {
        if (k in only) errors.push(`${name}: NATIVE_ONLY_ENV lists ${k}, which the compose file sets too; drop it from the list`);
        continue;
      }
      if (!(k in only)) errors.push(`${name}: the native table sets ${k}, which the compose file does not (list it in NATIVE_ONLY_ENV with the reason)`);
      else if (!String(only[k] ?? "").trim()) errors.push(`${name}: NATIVE_ONLY_ENV ${k} has no reason`);
    }
    for (const k of Object.keys(only)) if (!have.has(k)) errors.push(`${name}: NATIVE_ONLY_ENV lists ${k}, which the native table does not set; drop it`);

    const cd = composeDeps(svc.depends_on);
    const nd = (entry.dependsOn ?? []).map((d) => `${d.name}:${d.condition}`).sort();
    if (cd.join(",") !== nd.join(",")) errors.push(`${name}: depends_on differs; compose [${cd.join(", ")}], native [${nd.join(", ")}]`);

    const composeHealth = svc.healthcheck !== undefined && svc.healthcheck?.disable !== true;
    if (composeHealth !== !!entry.health) errors.push(`${name}: ${composeHealth ? "compose has a healthcheck, the native entry none" : "the native entry has a health check, compose none"}`);

    const oneShot = String(svc.restart ?? "") === "no";
    if (oneShot !== !!entry.init) errors.push(`${name}: ${oneShot ? 'a one-shot in compose (restart: "no"), not init: true natively' : "init: true natively, but compose restarts it"}`);

    for (const w of wildcardBinds(entry)) errors.push(`${name}: listens on a non-loopback address (${w}); bind its own 127.0.0.x: the pool is a shared cluster`);

    for (const host of entry.hostnames ?? []) {
      if (!(host in names)) errors.push(`${name}: hostname ${host} is not in the pod's name map (${README})`);
      else if (names[host] !== entry.ip) errors.push(`${name}: binds ${entry.ip}, but the pod's name map gives ${host} ${names[host]} (${README})`);
    }
  }
  for (const name of native.keys()) if (!(name in composeServices)) errors.push(`${name}: in the native table, not in the compose file`);
  for (const name of [...Object.keys(exempt), ...Object.keys(nativeOnly), ...Object.keys(rewritten)]) {
    if (!(name in composeServices)) errors.push(`${name}: an env list names a service the compose file does not have`);
  }
  return { errors, services: Object.keys(composeServices).length, keys, rewrittenSeen };
}

export async function main({ root = ROOT, tables = TABLES, out = console.log, err = console.error } = {}) {
  const names = nameMap(fs.readFileSync(path.join(root, README), "utf8"));
  const errors = [];
  if (Object.keys(names).length < 7) errors.push(`${README}: read ${Object.keys(names).length} name(s) from "Native stack name map"; the table moved or its shape changed`);
  for (const t of tables) {
    const mod = await import(pathToFileURL(path.join(root, t.table)).href);
    const composeFile = mod.COMPOSE_FILE;
    if (!composeFile) {
      errors.push(`${t.table}: exports no COMPOSE_FILE`);
      continue;
    }
    const load = (f) => loadYaml(fs.readFileSync(path.join(root, f), "utf8"));
    const always = mod.COMPOSE_OVERLAYS ?? [];
    const variants = [[null, []], ...Object.keys(mod.OVERLAYS ?? {}).map((o) => [o, [o]])];
    const seen = new Set();
    for (const [overlay, extra] of variants) {
      const files = [composeFile, ...always, ...extra];
      const compose = mergeCompose(...files.map(load));
      const ctx = { repo: root, state: "/state", data: (s) => `/state/data/${s}`, short: (t) => `/tmp/ns-test/${t}`, bin: "/bin", env: overlay ? { STANDALONE_OVERLAY: overlay } : {} };
      const table = mod.default(ctx);
      const r = check({ compose, table, nativeOnly: mod.NATIVE_ONLY_ENV, exempt: mod.EXEMPT_ENV, names, absent: mod.NATIVE_ABSENT, rewritten: mod.REWRITTEN_ENV });
      for (const k of r.rewrittenSeen) seen.add(k);
      const what = files.join(" + ");
      for (const e of r.errors) errors.push(`${t.table} vs ${what}: ${e}`);
      if (overlay !== null) {
        if (!r.errors.length) out(`${t.table} with STANDALONE_OVERLAY=${overlay} matches ${what}`);
        continue;
      }
      if (r.services < t.minServices) errors.push(`${composeFile}: read ${r.services} service(s), expected at least ${t.minServices}`);
      if (r.keys < t.minEnvKeys) errors.push(`${composeFile}: read ${r.keys} environment key(s), expected at least ${t.minEnvKeys}`);
      if (!r.errors.length) out(`${t.table} matches ${what}: ${r.services} service(s), ${r.keys} environment key(s), every depends_on edge, healthcheck and one-shot`);
    }
    for (const [svc, keys] of Object.entries(mod.REWRITTEN_ENV ?? {})) {
      for (const k of Object.keys(keys)) if (!seen.has(`${svc}/${k}`)) errors.push(`${t.table}: REWRITTEN_ENV lists ${svc} ${k}, which no compose variant and the native table both set; drop it`);
    }
  }
  for (const e of errors) err(`::error::${e}`);
  return errors.length ? 1 : 0;
}

if (isEntrypoint(import.meta.url)) process.exit(await main());
