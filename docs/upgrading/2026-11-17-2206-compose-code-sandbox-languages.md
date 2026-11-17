# Compose code sandbox overlay now reports its languages to Main (#2206, #2208)

**What changed.** `deploy/docker-compose.sandbox.yml` now passes `ELITEA_CODE_SANDBOX_LANGUAGES` through to elitea-main
(default empty; `sandbox-compose-material.sh` prints `python,javascript,typescript,rust`). Main reports it in `GET /elitea_core/runtime_capabilities`, which turns
the agent form's **Python sandbox** (`run_code`) toggle on. Before, a Compose deployment of the sandbox had to set it
by hand, and still does unless you use the script. The new overlay `docker-compose.sandbox-postgres-tls.yml` and `deploy/scripts/sandbox-compose-material.sh`
are optional helpers; nothing changes unless you use them. No migration.

**Who is affected.** Operators who run the sandbox with `docker-compose.sandbox.yml` and serve fewer than the four
languages: the toggle would offer a language the worker cannot run.

**What to do.** Set `ELITEA_CODE_SANDBOX_LANGUAGES` on the stack to the comma-separated languages your worker's
`sandbox_runtimes` serve (for example `python,javascript`). Operators with all four need nothing.
