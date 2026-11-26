// deploy/docker-compose.e2e-standalone.yml as processes on the self-hosted pool.
//
// Read by scripts/e2e/native-stack.mjs (the supervisor) when apps/elitea-web/scripts/e2e-stack.sh runs with
// E2E_STACK=native. Every compose service is here under its compose name, with its compose `environment:`
// verbatim except for three kinds of rewrite:
//
//   * paths: /certs -> deploy/certs, /run/elitea-platform -> platform-material's data dir, the images'
//     static dirs -> the build directory (E2E_NATIVE_BIN), /tmp scratch dirs -> the service's data dir;
//   * binds: each service listens on ITS address from the pod's hostAliases (deploy/ci-runners/README.md,
//     "Native stack name map"), at its compose port; ":9443" becomes "127.0.0.5:9443";
//   * what a container network gave for free: 127.0.0.0/8 where compose relied on Docker's private ranges,
//     and nginx on 8080 (a non-root process cannot bind 80).
//
// scripts/ci/check-native-stack-parity.mjs fails when a compose `environment:` key, a `depends_on` edge, a
// healthcheck or a one-shot has no counterpart here, so a variable added to the compose file cannot be
// silently missing natively. A variable that exists only here is listed in NATIVE_ONLY_ENV with its reason,
// and a compose variable that does not apply natively in EXEMPT_ENV with its reason.
//
// The build directory (E2E_NATIVE_BIN) is what ci-web-e2e-native.yml's `native-build` job uploads:
//
//   elitea-main, elitea-migrate, elitea-subapp-host      CGO_ENABLED=0, as the images build them
//   web/app/                                             apps/elitea-web dist/app      (the web image's spa)
//   admin_ui/static/dist/                                apps/elitea-web dist/admin    (elitea-main's e2e target)
//   admin_ui/static/brand-preview/                       apps/elitea-web dist/brand-preview
//
// The image's own binaries come from the image README (deploy/ci-runners/image/README.md): postgres 18,
// nats-server 2.12.0, the nats CLI 0.4.0, rustfs 1.0.0, rc, oidc-provider-mock 0.4.8, nginx-light.

import fs from "node:fs";
import path from "node:path";

export const COMPOSE_FILE = "deploy/docker-compose.e2e-standalone.yml";

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
};
/**
 * Where rustfs listens, and how this table's clients reach it. rustfs refuses a bind host that is not one of
 * the machine's own interface addresses ("host in server address should be this server"), and its name-map
 * address 127.0.0.4 is not one (lo carries 127.0.0.1/8). A wildcard bind is never an option: the pool is a
 * shared cluster, and a CI service with dev credentials must not listen on the pod's network address. So it
 * binds 127.0.0.1:9000 (no other service of either stack uses 9000) and its clients, here and in
 * standalone-full.mjs, the e2e-stack.sh probes and handoff-live.sh, use http://127.0.0.1:9000, not
 * http://rustfs:9000. scripts/ci/check-native-stack-parity.mjs knows that endpoint rewrite.
 */
export const RUSTFS_BIND = "127.0.0.1";
export const RUSTFS_ENDPOINT = `http://${RUSTFS_BIND}:9000`;
/** The non-root nginx port (compose's container listens on 80). ELITEA_WEB_UPSTREAM names it. */
export const WEB_PORT = 8080;

