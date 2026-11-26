# Edge: HSTS, no Server version, strict API docs policy, container-local readiness (#2955, ADR-0036)

**What changed.**

- elitea-main sends `Strict-Transport-Security: max-age=63072000; includeSubDomains`
  on every response when `DEPLOYMENT_URL` (`global.publicUrl`) is `https`. It
  sends none on a plain-HTTP origin.
- The web image sets `server_tokens off`, and elitea-main's web edge drops the
  `Server` and `X-Powered-By` headers from every answer it relays.
- `/api/docs` loads its script from `/api/docs/app.js`; its
  Content-Security-Policy no longer allows inline scripts.
- `/readyz` and `/startupz` answer only a probe inside the container (a loopback
  peer with no `Forwarded`, `X-Forwarded-For` or `X-Real-IP` header). Every other
  caller gets `404`, whether or not the web edge is on. Before, they were hidden
  only when `web.enabled` was true. `/healthz` is unchanged and public.

**Who is affected.**

- An external monitor or load-balancer health check that polled `/readyz` or
  `/startupz` on elitea-main from outside the pod.
- A TLS-terminating proxy in front of elitea-main that strips response headers.
- A deployment whose public host has subdomains served over plain HTTP: HSTS
  with `includeSubDomains` makes browsers use HTTPS for them.

**What to do.**

- Point external monitors and load-balancer health checks at `/healthz`. The
  chart's own probes already run `elitea-main -healthcheck <path>` inside the
  container, and the compose healthchecks are unchanged.
- Make sure a proxy in front of elitea-main passes `Strict-Transport-Security`
  through.
- No chart value changes.
