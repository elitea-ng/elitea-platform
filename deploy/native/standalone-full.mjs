// deploy/docker-compose.standalone-full.yml + deploy/docker-compose.standalone-rust-agent.yml as processes on
// the self-hosted pool.
//
// Read by scripts/e2e/native-stack.mjs (the supervisor) when deploy/scripts/standalone-stack.sh runs with
// E2E_STACK=native (apps/elitea-web/scripts/lib/stack-backend.sh). standalone-stack.sh always adds the Rust
// agent overlay, so this table is the MERGE of the two files, as compose merges them (`environment:` and
// `depends_on:` per key, `command:` replaced). Every compose service is here under its compose name, with
// its compose `environment:` verbatim (compose's `${VAR:-default}` read from the caller, as compose reads
// it) except for these rewrites:
//
//   * paths: ./certs and the mounted /run/certs -> deploy/certs; the runtime-material volumes -> that
//     one-shot's data dir (main/, worker/, subapp/, subapp-host/, the same files and modes
//     deploy/runtime/install-material.sh gives each consumer); the engine and extractor socket volumes ->
//     the engine's (extractor's) data dir; /tmp scratch dirs and the images' static dirs -> data and build
//     directories;
//   * binds: each service listens on ITS address from the pod's hostAliases (deploy/ci-runners/README.md,
//     "Native stack name map") at its compose port: ":9443" becomes "127.0.0.1:9443". The four mocks keep
//     the image's 0.0.0.0, so the specs reach them on localhost:<port>, where compose publishes them;
//     mcp-mock binds its own address (MOCK_MCP_HOST), because the gateway holds 8443 on 127.0.0.10;
//   * what a container network gave for free: 127.0.0.0/8 in the gateway's egress allowlist (compose relied
//     on Docker's private ranges); nginx on 8080, not 80; elitea-doc-extractor's `network_mode: none` and
//     its peer uid 10001 -> no address at all (it only ever listens on its Unix socket) and the runner's own
//     uid, which is the uid elitea-worker runs as here.
//
// What does NOT carry over, by design (docs/contributing/ci.md, "Native standalone lanes"):
//   * container isolation: doc-extractor's read_only, cap_drop, no-new-privileges, mem_limit and
//     pids_limit, and every image's non-root uid split (every process is the runner user);
//   * llm-real (the `real-llm` profile): the pool has no llama.cpp server and no GGUFs, and its binary is
//     never lifted from the image (glibc 2.41 on the pool vs the image's). COMPOSE_PROFILES=real-llm is
//     refused here; the real-model lanes stay compose;
//   * the otel-collector image: a stdlib OTLP/HTTP sink stands in on its address and port 4318 (every
//     exporter here is OTLP/HTTP), answering 200 to each export so no exporter logs a dead collector.
//
// Overlays: standalone-stack.sh's STANDALONE_OVERLAY may name only the files in OVERLAYS below; each has its
// native delta here, and scripts/ci/check-native-stack-parity.mjs compares the table built with each one
// against the compose files merged with it. Any other overlay is refused: a delta nobody wrote is not
// applied silently.
//
// The build directory (E2E_NATIVE_BIN) holds what ci-web-e2e-native.yml's two build jobs upload:
//   native-build:      elitea-main, elitea-migrate, elitea-subapp-host, elitea-scheduler, elitea-grpc-health,
//                      grpcurl, web/app/, admin_ui/static/{dist,brand-preview}/  (scripts/e2e/build-native-bin.sh)
//   native-build-rust: elitea-worker-rust, elitea-vector, elitea-doc-extractor, elitea-deepwiki-engine,
//                      elitea-inventory-engine, elitea-llm-gateway             (scripts/e2e/build-native-rust.sh)

import fs from "node:fs";
import path from "node:path";

import { renderSpaConf, RUSTFS_BIND, RUSTFS_ENDPOINT, WEB_PORT } from "./e2e-standalone.mjs";

export const COMPOSE_FILE = "deploy/docker-compose.standalone-full.yml";
/** Applied on top of COMPOSE_FILE every time (standalone-stack.sh's STANDALONE_WORKER=rust). */
export const COMPOSE_OVERLAYS = ["deploy/docker-compose.standalone-rust-agent.yml"];

/** The pod's hostAliases for this stack (deploy/ci-runners/README.md), the source of every bind. */
export const ADDRESSES = {
  "elitea-main": "127.0.0.1",
  "oidc-mock": "127.0.0.1",
  postgres: "127.0.0.2",
  nats: "127.0.0.3",
  rustfs: "127.0.0.4",
  "elitea-deepwiki": "127.0.0.5",
  "elitea-inventory": "127.0.0.6",
  "elitea-web": "127.0.0.7",
  pgbouncer: "127.0.0.8",
  qdrant: "127.0.0.9",
  "elitea-llm-gateway": "127.0.0.10",
  "elitea-scheduler": "127.0.0.11",
  "elitea-vector": "127.0.0.12",
  "elitea-worker": "127.0.0.13",
  "elitea-deepwiki-engine": "127.0.0.15",
  "elitea-inventory-engine": "127.0.0.16",
  "llm-mock": "127.0.0.17",
  "mcp-mock": "127.0.0.19",
  "qtest-mock": "127.0.0.21",
  "ado-mock": "127.0.0.22",
  "otel-collector": "127.0.0.23",
};

/**
 * elitea-main's port. Internal callers name `elitea-main:8080` (the gateway's and main's self-LLM origins,
 * `check`, embedding-path-check.sh), which compose serves inside its network whatever host port it
 * publishes; natively that is the one port, so it is 8080 and nothing else.
 */
export const MAIN_PORT = "8080";

/** Compose services with no native process, and why. The parity check refuses a stale or reasonless entry. */
export const NATIVE_ABSENT = {
  "llm-real":
    "the real-llm profile: the pool image has no llama.cpp server and no GGUFs, and the elitea-e2e-llm image's binary is never lifted (glibc); the real-model lanes stay compose",
};

/** The overlays standalone-stack.sh may add natively: file -> (ctx values) -> { service: env delta }. */
export const OVERLAYS = {
  "deploy/docker-compose.native-conformance.yml": () => ({
    "elitea-main": { DEPLOYMENT_URL: `http://localhost:${MAIN_PORT}` },
  }),
  "deploy/docker-compose.handoff-live.yml": () => ({
    // The 127.0.0.1:19000 publish is not needed: handoff-live.sh reaches S3 at RUSTFS_ENDPOINT natively.
    rustfs: { RUSTFS_SSE_S3_MASTER_KEY: "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=" },
    "elitea-main": { S3_PRESIGN: "false", AWS_REQUEST_CHECKSUM_CALCULATION: "when_required" },
  }),
};