/** Variables only the native table sets, and why. The parity check refuses any other extra. */
export const NATIVE_ONLY_ENV = {
  postgres: {
    PGDATA: "the cluster directory under the service's data dir (the image sets PGDATA itself)",
    PG_SOCKET_DIR: "the Unix socket directory: compose's container has its own /var/run/postgresql",
  },
  rustfs: {
    RUSTFS_ADDRESS: "the bind: the image listens on :9000; natively 127.0.0.1:9000 (RUSTFS_BIND), loopback only",
    RUSTFS_VOLUMES: "the data directory the image keeps at /data",
    RUSTFS_CONSOLE_ENABLE: "the image's console is never published; natively it would bind another port",
    RUSTFS_OBS_LOG_DIRECTORY: "the image writes its logs under /logs; natively under the data dir",
  },
  "elitea-main": {
    ELITEA_HTTP_ADDRESS: "the bind: the image listens on :8080 (cmd/elitea-main/http_address.go); natively 127.0.0.1:E2E_PORT, where compose published it",
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
  "elitea-main": {
    ELITEA_TOOLKIT_CHECK_ALLOWLIST: "127.0.0.0/8 added: natively every service is a loopback address (the pod's name map), which the container networks never were",
    ELITEA_WEB_UPSTREAM: "the non-root nginx listens on 8080 natively (WEB_PORT); the container binds 80",
  },
  "elitea-deepwiki": { ELITEA_DEEPWIKI_SCRATCH_PATH: "the container's private /tmp; natively a per-service directory under the stack state, so two services never share one" },
  "elitea-inventory": { ELITEA_INVENTORY_SCRATCH_PATH: "the container's private /tmp; natively a per-service directory under the stack state, so two services never share one" },
};

/**
 * Compose variables that do not apply natively, and why. Empty today: every compose variable of this stack
 * has a native value. An entry whose variable left the compose file fails the parity check, so the list
 * cannot go stale.
 */
export const EXEMPT_ENV = {};

const need = (cond, msg) => {
  if (!cond) throw new Error(`deploy/native/e2e-standalone.mjs: ${msg}`);
};

/** spa.conf with the image's paths and its port 80 replaced; each must be found, so a drift fails loudly. */
export function renderSpaConf(source, { spaDir, htmlDir, snippetsDir, listen }) {
  const rewrites = [
    [/^(\s*)listen 80;$/m, `$1listen ${listen};`],
    [/\/usr\/share\/nginx\/html\/spa\//g, `${spaDir}/`],
    [/\/usr\/share\/nginx\/html\//g, `${htmlDir}/`],
    [/\/etc\/nginx\/snippets\//g, `${snippetsDir}/`],
  ];
  let out = source;
  for (const [from, to] of rewrites) {
    need(from.test(out), `apps/elitea-web/nginx/spa.conf no longer contains ${from}; update renderSpaConf`);
    from.lastIndex = 0;
    out = out.replace(from, to);
  }
  need(!/\/usr\/share\/nginx|\/etc\/nginx\/(?!mime\.types)/.test(out), "spa.conf names an image path renderSpaConf does not rewrite");
  return out;
}

export default function table(ctx) {
  const { repo, data, bin, env } = ctx;
  const certs = path.join(repo, "deploy", "certs");
  const cert = (f) => path.join(certs, f);
  const platform = data("platform-material");
  const e2ePort = env.E2E_PORT || "8082";
  const oidcPort = env.E2E_OIDC_PORT || "9400";
  const ip = (svc) => ADDRESSES[svc];
  const pg = data("postgres");
  // A Unix socket path is limited to ~104 bytes (.github/actions/native-services/start.sh).
  const pgSocket = path.join(pg, "sock").length > 80 ? ctx.short("pg") : path.join(pg, "sock");
  const web = data("elitea-web");
  const readNginx = (f) => () => fs.readFileSync(path.join(repo, "apps", "elitea-web", "nginx", f), "utf8");

  return {
    services: [
      // ── Infrastructure ──────────────────────────────────────────────────
      {
        name: "postgres",
        ip: ip("postgres"),
        hostnames: ["postgres"],
        // The postgres image's first start, as its entrypoint does it: initdb with the superuser and its
        // password (scram over TCP, trust on the socket), the image's locale, then POSTGRES_DB on a server
        // that listens on the socket only. The CI-only settings trade crash safety for the pod's disk.
        setup: [
          'pw="$(mktemp)"; printf "%s\\n" "$POSTGRES_PASSWORD" > "$pw"',
          'loc="--locale=en_US.UTF-8"; locale -a 2>/dev/null | grep -qix "en_US.utf-\\?8" || loc="--locale=C.UTF-8"',
          'initdb -D "$PGDATA" -U "$POSTGRES_USER" --pwfile="$pw" --auth-local=trust --auth-host=scram-sha-256 --encoding=UTF8 $loc',
          'rm -f "$pw"; mkdir -p "$PG_SOCKET_DIR"',
          // -l: the temporary server must not hold the setup's output pipe open.
          `pg_ctl -D "$PGDATA" -l "$PGDATA/../setup-server.log" -w -t 60 -o "-c listen_addresses='' -c unix_socket_directories=$PG_SOCKET_DIR" start`,
          '[ "$POSTGRES_DB" = postgres ] || createdb -h "$PG_SOCKET_DIR" -U "$POSTGRES_USER" "$POSTGRES_DB"',
          'pg_ctl -D "$PGDATA" -m fast -w stop',
        ].join("\n"),
        cmd: "postgres",
        args: [
          "-D", path.join(pg, "cluster"),
          "-c", `listen_addresses=${ip("postgres")}`,
          "-c", "port=5432",
          "-c", `unix_socket_directories=${pgSocket}`,
          "-c", "fsync=off",
          "-c", "synchronous_commit=off",
          "-c", "full_page_writes=off",
        ],
        env: { POSTGRES_USER: "elitea", POSTGRES_PASSWORD: "elitea", POSTGRES_DB: "elitea", PGDATA: path.join(pg, "cluster"), PG_SOCKET_DIR: pgSocket },
        stop: { cmd: "pg_ctl", args: ["-D", path.join(pg, "cluster"), "-m", "fast", "-w", "-t", "10", "stop"] },
        health: { kind: "pg", host: ip("postgres"), port: 5432, user: "elitea", db: "elitea", interval: 5, timeout: 5, retries: 10 },
      },
      {
        name: "nats",
        ip: ip("nats"),
        hostnames: ["nats"],
        // compose: ["-js", "-sd", "/data", "-m", "8222"], on the container's address. A config file, not
        // flags: -a binds only the client port, and the monitor's host must be named too (loopback only).
        cmd: "nats-server",
        args: ["-c", path.join(data("nats"), "nats.conf")],
        files: [
          {
            path: path.join(data("nats"), "nats.conf"),
            content: () =>
              [
                `listen: ${ip("nats")}:4222`,
                `http: ${ip("nats")}:8222`,
                `jetstream { store_dir: ${JSON.stringify(data("nats"))} }`,
                "",
              ].join("\n"),
          },
        ],
      },
      {
        // An idle process whose check reads the server's /healthz from outside, as the compose sidecar does:
        // nats itself has none, and its dependents wait on this one.
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
        },
        setup: 'mkdir -p "$RUSTFS_VOLUMES" "$RUSTFS_OBS_LOG_DIRECTORY"',
        // /minio/health/cluster stays 503 until quorum (the compose file's comment).
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

      // ── OIDC mock ───────────────────────────────────────────────────────
      {
        // On 127.0.0.1 with elitea-main: the browser reaches oidc.localhost there by itself.
        name: "oidc-mock",
        ip: ip("oidc-mock"),
        hostnames: ["oidc-mock", "oidc.localhost"],
        cmd: "oidc-provider-mock",
        args: ["--host", ip("oidc-mock"), "--port", oidcPort],
        health: { kind: "http", url: `http://${ip("oidc-mock")}:${oidcPort}/.well-known/openid-configuration`, interval: 2, timeout: 5, retries: 20, startPeriod: 5 },
      },

      // ── Application services ────────────────────────────────────────────
      {
        // The image's platform-listener material: securefile wants the key owner-only and readable by the
        // reading uid, here the runner itself.
        name: "platform-material",
        init: true,
        cmd: "sh",
        args: [
          "-ec",
          `install -m 0600 "${cert("platform-server.key")}" "${platform}/platform-server.key" && ` +
            `install -m 0644 "${cert("platform-server.crt")}" "${platform}/platform-server.crt" && ` +
            `install -m 0644 "${cert("ca.crt")}" "${platform}/ca.crt"`,
        ],
      },
      {
        name: "elitea-main",
        ip: ip("elitea-main"),
        hostnames: ["elitea-main"],
        cmd: path.join(bin, "elitea-main"),
        env: {
          DATABASE_URL: "postgres://elitea:elitea@postgres:5432/elitea?sslmode=disable",
          ELITEA_EVENTS_NATS_URL: "nats://nats:4222",
          ELITEA_WEB_UPSTREAM: `http://elitea-web:${WEB_PORT}`,
          BRAND_PACK_PATH: env.BRAND_PACK_PATH ?? "",
          ADMIN_UI_STATIC_DIR: path.join(bin, "admin_ui", "static", "dist"),
          // Generated per stack (deploy/scripts/lib/stack-secrets.sh, #2958).
          APPLICATION_SECRET_KEY: env.APPLICATION_SECRET_KEY ?? "",
          SKIP_MIGRATIONS: "1",
          OIDC_ISSUER_URL: `http://oidc.localhost:${oidcPort}`,
          // The mock is on loopback here; the CIDRs keep compose parity.
          ELITEA_IDENTITY_PROVIDER_EGRESS_ALLOWLIST: "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16",
          ELITEA_IDENTITY_PROVIDER_ALLOW_LOOPBACK: "true",
          OIDC_CLIENT_ID: "elitea-e2e-client",
          OIDC_CLIENT_SECRET: "elitea-e2e-secret",
          OIDC_REDIRECT_URI: `http://localhost:${e2ePort}/auth/oidc/callback`,
          COOKIE_SECURE: "false",
          DEPLOYMENT_URL: `http://localhost:${e2ePort}`,
          ELITEA_INITIAL_GLOBAL_ADMINS: env.ELITEA_INITIAL_GLOBAL_ADMINS ?? "",
          STORAGE_BACKEND: "s3",
          STORAGE_CONTAINER: "elitea-artifacts",
          S3_ENDPOINT_URL: RUSTFS_ENDPOINT,
          S3_ACCESS_KEY: "elitea",
          S3_SECRET_KEY: "elitea-dev-secret",
          S3_REGION: "us-east-1",
          S3_FORCE_PATH_STYLE: "true",
          ELITEA_INDEX_TYPES_ENABLED: "true",
          ELITEA_APPLICATION_SKILLS_ENABLED: "true",
          ELITEA_CONFIGURATIONS_ENABLED: "true",
          ELITEA_AI_PROJECT_ID: "1",
          SECRETS_MASTER_KEY: env.SECRETS_MASTER_KEY ?? "",
          // The two names resolve to loopback here (hostAliases), which the egress guard classes as private
          // like Docker's ranges (libs/go/egresslib/addrcheck/classify.go), so 127.0.0.0/8 is named too.
          ELITEA_TOOLKIT_CHECK_ALLOWLIST: "elitea-main, elitea-web, 172.16.0.0/12, 192.168.0.0/16, 127.0.0.0/8",
          ELITEA_DEEPWIKI_ENABLED: "true",
          ELITEA_DEEPWIKI_PLATFORM_GRPC_TARGET: "elitea-deepwiki:9443",
          ELITEA_DEEPWIKI_SERVER_NAME: "elitea-deepwiki",
          ELITEA_DEEPWIKI_CLIENT_CERT_FILE: cert("client.crt"),
          ELITEA_DEEPWIKI_CLIENT_KEY_FILE: cert("client.key"),
          ELITEA_DEEPWIKI_CA_FILE: cert("ca.crt"),
          ELITEA_DEEPWIKI_IDENTITY_SECRET: env.ELITEA_DEEPWIKI_IDENTITY_SECRET ?? "",
          ELITEA_DEEPWIKI_GIT_ALLOWLIST: "github.com,*.github.com",
          ELITEA_RUNTIME_PLATFORM_ADDRESS: `${ip("elitea-main")}:9446`,
          ELITEA_RUNTIME_PLATFORM_TLS_CERT_FILE: path.join(platform, "platform-server.crt"),
          ELITEA_RUNTIME_PLATFORM_TLS_KEY_FILE: path.join(platform, "platform-server.key"),
          ELITEA_RUNTIME_PLATFORM_TLS_CLIENT_CA_FILE: path.join(platform, "ca.crt"),
          ELITEA_MODEL_SERVICE_CLIENTS: "dns:elitea-subapp-callback",
          ELITEA_ARTIFACT_SERVICE_CLIENTS: "dns:elitea-subapp-callback",
          ELITEA_INVENTORY_ENABLED: "true",
          ELITEA_INVENTORY_PLATFORM_GRPC_TARGET: "elitea-inventory:9443",
          ELITEA_INVENTORY_SERVER_NAME: "elitea-inventory",
          ELITEA_INVENTORY_CLIENT_CERT_FILE: cert("client.crt"),
          ELITEA_INVENTORY_CLIENT_KEY_FILE: cert("client.key"),
          ELITEA_INVENTORY_CA_FILE: cert("ca.crt"),
          ELITEA_INVENTORY_IDENTITY_SECRET: env.ELITEA_INVENTORY_IDENTITY_SECRET ?? "",
          ELITEA_INVENTORY_GIT_ALLOWLIST: "github.com,*.github.com",
          ELITEA_HTTP_ADDRESS: `${ip("elitea-main")}:${e2ePort}`,
        },
        dependsOn: [
          { name: "platform-material", condition: "completed" },
          { name: "postgres", condition: "healthy" },
          { name: "nats-health", condition: "healthy" },
          { name: "nats-bootstrap", condition: "completed" },
          { name: "oidc-mock", condition: "healthy" },
          { name: "rustfs-bucket-init", condition: "completed" },
          { name: "elitea-deepwiki", condition: "healthy" },
          { name: "elitea-inventory", condition: "healthy" },
        ],
        // The image's own probe, with the same environment (it reads ELITEA_HTTP_ADDRESS).
        health: { kind: "cmd", cmd: path.join(bin, "elitea-main"), args: ["-healthcheck"], interval: 10, timeout: 5, retries: 6, startPeriod: 15 },
      },
      subappHost("deepwiki", "DEEPWIKI", {
        ELITEA_DEEPWIKI_RUNNER: "fixture",
        ELITEA_DEEPWIKI_FIXTURE_STEP_SECONDS: "1",
        ELITEA_DEEPWIKI_MAX_PARALLEL_WORKERS: "8",
        ELITEA_DEEPWIKI_GIT_ALLOWLIST: "github.com,*.github.com",
      }),
      subappHost("inventory", "INVENTORY", {
        ELITEA_INVENTORY_RUNNER: "fixture",
        ELITEA_INVENTORY_FIXTURE_STEP_SECONDS: "1",
      }),
      {
        // The web image's nginx, as the runner user: the repository's spa.conf with the image's paths
        // rewritten (renderSpaConf), on 127.0.0.7:8080, started through the image's own entrypoint script.
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
          VITE_PUBLIC_PROJECT_ID: "99",
          ELITEA_WEB_CONFIG_JS: path.join(web, "html", "config.js"),
        },
        setup: 'mkdir -p html/docs tmp/body tmp/proxy tmp/fastcgi tmp/uwsgi tmp/scgi',
        files: [
          {
            path: path.join(web, "nginx.conf"),
            // The nginx image's main config, minus `user` (a non-root master cannot switch) and with every
            // path in the prefix. Its access log goes to a file in the data dir (the image symlinks it to stdout).
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
              renderSpaConf(readNginx("spa.conf")(), {
                spaDir: path.join(bin, "web", "app"),
                htmlDir: path.join(web, "html"),
                snippetsDir: path.join(web, "snippets"),
                listen: `${ip("elitea-web")}:${WEB_PORT}`,
              }),
          },
          ...["security-headers.conf", "docs-security-headers.conf", "opener-isolation.conf"].map((f) => ({ path: path.join(web, "snippets", f), content: readNginx(f) })),
          {
            // The spa target's placeholder docs page (apps/elitea-web/Containerfile).
            path: path.join(web, "html", "docs", "index.html"),
            content:
              '<!doctype html><meta charset="utf-8"><title>Elitea AI Docs</title>\n' +
              "<p>The documentation is in the release image. Locally: <code>npm run dev</code> in apps/elitea-docs.</p>\n",
          },
        ],
        dependsOn: [{ name: "elitea-main", condition: "healthy" }],
        health: { kind: "http", url: `http://${ip("elitea-web")}:${WEB_PORT}/app/`, interval: 3, timeout: 3, retries: 10, startPeriod: 5 },
      },
    ],
  };

  /** elitea-deepwiki / elitea-inventory: the sub-application host over its fixture runner. */
  function subappHost(app, prefix, extra) {
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
        [p("IDENTITY_SECRET")]: env[p("IDENTITY_SECRET")] ?? "",
        [p("SCRATCH_PATH")]: path.join(data(name), "scratch"),
        [p("SERVICE_LOCATION_URL")]: `grpcs://${name}:9443`,
        [p("PLATFORM_CLIENTS")]: "elitea-main",
        [p("PLATFORM_GRPC_ADDR")]: `${ip(name)}:9443`,
        [p("ADMIN_ADDR")]: `${ip(name)}:9090`,
        [p("ARTIFACT_SERVICE_URL")]: "https://elitea-main:9446",
        [p("CALLBACK_CA_FILE")]: cert("ca.crt"),
        [p("CALLBACK_CERT_FILE")]: cert("subapp-callback.crt"),
        [p("CALLBACK_KEY_FILE")]: cert("subapp-callback.key"),
      },
      setup: `mkdir -p "$${p("SCRATCH_PATH")}"`,
      // The image runs `elitea-subapp-host healthcheck`, which reads /readyz on 127.0.0.1 at the admin
      // port (cmd/elitea-subapp-host/main.go). Here the admin port is on the service's own address, so the
      // check reads the same /readyz there.
      health: { kind: "http", url: `http://${ip(name)}:9090/readyz`, interval: 5, timeout: 3, retries: 12, startPeriod: 10 },
    };
  }
}
