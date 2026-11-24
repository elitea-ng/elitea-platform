# Helm: securityContext for elitea-web, elitea-scheduler and otel-collector (#2941)

- **What changed:** the chart now renders a pod and a container
  `securityContext` for `web`, `scheduler` and `otelCollector`, read from
  `<component>.podSecurityContext` / `<component>.securityContext` like
  `main` and `worker`. All three get a `RuntimeDefault` seccomp profile and
  `allowPrivilegeEscalation: false`. `scheduler` and `otelCollector` also run
  as non-root with a read-only root filesystem and drop every capability
  (Pod Security Standards `restricted`). `web` (the stock nginx image, whose
  master runs as root) drops every capability but `CHOWN`,
  `NET_BIND_SERVICE`, `SETGID` and `SETUID`, and keeps a writable root
  filesystem. New `otelCollector.extraVolumes` / `extraVolumeMounts` (empty
  by default) mount extra volumes into the collector.
- **Who is affected:** every install. The pod spec of the three Deployments
  changes, so the upgrade rolls them. An install whose
  `otelCollector.config` writes to disk (a `file` exporter, a `file_storage`
  extension) now fails to start the collector on the read-only root: that
  config already needed a writable directory, which the image does not have.
  An install that runs its own `web` image needing other capabilities, or
  another user, must set them.
- **What to do:** nothing for the shipped config. For a collector that
  writes files, mount a directory for the path with
  `otelCollector.extraVolumes` (an `emptyDir`, or a PVC to keep the files)
  and `otelCollector.extraVolumeMounts`. To change or remove any default,
  set the key in your values; `null` (for example
  `web.securityContext: null`) renders no securityContext at all.
