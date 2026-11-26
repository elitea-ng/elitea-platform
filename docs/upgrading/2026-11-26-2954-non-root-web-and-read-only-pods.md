# Pods: non-root web image, read-only root filesystems (#2954, ADR-0011)

**What changed.**

- The elitea-web image runs nginx as its `nginx` user (uid 101) on port **8080**
  instead of as root on port 80. Its pid file, temp files and `config.js` are
  under `/tmp`. The chart's web container port is 8080 and the pod mounts an
  `emptyDir` at `/tmp`.
- The `elitea-web` **Service still listens on 80** (`web.service.port`) and
  targets the named `http` port, so `ELITEA_WEB_UPSTREAM` and anything that
  calls the Service are unchanged.
- Changed chart defaults:
  - `web.podSecurityContext`: adds `runAsNonRoot: true`, `runAsUser: 101`,
    `runAsGroup: 101`.
  - `web.securityContext`: adds `readOnlyRootFilesystem: true`. The four added
    capabilities (`CHOWN`, `NET_BIND_SERVICE`, `SETGID`, `SETUID`) are gone.
  - `pgbouncer.podSecurityContext`: was `{}`, now `runAsNonRoot: true`,
    `runAsUser: 70`, `runAsGroup: 70` and a `RuntimeDefault` seccomp profile.
  - `worker.securityContext.readOnlyRootFilesystem`: `false` → `true`.
- New chart value `pgbouncer.securityContext`: `allowPrivilegeEscalation: false`,
  `readOnlyRootFilesystem: true` and `capabilities.drop: [ALL]`, applied to the
  PgBouncer container and its `userlist` init container. PgBouncer gets an
  `emptyDir` at `/tmp` for its Unix socket.
- The code runner's `rust-runtime` image no longer leaves `/usr/local/cargo` and
  `/usr/local/rustup` group- or world-writable.
- The compose stacks reach elitea-web on `http://elitea-web:8080`.

**Who is affected.**

- Anything that reaches an elitea-web **pod** on port 80 directly, not through
  the Service: a custom Ingress or HTTPRoute with a numeric backend port of 80
  on a headless Service or the pod, a NetworkPolicy of your own that admits
  port 80, or a sidecar. Point it at 8080.
- A chart upgrade that keeps an elitea-web image older than this chart: that
  image starts nginx as root on port 80 and fails under the new security
  context. Upgrade the image and the chart together (`image.tag`).
- Overrides of `web.securityContext` or `web.podSecurityContext` that set
  `runAsUser: 0` or add capabilities back.
- A PgBouncer image other than the default `edoburu/pgbouncer` whose user is not
  uid 70: set `pgbouncer.podSecurityContext.runAsUser` to its uid.
- A worker toolkit or extension of your own that writes outside `/tmp`, the
  output spool or the workspace scratch.

**What to do.**

- Repoint anything from the first bullet to port 8080.
- Upgrade the chart and the images together.
- To return to the old behaviour for one component while you migrate, set its
  value back. For example, `worker.securityContext.readOnlyRootFilesystem: false`,
  or `pgbouncer.podSecurityContext: null` with `pgbouncer.securityContext: null`.
- Nothing to do for a default install.