/** Variables only the native table sets, and why. The parity check refuses any other extra. */
export const NATIVE_ONLY_ENV = {
  postgres: {
    PG_SOCKET_DIR: "the Unix socket directory: compose's container has its own /var/run/postgresql",
  },
  rustfs: {
    RUSTFS_ADDRESS: "the bind: the image listens on :9000; natively 127.0.0.1:9000 (RUSTFS_BIND), loopback only",
    RUSTFS_VOLUMES: "the data directory the image keeps at /data",
    RUSTFS_CONSOLE_ENABLE: "the image's console is never published; natively it would bind another port",
    RUSTFS_OBS_LOG_DIRECTORY: "the image writes its logs under /logs; natively under the data dir",
  },
  qdrant: {
    QDRANT__SERVICE__HOST: "the bind: the image listens on 0.0.0.0; natively qdrant's own address",
    QDRANT__STORAGE__STORAGE_PATH: "the image's /qdrant/storage volume, natively the data dir",
    QDRANT__STORAGE__SNAPSHOTS_PATH: "the image's ./snapshots, natively the data dir",
    QDRANT_API_KEY_FILE: "the file compose mounts at /run/qdrant/api-key, read by the same sh -c wrapper",
  },
  "llm-mock": { MOCK_LLM_HOST: "the bind: the image listens on 0.0.0.0; natively its own loopback address" },
  "qtest-mock": { QTEST_MOCK_HOST: "the bind: the image listens on 0.0.0.0; natively its own loopback address" },
  "ado-mock": { ADO_MOCK_HOST: "the bind: the image listens on 0.0.0.0; natively its own loopback address" },
  "mcp-mock": {
    MOCK_MCP_HOST: "the bind: mcp-mock's own address, because the gateway holds 8443 on 127.0.0.10",
    MOCK_MCP_CERT: "the image's /opt/mock-mcp/tls/server-chain.crt, minted natively by setup as its Containerfile's tls stage does",
    MOCK_MCP_KEY: "the image's /opt/mock-mcp/tls/server.key, minted natively by setup",
  },
  "runtime-session-init": {
    CERT: "the worker certificate provision-runtime-session.sh reads (its default is the container's /run/elitea-runtime path)",
  },
  "elitea-main": {
    ELITEA_HTTP_ADDRESS: "the bind: the image listens on :8080 (cmd/elitea-main/http_address.go); natively 127.0.0.1:8080",
  },
  "elitea-web": {
    ELITEA_WEB_CONFIG_JS: "where docker-entrypoint.sh writes config.js (the image's path is /usr/share/nginx/html, root-owned)",
  },
};

/**
 * Compose values the native table changes, and why (the parity check compares values too; the mechanical
 * rewrites, such as a mount path or a listen address, need no entry).
 */
export const REWRITTEN_ENV = {
  postgres: { PGDATA: "the cluster directory under the service's data dir; the image's volume path does not exist natively" },
  "elitea-main": {
    ELITEA_WEB_UPSTREAM: "the non-root nginx listens on 8080 natively (WEB_PORT); the container binds 80",
    DEPLOYMENT_URL: "natively the browser reaches the stack at localhost:8080 (elitea-main's own bind); compose publishes it on 8084",
    OIDC_REDIRECT_URI: "as DEPLOYMENT_URL: localhost:8080 natively, 8084 where compose publishes it",
  },
  "elitea-llm-gateway": { GATEWAY_EGRESS_ALLOWLIST: "127.0.0.0/8 added: natively every service is a loopback address (the pod's name map), which the container networks never were" },
  "elitea-doc-extractor": {
    ELITEA_EXTRACTOR_PEER_UIDS: "the runner's own uid: natively every process runs as it, where compose gives each container its own uid",
  },
  "elitea-deepwiki": { ELITEA_DEEPWIKI_SCRATCH_PATH: "the container's private /tmp; natively a per-service directory under the stack state, so two services never share one" },
  "elitea-deepwiki-engine": { ELITEA_DEEPWIKI_SCRATCH_PATH: "the container's private /tmp; natively a per-service directory under the stack state, so two services never share one" },
  "elitea-inventory": { ELITEA_INVENTORY_SCRATCH_PATH: "the container's private /tmp; natively a per-service directory under the stack state, so two services never share one" },
  "elitea-inventory-engine": { ELITEA_INVENTORY_SCRATCH_PATH: "the container's private /tmp; natively a per-service directory under the stack state, so two services never share one" },
};

/** Compose variables that do not apply natively, and why. Empty: every one has a native value. */
export const EXEMPT_ENV = {};

const need = (cond, msg) => {
  if (!cond) throw new Error(`deploy/native/standalone-full.mjs: ${msg}`);
};

/** Replaces each [from, to]; each `from` must be present, so a drift in the source fails loudly. */
export function rewrite(source, rewrites, what) {
  let out = source;
  for (const [from, to] of rewrites) {
    need(typeof from === "string" ? out.includes(from) : from.test(out), `${what} no longer contains ${from}; update the native rewrite`);
    if (typeof from !== "string") from.lastIndex = 0;
    out = typeof from === "string" ? out.split(from).join(to) : out.replace(from, to);
  }
  return out;
}

/** The overlay files STANDALONE_OVERLAY names, repository-relative; an unknown one is refused. */
export function overlaysOf(raw, repo) {
  const out = [];
  for (const item of String(raw ?? "").split(/\s+/).filter(Boolean)) {
    const rel = path.isAbsolute(item) ? path.relative(repo, item) : item;
    need(rel in OVERLAYS, `STANDALONE_OVERLAY names ${item}, which has no native delta (known: ${Object.keys(OVERLAYS).join(", ")})`);
    out.push(rel);
  }
  return out;
}

