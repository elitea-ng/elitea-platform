---
date: 2026-11-17
order: 8
---
## Run the code sandbox with Docker Compose, proven in CI

A single-host Compose deployment can now run the code sandbox with two overlays and one script: `docker-compose.sandbox.yml` (it now also passes the served languages to Main), `docker-compose.sandbox-postgres-tls.yml` (TLS for the supervisor's receipt ledger) and `deploy/scripts/sandbox-compose-material.sh` (profiles, certificates and the worker's `sandbox_runtimes`). A new CI job runs an agent with **Python sandbox** on and checks that `run_code` returns the program's own output on the real sandbox. See [Run the code sandbox with Docker Compose](/deployment/configuration#run-the-code-sandbox-with-docker-compose). Set `ELITEA_CODE_SANDBOX_LANGUAGES` to the languages you serve (the script prints it).
