# Main stays up through a PostgreSQL or NATS restart; compose restarts its daemons (#1852)

Before this change, Main exited when PostgreSQL restarted or went into recovery. On compose nothing started it again.

- **What changed.**
  - **Main stays up.** It retries in the background and resumes when PostgreSQL or NATS is back.
    - It logs one line when it loses a dependency ("Main lost its database …", "Main lost NATS …") and one when the
      dependency is back ("… again after 45s").
    - A background runner that is restarted after such a failure logs "Main restarts the &lt;runner&gt; runner after a
      dependency failure, in &lt;delay&gt;".
    - A permanent fault, such as a missing table, a closed pool or a wrong password, still ends Main. The message names
      the runner and the cause.
  - **`/readyz` reports more than the database.**

    | Situation | Answer |
    |---|---|
    | Database unreachable | 503 `not_ready` |
    | A NATS connection is closed | 503 `not_ready` |
    | NATS reconnecting, or a runner was just restarted | 200 `"status":"degraded"`, with the reason under `checks` (`nats: reconnecting`, `runners: degraded`) |

    Liveness (`/healthz`) is unchanged.
  - **Workers get "retry" during a database outage.** Workers asking Main while the database is restarting get
    `DEPENDENCY_UNAVAILABLE`, which is retryable, instead of `AUTHENTICATION_FAILED`. Sessions that are unknown,
    expired, revoked or foreign are still refused as before.
  - **Compose restarts daemons.** Every long-running service in the compose files under `deploy/` now has
    `restart: unless-stopped`. That includes Main, Web, postgres, pgbouncer and nats. One-shot jobs (migrate,
    bootstrap, init) have `restart: "no"`.
- **Who is affected.**
  - Compose operators: an exited service now comes back on its own. Use `docker compose stop` to stop one; a manual
    stop is never restarted by the policy.
  - Anyone who scrapes `/readyz`: there is a new `degraded` status. It answers 200, so compose healthchecks and Helm
    readiness keep Main in service while NATS reconnects.
- **What to do.** Nothing. A turn that was streaming while the database restarted still finishes once, but its model
  call is repeated from the last checkpoint (FU-302).
