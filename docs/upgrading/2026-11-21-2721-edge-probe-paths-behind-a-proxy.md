# Edge: `/readyz` and `/startupz` stay withheld behind a loopback proxy (#2721)

When elitea-main is the browser edge (`ELITEA_WEB_UPSTREAM` set: compose, and
the chart with `web.enabled`), it answers `/readyz` and `/startupz` only to a
loopback caller, because their bodies name each dependency's state. Before this
change, a reverse proxy in front of it on loopback (an nginx on the same host,
or a service-mesh sidecar, which forwards inbound traffic from `127.0.0.6`)
made every browser request a loopback caller, and the two paths were public.

- **Now:** a request that carries `Forwarded`, `X-Forwarded-For` or
  `X-Real-IP` is never treated as a loopback caller. Through such a proxy the
  two paths answer `404`.
- **Who is affected:** only a deployment that read `/readyz` or `/startupz`
  over HTTP through a proxy on the same host. The chart's probes
  (`elitea-main -healthcheck <path>`, an exec probe) and the compose
  healthchecks call elitea-main directly and are unchanged.
- **What to do:** point such a probe at elitea-main's own listener, or run
  `elitea-main -healthcheck /readyz` inside the container, as the chart does.