export default function table(ctx) {
  const { repo, data, bin, env } = ctx;
  // compose's ${K:-def} (unset or empty -> def) and ${K-def} (unset -> def).
  const d = (k, def) => (env[k] === undefined || env[k] === "" ? def : env[k]);
  const dd = (k, def) => (env[k] === undefined ? def : env[k]);
  need(d("STANDALONE_PORT", MAIN_PORT) === MAIN_PORT, `STANDALONE_PORT must be ${MAIN_PORT} natively (got ${env.STANDALONE_PORT}): internal callers name elitea-main:${MAIN_PORT}`);
  need(!String(env.COMPOSE_PROFILES ?? "").split(",").includes("real-llm"), "COMPOSE_PROFILES=real-llm has no native counterpart (NATIVE_ABSENT llm-real); run that lane on compose");
  const overlays = overlaysOf(env.STANDALONE_OVERLAY, repo);
  const delta = (svc) => Object.assign({}, ...overlays.map((o) => OVERLAYS[o]()[svc] ?? {}));

  const ip = (svc) => ADDRESSES[svc];
  const certs = path.join(repo, "deploy", "certs");
  const cert = (f) => path.join(certs, f);
  const vec = (f) => path.join(certs, "vector", f);
  const material = data("runtime-material");
  const rt = (consumer, f) => path.join(material, consumer, f);
  const oidcPort = d("E2E_OIDC_PORT", "9400");
  const host = d("STANDALONE_HOST", "localhost");
  const spiffe = d("RUNTIME_WORKER_SPIFFE_ID", "spiffe://elitea.standalone/ns/default/sa/agent-worker");
  const pg = data("postgres");
  // A Unix socket path is limited to ~104 bytes (.github/actions/native-services/start.sh).
  const short = (dir, tag) => (path.join(dir, "x.sock").length > 90 ? ctx.short(tag) : dir);
  const pgSocket = short(path.join(pg, "sock"), "pg");
  const deepwikiSock = path.join(short(data("elitea-deepwiki-engine"), "dw"), "engine.sock");
  const inventorySock = path.join(short(data("elitea-inventory-engine"), "inv"), "engine.sock");
  const extractorSock = path.join(short(data("elitea-doc-extractor"), "ext"), "extractor.sock");
  const spool = path.join(data("worker-spool-init"), "output-spool");
  const trust = path.join(data("mcp-mock-trust"), "mcp-mock-ca-bundle.pem");
  const mcpTls = path.join(data("mcp-mock"), "tls");
  const web = data("elitea-web");
  const main = data("elitea-main");
  const read = (...p) => () => fs.readFileSync(path.join(repo, ...p), "utf8");
  const db = "postgres://elitea:elitea@postgres:5432/elitea?sslmode=disable";
  const pyHealth = (url) => ({ kind: "http", url, interval: 3, timeout: 3, retries: 10, startPeriod: 3 });
  const egressDefault = "llm-mock:8090,llm-real:8090,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16";
  const egress = dd("GATEWAY_EGRESS_ALLOWLIST", egressDefault);

  return {
    services: [
      // ── Infrastructure ──────────────────────────────────────────────────
      {
        name: "postgres",
        ip: ip("postgres"),
        hostnames: ["postgres"],
        // The image's first start, as deploy/native/e2e-standalone.mjs does it (initdb, POSTGRES_DB on a
        // socket-only server); compose's `-c max_connections=200`, and the CI-only durability trade.
        setup: [
          'pw="$(mktemp)"; printf "%s\\n" "$POSTGRES_PASSWORD" > "$pw"',
          'loc="--locale=en_US.UTF-8"; locale -a 2>/dev/null | grep -qix "en_US.utf-\\?8" || loc="--locale=C.UTF-8"',
          'initdb -D "$PGDATA" -U "$POSTGRES_USER" --pwfile="$pw" --auth-local=trust --auth-host=scram-sha-256 --encoding=UTF8 $loc',
          'rm -f "$pw"; mkdir -p "$PG_SOCKET_DIR"',
          `pg_ctl -D "$PGDATA" -l "$PGDATA/../setup-server.log" -w -t 60 -o "-c listen_addresses='' -c unix_socket_directories=$PG_SOCKET_DIR" start`,
          '[ "$POSTGRES_DB" = postgres ] || createdb -h "$PG_SOCKET_DIR" -U "$POSTGRES_USER" "$POSTGRES_DB"',
          'pg_ctl -D "$PGDATA" -m fast -w stop',
        ].join("\n"),
        cmd: "postgres",
        args: [
          "-D", path.join(pg, "cluster"),
          "-c", "max_connections=200",
          "-c", `listen_addresses=${ip("postgres")}`,
          "-c", "port=5432",
          "-c", `unix_socket_directories=${pgSocket}`,
          "-c", "fsync=off",
          "-c", "synchronous_commit=off",
          "-c", "full_page_writes=off",
        ],
        env: { POSTGRES_USER: "elitea", POSTGRES_PASSWORD: "elitea", POSTGRES_DB: "elitea", PGDATA: path.join(pg, "cluster"), PG_SOCKET_DIR: pgSocket },
        stop: { cmd: "pg_ctl", args: ["-D", path.join(pg, "cluster"), "-m", "fast", "-w", "-t", "10", "stop"] },
        health: { kind: "pg", host: ip("postgres"), port: 5432, user: "elitea", interval: 5, timeout: 5, retries: 10 },
      },
      {
        // deploy/runtime/pgbouncer.ini on pgbouncer's own address, its userlist from the repository, and no
        // Unix socket (the container's was /tmp, which nothing used).
        name: "pgbouncer",
        ip: ip("pgbouncer"),
        hostnames: ["pgbouncer"],
        cmd: "pgbouncer",
        args: [path.join(data("pgbouncer"), "pgbouncer.ini")],
        files: [
          {
            path: path.join(data("pgbouncer"), "pgbouncer.ini"),
            content: () =>
              rewrite(read("deploy", "runtime", "pgbouncer.ini")(), [
                [/^listen_addr = 0\.0\.0\.0$/m, `listen_addr = ${ip("pgbouncer")}`],
                [/^unix_socket_dir = \/tmp$/m, "unix_socket_dir ="],
                [/^auth_file = \/etc\/pgbouncer\/userlist\.txt$/m, `auth_file = ${path.join(repo, "deploy", "runtime", "pgbouncer-userlist.txt")}`],
              ], "deploy/runtime/pgbouncer.ini"),
          },
        ],
        dependsOn: [{ name: "postgres", condition: "healthy" }],
        health: { kind: "pg", host: ip("pgbouncer"), port: 6432, user: "elitea", interval: 5, timeout: 5, retries: 10 },
      },
      {
        // deploy/runtime/nats-local.conf on nats's own address, its store in the data dir.
        name: "nats",
        ip: ip("nats"),
        hostnames: ["nats"],
        cmd: "nats-server",
        args: ["-c", path.join(data("nats"), "nats.conf")],
        files: [
          {
            path: path.join(data("nats"), "nats.conf"),
            content: () =>
              rewrite(read("deploy", "runtime", "nats-local.conf")(), [
                [/^port: 4222$/m, `listen: ${ip("nats")}:4222`],
                [/^http: 8222$/m, `http: ${ip("nats")}:8222`],
                [/store_dir: "\/data"/, `store_dir: "${path.join(data("nats"), "jetstream")}"`],
              ], "deploy/runtime/nats-local.conf"),
          },
        ],
      },
      {
        name: "nats-health",
        cmd: "sleep",
        args: ["infinity"],
        dependsOn: [{ name: "nats", condition: "started" }],
        health: { kind: "http", url: `http://${ip("nats")}:8222/healthz?js-enabled-only=true`, interval: 5, timeout: 5, retries: 10 },
      },
      {
        name: "nats-bootstrap",
        init: true,
        cmd: "sh",
        args: [path.join(repo, "deploy", "helm", "elitea", "files", "nats-bootstrap.sh")],
        env: { NATS_URL: "nats://nats:4222" },
        dependsOn: [{ name: "nats-health", condition: "healthy" }],
      },
      {
        // deploy/runtime/install-material.sh itself, with its /src and /dst paths pointed at
        // deploy/certs/runtime and this one-shot's data dir and its chown a no-op (every consumer is the
        // runner user here): the same files and modes per consumer, from the one list.
        name: "runtime-material",
        init: true,
        cmd: "sh",
        args: [path.join(material, "install-material.sh")],
        files: [
          {
            path: path.join(material, "install-material.sh"),
            content: () =>
              "chown() { :; }\n" +
              `mkdir -p "${material}/subapp" "${material}/subapp-host"\n` +
              rewrite(read("deploy", "runtime", "install-material.sh")(), [
                [/^SRC=\/src$/m, `SRC=${path.join(certs, "runtime")}`],
                [/=\/dst\//g, `=${material}/`],
              ], "deploy/runtime/install-material.sh"),
          },
        ],
      },
      {
        name: "db-init",
        init: true,
        cmd: "sh",
        args: [
          "-c",
          [
            "set -eu",
            "export PGPASSWORD=elitea",
            "if ! psql -h postgres -U elitea -d postgres -tAc \\",
            "     \"SELECT 1 FROM pg_database WHERE datname = 'agentstate'\" | grep -q 1; then",
            '  psql -v ON_ERROR_STOP=1 -h postgres -U elitea -d postgres -c "CREATE DATABASE agentstate"',
            "fi",
            "if psql -h postgres -U elitea -d elitea -tAc \"SELECT to_regclass('centry.project')\" | grep -q .; then",
            '  echo "centry schema already present; skipping 001_initial.sql"',
            "  exit 0",
            "fi",
            `psql -v ON_ERROR_STOP=1 -h postgres -U elitea -d elitea -f ${path.join(repo, "services", "elitea-main", "internal", "infra", "db", "migrations", "001_initial.sql")}`,
          ].join("\n"),
        ],
        dependsOn: [{ name: "postgres", condition: "healthy" }],
      },
      {
        name: "elitea-migrate",
        init: true,
        cmd: path.join(bin, "elitea-migrate"),
        args: ["-all-tenants"],
        env: { DATABASE_URL: db },
        dependsOn: [{ name: "db-init", condition: "completed" }],
      },
      {
        name: "elitea-agentstate-migrate",
        init: true,
        cmd: path.join(bin, "elitea-migrate"),
        args: ["-agentstate"],
        env: { AGENTSTATE_DATABASE_URL: "postgres://elitea:elitea@postgres:5432/agentstate?sslmode=disable" },
        dependsOn: [{ name: "db-init", condition: "completed" }],
      },
      {
        name: "rustfs",
        // No hostname: the name map's rustfs (127.0.0.4) is refused as a bind, so every client of
        // this table is pointed at RUSTFS_ENDPOINT instead.
        ip: RUSTFS_BIND,
        cmd: "rustfs",
        env: {
          RUSTFS_ACCESS_KEY: "elitea",
          RUSTFS_SECRET_KEY: "elitea-dev-secret",
          // RUSTFS_BIND (above): 127.0.0.1, never a wildcard.
          RUSTFS_ADDRESS: `${RUSTFS_BIND}:9000`,
          RUSTFS_VOLUMES: path.join(data("rustfs"), "data"),
          RUSTFS_CONSOLE_ENABLE: "false",
          RUSTFS_OBS_LOG_DIRECTORY: path.join(data("rustfs"), "logs"),
          ...delta("rustfs"),
        },
        setup: 'mkdir -p "$RUSTFS_VOLUMES" "$RUSTFS_OBS_LOG_DIRECTORY"',
        health: { kind: "http", url: `${RUSTFS_ENDPOINT}/minio/health/cluster`, interval: 2, timeout: 3, retries: 15 },
      },
      {
        name: "rustfs-bucket-init",
        init: true,
        cmd: "sh",
        args: [
          "-c",
          [
            "i=0",
            `until rc alias set rustfs ${RUSTFS_ENDPOINT} elitea elitea-dev-secret; do`,
            "  i=$((i + 1))",
            '  if [ "$i" -ge 30 ]; then',
            '    echo "rustfs alias set still failing after 30 attempts (60s) — giving up" >&2',
            "    exit 1",
            "  fi",
            '  echo "rustfs not ready yet (attempt $i/30), retrying in 2s..." >&2',
            "  sleep 2",
            "done &&",
            "rc mb --ignore-existing rustfs/elitea-artifacts",
          ].join("\n"),
        ],
        dependsOn: [{ name: "rustfs", condition: "healthy" }],
      },
      {
        name: "oidc-mock",
        ip: ip("oidc-mock"),
        hostnames: ["oidc-mock", "oidc.localhost"],
        cmd: "oidc-provider-mock",
        args: ["--host", ip("oidc-mock"), "--port", oidcPort],
        health: { kind: "http", url: `http://${ip("oidc-mock")}:${oidcPort}/.well-known/openid-configuration`, interval: 2, timeout: 5, retries: 20, startPeriod: 5 },
      },
      {
        name: "elitea-llm-gateway",
        ip: ip("elitea-llm-gateway"),
        hostnames: ["elitea-llm-gateway"],
        cmd: path.join(bin, "elitea-llm-gateway"),
        env: {
          GATEWAY_HTTP_ADDR: `${ip("elitea-llm-gateway")}:8083`,
          GATEWAY_GRPC_ADDR: `${ip("elitea-llm-gateway")}:8443`,
          GATEWAY_GRPC_CLIENTS: "dns:elitea-main",
          DATABASE_URL: db,
          // Generated per stack (deploy/scripts/lib/stack-secrets.sh); no
          // literal value and no unwrapped opt-out (#2958).
          SECRETS_MASTER_KEY: d("SECRETS_MASTER_KEY", ""),
          GATEWAY_IDENTITY_SECRET: d("GATEWAY_IDENTITY_SECRET", ""),
          LLM_BUDGET_REQUIRE_ENFORCEMENT: d("LLM_BUDGET_REQUIRE_ENFORCEMENT", "off"),
          GATEWAY_TLS_CERT_FILE: cert("gateway-server.crt"),
          GATEWAY_TLS_KEY_FILE: cert("gateway-server.key"),
          GATEWAY_TLS_CA_FILE: cert("ca.crt"),
          // Every name here resolves to loopback on the pool, which the egress guard refuses unless a CIDR
          // names it, as compose's private ranges name Docker's network.
          GATEWAY_EGRESS_ALLOWLIST: egress.split(",").includes("127.0.0.0/8") ? egress : `${egress},127.0.0.0/8`,
          ELITEA_AI_PROJECT_ID: d("ELITEA_AI_PROJECT_ID", "1"),
          GATEWAY_SELF_LLM_ORIGINS: dd("GATEWAY_SELF_LLM_ORIGINS", "http://elitea-main:8080/llm/v1,http://localhost:8080/llm/v1"),
          GATEWAY_LOG_LEVEL: d("GATEWAY_LOG_LEVEL", "info"),
        },
        dependsOn: [
          { name: "postgres", condition: "healthy" },
          { name: "elitea-migrate", condition: "completed" },
          { name: "llm-mock", condition: "healthy" },
        ],
      },
      {
        name: "llm-mock",
        ip: ip("llm-mock"),
        hostnames: ["llm-mock"],
        cmd: "python3",
        args: [path.join(repo, "deploy", "mock-llm", "server.py")],
        env: {
          MOCK_LLM_HOST: ip("llm-mock"),
          MOCK_LLM_PORT: "8090",
          MOCK_LLM_MODEL: "E2E-MOCK-MODEL",
          MOCK_LLM_TOOL_BASE_URL: "https://llm-mock:8090/tool",
          MOCK_LLM_CHUNK_DELAY_MS: d("MOCK_LLM_CHUNK_DELAY_MS", "0"),
        },
        health: pyHealth(`http://${ip("llm-mock")}:8090/healthz`),
      },
      {
        name: "qtest-mock",
        ip: ip("qtest-mock"),
        hostnames: ["qtest-mock"],
        cmd: "python3",
        args: [path.join(repo, "deploy", "qtest-mock", "server.py")],
        env: { QTEST_MOCK_HOST: ip("qtest-mock"), QTEST_MOCK_PORT: "8096", QTEST_MOCK_TOKEN: d("STANDALONE_QTEST_TOKEN", "e2e-qtest-token") },
        health: pyHealth(`http://${ip("qtest-mock")}:8096/healthz`),
      },
      {
        name: "ado-mock",
        ip: ip("ado-mock"),
        hostnames: ["ado-mock"],
        cmd: "python3",
        args: [path.join(repo, "deploy", "ado-mock", "server.py")],
        env: {
          ADO_MOCK_HOST: ip("ado-mock"),
          ADO_MOCK_PORT: "8097",
          ADO_MOCK_PAT: d("STANDALONE_ADO_PAT", "e2e-ado-pat"),
          ADO_MOCK_PUBLIC_URL: "http://ado-mock:8097",
          ADO_MOCK_PROJECT: "E2E",
        },
        health: pyHealth(`http://${ip("ado-mock")}:8097/healthz`),
      },
      {
        // The stand-in (see the header): every export is answered 200 with an empty body, which decodes as
        // an empty Export*ServiceResponse, and is otherwise dropped, as the collector's debug exporter only
        // prints it.
        name: "otel-collector",
        ip: ip("otel-collector"),
        hostnames: ["otel-collector"],
        cmd: "python3",
        args: [
          "-c",
          [
            "import http.server, sys",
            "class H(http.server.BaseHTTPRequestHandler):",
            "    def do_POST(self):",
            "        self.rfile.read(int(self.headers.get('content-length') or 0))",
            "        self.send_response(200); self.send_header('content-type', self.headers.get('content-type') or 'application/x-protobuf')",
            "        self.send_header('content-length', '0'); self.end_headers()",
            "    def log_message(self, *a): pass",
            `http.server.ThreadingHTTPServer(('${ip("otel-collector")}', 4318), H).serve_forever()`,
          ].join("\n"),
        ],
      },

      // ── Application services ────────────────────────────────────────────
      {
        name: "elitea-main",
        ip: ip("elitea-main"),
        hostnames: ["elitea-main"],
        cmd: path.join(bin, "elitea-main"),
        files: [
          {
            // deploy/runtime/auth.form.yml with the runtime volume's path pointed at main's material.
            path: path.join(main, "auth.form.yml"),
            content: () => rewrite(read("deploy", "runtime", "auth.form.yml")(), [["/run/elitea-runtime/", `${path.join(material, "main")}/`]], "deploy/runtime/auth.form.yml"),
          },
        ],
        env: {
          ELITEA_WEB_UPSTREAM: `http://elitea-web:${WEB_PORT}`,
          DATABASE_URL: "postgres://elitea:elitea@pgbouncer:6432/elitea?sslmode=disable",
          ELITEA_DATABASE_MAX_CONNS: "16",
          ELITEA_RUNTIME_DB_ADMISSION_MAX_CONNS: "16",
          // Generated per stack (deploy/scripts/lib/stack-secrets.sh); no
          // literal value and no unwrapped opt-out (#2958).
          SECRETS_MASTER_KEY: d("SECRETS_MASTER_KEY", ""),
          ELITEA_EVENTS_NATS_URL: "nats://nats:4222",
          ADMIN_UI_STATIC_DIR: path.join(bin, "admin_ui", "static", "dist"),
          APPLICATION_SECRET_KEY: d("APPLICATION_SECRET_KEY", ""),
          OIDC_ISSUER_URL: `http://oidc.localhost:${oidcPort}`,
          OIDC_CLIENT_ID: "elitea-standalone-client",
          OIDC_CLIENT_SECRET: "elitea-standalone-secret",
          OIDC_REDIRECT_URI: `http://${host}:${MAIN_PORT}/auth/oidc/callback`,
          COOKIE_SECURE: "false",
          ELITEA_INITIAL_GLOBAL_ADMINS: dd("ELITEA_INITIAL_GLOBAL_ADMINS", ""),
          STORAGE_BACKEND: "s3",
          STORAGE_CONTAINER: "elitea-artifacts",
          S3_ENDPOINT_URL: RUSTFS_ENDPOINT,
          S3_ACCESS_KEY: "elitea",
          S3_SECRET_KEY: "elitea-dev-secret",
          S3_REGION: "us-east-1",
          S3_FORCE_PATH_STYLE: "true",
          LLM_GATEWAY_URL: "https://elitea-llm-gateway:8443",
          GATEWAY_IDENTITY_SECRET: d("GATEWAY_IDENTITY_SECRET", ""),
          LLM_GATEWAY_CLIENT_CERT: cert("client.crt"),
          LLM_GATEWAY_CLIENT_KEY: cert("client.key"),
          LLM_GATEWAY_CA_FILE: cert("ca.crt"),
          OTEL_EXPORTER_OTLP_ENDPOINT: "http://otel-collector:4318",
          ELITEA_AUTH_CONFIG_FILE: path.join(main, "auth.form.yml"),
          ELITEA_FORM_LOGIN_ENABLED: "false",
          ELITEA_CONFIGURATIONS_ENABLED: "true",
          ...providerClient("DEEPWIKI", "deepwiki"),
          ...providerClient("INVENTORY", "inventory"),
          ELITEA_TOOLKIT_DELETION_RECONCILER_ENABLED: d("ELITEA_TOOLKIT_DELETION_RECONCILER_ENABLED", "true"),
          ELITEA_AI_PROJECT_ID: d("ELITEA_AI_PROJECT_ID", "1"),
          ELITEA_SELF_LLM_ORIGINS: dd("ELITEA_SELF_LLM_ORIGINS", "http://elitea-main:8080/llm/v1,http://localhost:8080/llm/v1"),
          ELITEA_HTTP_ACTION_EGRESS_ALLOWLIST: d("ELITEA_HTTP_ACTION_EGRESS_ALLOWLIST", ""),
          ELITEA_PROJECT_INFO_ENABLED: "true",
          ELITEA_INDEX_TYPES_ENABLED: "true",
          ELITEA_APPLICATION_SKILLS_ENABLED: "true",
          ...Object.fromEntries(
            ["BRAND_PACK_PATH", "SMTP_HOST", "SMTP_PORT", "SMTP_USERNAME", "SMTP_PASSWORD", "SMTP_TLS", "EMAIL_FROM", "EMAIL_REPLY_TO", "PUBLIC_BASE_URL", "ELITEA_EMAIL_SUPPRESS"].map((k) => [k, d(k, "")]),
          ),
          ELITEA_RUNTIME_ENABLED: "true",
          // The fixtures toolkits reach are on loopback addresses here (#2950).
          ELITEA_WORKER_EGRESS_PRIVATE_PERMIT: d("ELITEA_WORKER_EGRESS_PRIVATE_PERMIT", "mcp-mock,ado-mock,qtest-mock,llm-mock,llm-real"),
          ELITEA_RUNTIME_COMMAND_STREAM: "ELITEA_RT_V1_VALIDATE",
          ELITEA_RUNTIME_MAX_OUTSTANDING: d("ELITEA_RUNTIME_MAX_OUTSTANDING", "64"),
          ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED: "true",
          ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM: "ELITEA_RT_V1_AGENT",
          ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED: "true",
          ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM: "ELITEA_RT_V1_AGENT",
          ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED: d("ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED", "true"),
          ELITEA_RUNTIME_NATS_URL: "nats://nats:4222",
          ELITEA_RUNTIME_SANDBOX_AUDIENCES: d("ELITEA_RUNTIME_SANDBOX_AUDIENCES", ""),
          ELITEA_RUNTIME_SIGNING_KEY_ID: "standalone-runtime-v1",
          ELITEA_RUNTIME_SIGNING_KEY_FILE: rt("main", "command-signing-key.pem"),
          ELITEA_RUNTIME_VERIFICATION_KEYRING_FILE: rt("main", "command-signing-keyring.json"),
          ...listener("CONTROL", 9443, "control-server"),
          ...listener("OUTPUT", 9444, "output-server"),
          ...listener("CONTENT", 9445, "content-server"),
          ...listener("PLATFORM", 9446, "content-server"),
          ELITEA_MODEL_SERVICE_CLIENTS: `${spiffe},dns:elitea-subapp-callback`,
          ELITEA_ARTIFACT_SERVICE_CLIENTS: `${spiffe},dns:elitea-subapp-callback`,
          ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS: `${spiffe},dns:elitea-subapp-callback`,
          ELITEA_VECTOR_INTROSPECTION_CLIENTS: "dns:elitea-vector",
          ELITEA_VECTOR_GRPC_TARGET: "elitea-vector:9470",
          ELITEA_VECTOR_GRPC_CLIENT_CERT_FILE: vec("main-client.crt"),
          ELITEA_VECTOR_GRPC_CLIENT_KEY_FILE: vec("main-client.key"),
          ELITEA_VECTOR_GRPC_CA_FILE: vec("runtime-ca.crt"),
          // docker-compose.standalone-rust-agent.yml
          ELITEA_INDEXING_RUNTIME: "rust",
          ELITEA_HTTP_ADDRESS: `${ip("elitea-main")}:${MAIN_PORT}`,
          ...delta("elitea-main"),
        },
        dependsOn: [
          { name: "pgbouncer", condition: "healthy" },
          { name: "runtime-material", condition: "completed" },
          { name: "elitea-deepwiki", condition: "healthy" },
          { name: "elitea-inventory", condition: "healthy" },
          { name: "nats-health", condition: "healthy" },
          { name: "nats-bootstrap", condition: "completed" },
          { name: "elitea-migrate", condition: "completed" },
          { name: "oidc-mock", condition: "healthy" },
          { name: "rustfs-bucket-init", condition: "completed" },
          { name: "elitea-llm-gateway", condition: "started" },
          { name: "otel-collector", condition: "started" },
        ],
        health: { kind: "cmd", cmd: path.join(bin, "elitea-main"), args: ["-healthcheck"], interval: 10, timeout: 5, retries: 6, startPeriod: 15 },
      },
      {
        name: "elitea-scheduler",
        ip: ip("elitea-scheduler"),
        hostnames: ["elitea-scheduler"],
        cmd: path.join(bin, "elitea-scheduler"),
        env: {
          DATABASE_URL: db,
          HTTP_ADDR: `${ip("elitea-scheduler")}:8081`,
          OTEL_EXPORTER_OTLP_ENDPOINT: "http://otel-collector:4318",
          AUDIT_RETENTION_DAYS: "365",
          SYNC_TOMBSTONE_RETENTION_DAYS: "97",
        },
        dependsOn: [
          { name: "postgres", condition: "healthy" },
          { name: "elitea-migrate", condition: "completed" },
          { name: "otel-collector", condition: "started" },
        ],
      },
      subappHost("deepwiki", "DEEPWIKI", deepwikiSock, {
        ELITEA_DEEPWIKI_RUNNER: "native",
        ELITEA_DEEPWIKI_ENGINE_SOCKET: deepwikiSock,
        ELITEA_DEEPWIKI_GIT_ALLOWLIST: "github.com,*.github.com",
      }),
      engineMigrate("deepwiki", "DEEPWIKI"),
      engine("deepwiki", "DEEPWIKI", deepwikiSock, {
        ELITEA_DEEPWIKI_RUNNER: d("DEEPWIKI_NATIVE_RUNNER", "fixture"),
        ELITEA_DEEPWIKI_FIXTURE_STEP_SECONDS: d("DEEPWIKI_FIXTURE_STEP_SECONDS", "1"),
        ELITEA_DEEPWIKI_MAX_PARALLEL_WORKERS: "8",
        ELITEA_DEEPWIKI_BUILD_OWNER: "standalone-deepwiki-engine",
        ELITEA_DEEPWIKI_GIT_ALLOWLIST: "github.com,*.github.com",
        ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES: "17179869184",
        ELITEA_DEEPWIKI_WORKER_CPU_SECONDS: "14400",
      }),
      subappHost("inventory", "INVENTORY", inventorySock, {
        ELITEA_INVENTORY_RUNNER: d("INVENTORY_RUNNER", "sidecar"),
        ELITEA_INVENTORY_ENGINE_SOCKET: inventorySock,
        ELITEA_INVENTORY_FIXTURE_STEP_SECONDS: d("INVENTORY_FIXTURE_STEP_SECONDS", "1"),
      }),
      engineMigrate("inventory", "INVENTORY"),
      engine("inventory", "INVENTORY", inventorySock, {
        ELITEA_INVENTORY_RUNNER: d("INVENTORY_ENGINE_RUNNER", "fixture"),
        ELITEA_INVENTORY_FIXTURE_STEP_SECONDS: d("INVENTORY_FIXTURE_STEP_SECONDS", "1"),
        ELITEA_INVENTORY_GIT_ALLOWLIST: d("INVENTORY_GIT_ALLOWLIST", "github.com,*.github.com"),
        // compose mounts conformance/provider/fixtures/inventory at /fixtures/inventory.
        ELITEA_INVENTORY_FIXTURES: d("INVENTORY_FIXTURES", "").replaceAll("/fixtures/inventory", path.join(repo, "conformance", "provider", "fixtures", "inventory")),
      }),
      {
        name: "qdrant",
        ip: ip("qdrant"),
        hostnames: ["qdrant"],
        // compose: QDRANT__SERVICE__API_KEY="$(cat /run/qdrant/api-key)" exec ./entrypoint.sh
        cmd: "sh",
        args: ["-c", 'QDRANT__SERVICE__API_KEY="$(cat "$QDRANT_API_KEY_FILE")" exec qdrant'],
        env: {
          QDRANT__TELEMETRY_DISABLED: "true",
          QDRANT__LOG_LEVEL: "INFO",
          QDRANT__SERVICE__HOST: ip("qdrant"),
          QDRANT__STORAGE__STORAGE_PATH: path.join(data("qdrant"), "storage"),
          QDRANT__STORAGE__SNAPSHOTS_PATH: path.join(data("qdrant"), "snapshots"),
          QDRANT_API_KEY_FILE: vec("qdrant-api-key"),
        },
        health: { kind: "http", url: `http://${ip("qdrant")}:6333/readyz`, interval: 5, timeout: 3, retries: 12, startPeriod: 10 },
      },
      {
        name: "elitea-vector",
        ip: ip("elitea-vector"),
        hostnames: ["elitea-vector"],
        cmd: path.join(bin, "elitea-vector"),
        env: {
          ELITEA_VECTOR_GRPC_ADDR: `${ip("elitea-vector")}:9470`,
          ELITEA_VECTOR_HEALTH_ADDR: `${ip("elitea-vector")}:9471`,
          ELITEA_VECTOR_TLS_CERT_FILE: vec("vector.crt"),
          ELITEA_VECTOR_TLS_KEY_FILE: vec("vector.key"),
          ELITEA_VECTOR_TLS_CLIENT_CA_FILE: vec("runtime-ca.crt"),
          ELITEA_VECTOR_ADMIN_IDENTITIES: "dns:elitea-main",
          ELITEA_VECTOR_QDRANT_URL: "http://qdrant:6334",
          ELITEA_VECTOR_QDRANT_API_KEY_FILE: vec("qdrant-api-key"),
          ELITEA_VECTOR_COLLECTION_REPLICATION_FACTOR: "1",
          ELITEA_VECTOR_COLLECTION_SHARD_NUMBER: "1",
          ELITEA_VECTOR_INTROSPECTION_URL: "https://elitea-main:9443",
          ELITEA_VECTOR_INTROSPECTION_CA_FILE: vec("runtime-ca.crt"),
          ELITEA_VECTOR_INTROSPECTION_SERVER_NAME: "elitea-main",
          ELITEA_VECTOR_INTROSPECTION_TIMEOUT_MS: "2000",
          OTEL_EXPORTER_OTLP_ENDPOINT: "http://otel-collector:4318",
          RUST_LOG: d("ELITEA_VECTOR_LOG", "info"),
        },
        dependsOn: [
          { name: "qdrant", condition: "healthy" },
          { name: "otel-collector", condition: "started" },
        ],
        // `elitea-vector healthcheck` dials 127.0.0.1:<health port>; here the port is on vector's own
        // address, so the check sends the same GET /healthz there.
        health: { kind: "http", url: `http://${ip("elitea-vector")}:9471/healthz`, interval: 5, timeout: 3, retries: 12, startPeriod: 10 },
      },
      {
        name: "elitea-web",
        ip: ip("elitea-web"),
        hostnames: ["elitea-web"],
        cmd: "sh",
        args: [path.join(repo, "apps", "elitea-web", "docker-entrypoint.sh"), "-p", web, "-c", path.join(web, "nginx.conf"), "-e", "stderr"],
        env: {
          VITE_SERVER_URL: "/api/v2",
          VITE_BASE_URI: "/app/",
          VITE_SOCKET_SERVER: "",
          VITE_SOCKET_PATH: "/socket.io",
          VITE_PUBLIC_PROJECT_ID: d("VITE_PUBLIC_PROJECT_ID", "99"),
          ELITEA_WEB_CONFIG_JS: path.join(web, "html", "config.js"),
        },
        setup: "mkdir -p html/docs tmp/body tmp/proxy tmp/fastcgi tmp/uwsgi tmp/scgi",
        files: webFiles(),
        dependsOn: [{ name: "elitea-main", condition: "healthy" }],
        health: { kind: "http", url: `http://${ip("elitea-web")}:${WEB_PORT}/app/`, interval: 3, timeout: 3, retries: 10, startPeriod: 5 },
      },
      {
        name: "runtime-session-init",
        init: true,
        cmd: "sh",
        args: [path.join(repo, "deploy", "runtime", "provision-runtime-session.sh")],
        env: {
          PGURL: db,
          SESSION_ID: d("RUNTIME_WORKER_SESSION_ID", "standalone-agent-worker-1"),
          PRODUCER_ID: d("RUNTIME_WORKER_PRODUCER_ID", "standalone-agent-worker-1"),
          EXPECTED_IDENTITY: spiffe,
          TTL: d("RUNTIME_WORKER_SESSION_TTL", "2160h"),
          WAIT_ATTEMPTS: d("RUNTIME_WORKER_SESSION_WAIT_ATTEMPTS", "60"),
          WAIT_INTERVAL: d("RUNTIME_WORKER_SESSION_WAIT_INTERVAL", "5"),
          CERT: rt("worker", "agent-worker-client.crt"),
        },
        dependsOn: [
          { name: "runtime-material", condition: "completed" },
          { name: "elitea-migrate", condition: "completed" },
        ],
      },
      {
        name: "elitea-worker",
        ip: ip("elitea-worker"),
        hostnames: ["elitea-worker"],
        cmd: path.join(bin, "elitea-worker-rust"),
        args: [
          "serve",
          "--config", path.join(data("elitea-worker"), "runtime.json"),
          "--toolkit-security-config", path.join(repo, "deploy", "runtime", "worker-toolkit-security.json"),
        ],
        files: [
          {
            // deploy/runtime/worker-runtime.rust.json: its hostnames resolve through the name map unchanged;
            // its mounted paths point at the worker's material, the extractor's socket and the spool.
            path: path.join(data("elitea-worker"), "runtime.json"),
            content: () =>
              rewrite(read("deploy", "runtime", "worker-runtime.rust.json")(), [
                ['"/run/elitea-runtime/', `"${path.join(material, "worker")}/`],
                ['"/run/elitea-extract/extractor.sock"', JSON.stringify(extractorSock)],
                ['"/var/lib/elitea-worker/output-spool"', JSON.stringify(spool)],
              ], "deploy/runtime/worker-runtime.rust.json"),
          },
        ],
        env: {
          ELITEA_SENSITIVE_TOOLS: "null",
          SSL_CERT_FILE: trust,
          ELITEA_RUST_LOG: "info",
          ELITEA_RUST_TRACE: "info",
          ELITEA_DB_EGRESS_ALLOWLIST: dd("ELITEA_DB_EGRESS_ALLOWLIST", ""),
          OTEL_EXPORTER_OTLP_ENDPOINT: "http://otel-collector:4318",
        },
        dependsOn: [
          { name: "runtime-material", condition: "completed" },
          { name: "worker-spool-init", condition: "completed" },
          { name: "nats-bootstrap", condition: "completed" },
          { name: "runtime-session-init", condition: "completed" },
          { name: "elitea-main", condition: "healthy" },
          { name: "elitea-agentstate-migrate", condition: "completed" },
          { name: "otel-collector", condition: "started" },
          { name: "elitea-vector", condition: "healthy" },
          { name: "elitea-doc-extractor", condition: "started" },
          { name: "mcp-mock-trust", condition: "completed" },
          { name: "mcp-mock", condition: "healthy" },
        ],
      },
      {
        name: "worker-spool-init",
        init: true,
        // compose: chown 10001:10001 /spool && chmod 700 /spool (the worker is the runner user here).
        cmd: "sh",
        args: ["-ec", `mkdir -p "${spool}" && chmod 700 "${spool}"`],
      },
      {
        name: "mcp-mock",
        ip: ip("mcp-mock"),
        hostnames: ["mcp-mock"],
        // deploy/mock-mcp/Containerfile's `tls` stage, as setup: the same CA, SANs and chain.
        setup: [
          "mkdir -p tls && cd tls",
          'openssl req -x509 -newkey rsa:2048 -nodes -keyout ca.key -out ca.crt -days 3650 -subj "/CN=elitea-mock-mcp-ca" -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign"',
          'openssl req -newkey rsa:2048 -nodes -keyout server.key -out server.csr -subj "/CN=mcp-mock"',
          "printf '%s\\n' 'basicConstraints=critical,CA:FALSE' 'subjectAltName=DNS:mcp-mock,DNS:localhost,IP:127.0.0.1' 'extendedKeyUsage=serverAuth' 'keyUsage=critical,digitalSignature,keyEncipherment' > leaf.ext",
          "openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out server.crt -days 3650 -sha256 -extfile leaf.ext",
          "cat server.crt ca.crt > server-chain.crt",
        ].join("\n"),
        cmd: "python3",
        args: [path.join(repo, "deploy", "mock-mcp", "server.py")],
        env: {
          MOCK_MCP_PORT: "8443",
          MOCK_MCP_HOST: ip("mcp-mock"),
          MOCK_MCP_CERT: path.join(mcpTls, "server-chain.crt"),
          MOCK_MCP_KEY: path.join(mcpTls, "server.key"),
        },
        health: {
          kind: "cmd",
          cmd: "python3",
          args: ["-c", `import ssl,urllib.request;urllib.request.urlopen('https://${ip("mcp-mock")}:8443/healthz',context=ssl._create_unverified_context()).read()`],
          interval: 3,
          timeout: 3,
          retries: 10,
          startPeriod: 3,
        },
      },
      {
        name: "mcp-mock-trust",
        init: true,
        cmd: "sh",
        args: [
          "-ec",
          `mkdir -p "${path.dirname(trust)}" && cat /etc/ssl/certs/ca-certificates.crt "${path.join(mcpTls, "ca.crt")}" "${rt("worker", "runtime-ca.crt")}" > "${trust}" && chmod 0644 "${trust}" && python3 "${path.join(repo, "deploy", "mock-mcp", "verify-trust.py")}" "${trust}"`,
        ],
        dependsOn: [
          { name: "runtime-material", condition: "completed" },
          { name: "mcp-mock", condition: "healthy" },
        ],
      },
      {
        // No address: compose gives it no network at all, and it listens only on its Unix socket. The peer
        // uid it admits is the runner's, the uid elitea-worker runs as here (compose: 10001, the worker image's).
        name: "elitea-doc-extractor",
        cmd: path.join(bin, "elitea-doc-extractor"),
        args: ["serve"],
        setup: `mkdir -p "${path.dirname(extractorSock)}"`,
        env: {
          ELITEA_EXTRACTOR_SOCKET: extractorSock,
          ELITEA_EXTRACTOR_MAX_INPUT_BYTES: "52428800",
          ELITEA_EXTRACTOR_MAX_PAGES: "1000",
          ELITEA_EXTRACTOR_TIMEOUT_MILLIS: "120000",
          ELITEA_EXTRACTOR_CONCURRENCY: "4",
          ELITEA_EXTRACTOR_SLOT_WAIT_MILLIS: "600000",
          ELITEA_EXTRACTOR_PEER_UIDS: String(process.getuid()),
          RUST_LOG: d("ELITEA_EXTRACTOR_LOG", "info"),
        },
        health: { kind: "cmd", cmd: path.join(bin, "elitea-doc-extractor"), args: ["healthcheck"], interval: 5, timeout: 3, retries: 12, startPeriod: 5 },
      },
    ],
  };

  /** elitea-main's DeepWiki / Inventory provider client block. */
  function providerClient(prefix, app) {
    const p = (k) => `ELITEA_${prefix}_${k}`;
    return {
      [p("ENABLED")]: "true",
      [p("SERVER_NAME")]: `elitea-${app}`,
      [p("CLIENT_CERT_FILE")]: cert("client.crt"),
      [p("CLIENT_KEY_FILE")]: cert("client.key"),
      [p("CA_FILE")]: cert("ca.crt"),
      [p("IDENTITY_SECRET")]: d(p("IDENTITY_SECRET"), ""),
      [p("GIT_ALLOWLIST")]: "github.com,*.github.com",
      [p("PLATFORM_GRPC_TARGET")]: `elitea-${app}:9443`,
    };
  }

  /** One of elitea-main's four runtime listeners, on its own address. */
  function listener(kind, port, certName) {
    const p = (k) => `ELITEA_RUNTIME_${kind}_${k}`;
    return {
      [p("ADDRESS")]: `${ip("elitea-main")}:${port}`,
      [p("TLS_CERT_FILE")]: rt("main", `${certName}.crt`),
      [p("TLS_KEY_FILE")]: rt("main", `${certName}.key`),
      [p("TLS_CLIENT_CA_FILE")]: rt("main", "runtime-ca.crt"),
    };
  }

  /** elitea-deepwiki / elitea-inventory: the sub-application host over its engine's socket. */
  function subappHost(app, prefix, socket, extra) {
    const name = `elitea-${app}`;
    const p = (k) => `ELITEA_${prefix}_${k}`;
    return {
      name,
      ip: ip(name),
      hostnames: [name],
      cmd: path.join(bin, "elitea-subapp-host"),
      env: {
        ELITEA_SUBAPP: app,
        ...extra,
        [p("TLS_CERTFILE")]: cert(`${app}-server.crt`),
        [p("TLS_KEYFILE")]: cert(`${app}-server.key`),
        [p("TLS_CA_FILE")]: cert("ca.crt"),
        [p("IDENTITY_SECRET")]: d(p("IDENTITY_SECRET"), ""),
        [p("SCRATCH_PATH")]: path.join(data(name), "scratch"),
        [p("SERVICE_LOCATION_URL")]: `grpcs://${name}:9443`,
        [p("PLATFORM_CLIENTS")]: "elitea-main",
        [p("PLATFORM_GRPC_ADDR")]: `${ip(name)}:9443`,
        [p("ADMIN_ADDR")]: `${ip(name)}:9090`,
        [p("NATS_URL")]: "nats://nats:4222",
        [p("ARTIFACT_SERVICE_URL")]: "https://elitea-main:9446",
        [p("CALLBACK_CA_FILE")]: rt("subapp-host", "runtime-ca.crt"),
        [p("CALLBACK_CERT_FILE")]: rt("subapp-host", "subapp-callback-client.crt"),
        [p("CALLBACK_KEY_FILE")]: rt("subapp-host", "subapp-callback-client.key"),
      },
      setup: `mkdir -p "$${p("SCRATCH_PATH")}"`,
      dependsOn: [
        { name: "runtime-material", condition: "completed" },
        { name: `${name}-engine`, condition: "healthy" },
        { name: "nats-health", condition: "healthy" },
      ],
      // `elitea-subapp-host healthcheck` reads /readyz on 127.0.0.1 at the admin port; here the admin port
      // is on the service's own address, so the check reads the same /readyz there.
      health: { kind: "http", url: `http://${ip(name)}:9090/readyz`, interval: 5, timeout: 3, retries: 12, startPeriod: 10 },
    };
  }

  function engineMigrate(app, prefix) {
    return {
      name: `elitea-${app}-migrate`,
      init: true,
      cmd: path.join(bin, `elitea-${app}-engine`),
      args: ["migrate"],
      env: { [`ELITEA_${prefix}_DATABASE_URL`]: "postgresql://elitea:elitea@postgres:5432/elitea" },
      dependsOn: [
        { name: "postgres", condition: "healthy" },
        { name: "db-init", condition: "completed" },
      ],
    };
  }

  /** The DeepWiki / Inventory engine: no address (it serves its Unix socket only), the image's own check. */
  function engine(app, prefix, socket, extra) {
    const name = `elitea-${app}-engine`;
    const p = (k) => `ELITEA_${prefix}_${k}`;
    return {
      name,
      cmd: path.join(bin, name),
      args: ["serve"],
      setup: `mkdir -p "${path.dirname(socket)}" "$${p("SCRATCH_PATH")}"`,
      env: {
        ...extra,
        [p("ENGINE_SOCKET")]: socket,
        [p("SCRATCH_PATH")]: path.join(data(name), "scratch"),
        [p("DATABASE_URL")]: "postgresql://elitea:elitea@postgres:5432/elitea",
        [p("MODEL_SERVICE_URL")]: "https://elitea-main:9446",
        [p("VECTOR_URL")]: "https://elitea-vector:9470",
        [p("CALLBACK_CA_FILE")]: rt("subapp", "runtime-ca.crt"),
        [p("CALLBACK_CERT_FILE")]: rt("subapp", "subapp-callback-client.crt"),
        [p("CALLBACK_KEY_FILE")]: rt("subapp", "subapp-callback-client.key"),
      },
      dependsOn: [
        { name: "postgres", condition: "healthy" },
        { name: `elitea-${app}-migrate`, condition: "completed" },
        { name: "runtime-material", condition: "completed" },
        { name: "elitea-vector", condition: "healthy" },
      ],
      health: { kind: "cmd", cmd: path.join(bin, name), args: ["healthcheck"], interval: 5, timeout: 3, retries: 12, startPeriod: 10 },
    };
  }

  /** elitea-web's nginx files, as deploy/native/e2e-standalone.mjs renders them. */
  function webFiles() {
    const nginx = (f) => read("apps", "elitea-web", "nginx", f);
    return [
      {
        path: path.join(web, "nginx.conf"),
        content: () =>
          [
            `pid ${web}/nginx.pid;`,
            "worker_processes auto;",
            "events { worker_connections 1024; }",
            "http {",
            "  include /etc/nginx/mime.types;",
            "  default_type application/octet-stream;",
            // Not /dev/stdout: the supervisor's stdout is a socket, which open() refuses (ENXIO).
            `  access_log ${web}/access.log;`,
            "  sendfile on;",
            "  keepalive_timeout 65;",
            ...["client_body", "proxy", "fastcgi", "uwsgi", "scgi"].map((t) => `  ${t}_temp_path ${web}/tmp/${t === "client_body" ? "body" : t};`),
            `  include ${web}/conf.d/*.conf;`,
            "}",
            "",
          ].join("\n"),
      },
      {
        path: path.join(web, "conf.d", "default.conf"),
        content: () =>
          renderSpaConf(nginx("spa.conf")(), {
            spaDir: path.join(bin, "web", "app"),
            htmlDir: path.join(web, "html"),
            snippetsDir: path.join(web, "snippets"),
            listen: `${ip("elitea-web")}:${WEB_PORT}`,
          }),
      },
      ...["security-headers.conf", "docs-security-headers.conf", "opener-isolation.conf"].map((f) => ({ path: path.join(web, "snippets", f), content: nginx(f) })),
      {
        path: path.join(web, "html", "docs", "index.html"),
        content:
          '<!doctype html><meta charset="utf-8"><title>Elitea AI Docs</title>\n' +
          "<p>The documentation is in the release image. Locally: <code>npm run dev</code> in apps/elitea-docs.</p>\n",
      },
    ];
  }
}
