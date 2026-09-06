#!/usr/bin/env bash
# Wrapper for the full standalone stack (deploy/docker-compose.standalone-full.yml).
#
#   certs        generate local mTLS + runtime-plane material (idempotent)
#   build        rebuild the stack's images from source (`up` reuses existing
#                tags, so a code change needs this first)
#   up           bring the stack up and wait for healthy
#   seed         E2E/DEV CONVENIENCE, not required for a working stack: schema
#                + OIDC users + RBAC fixtures — delegates to the E2E seeder
#   seed-runtime E2E/DEV CONVENIENCE, not required for a working stack: `up`
#                already authorizes the agent worker's certificate identity
#                through the runtime-session-init service (#281) before the
#                worker starts. This exists as a manual fallback for a
#                database restored from a backup that predates that row, or a
#                stack brought up with `--no-deps`
#   seed-llm     LOCAL-MODEL CONVENIENCE, not required for a working stack:
#                writes a credential the gateway serves completions with, plus
#                the chat model and the embedding model. Defaults to the
#                offline mock (#283) unless OPENAI_API_KEY/ANTHROPIC_API_KEY
#                is set. seed-llm OWNS the embedding model name (#468).
#                It writes every row THROUGH THE PRODUCT API
#                (deploy/scripts/seed-llm-api.py), so it needs elitea-main up
#                and ELITEA_CONFIGURATIONS_ENABLED on, not just postgres. Two
#                database calls survive; both are named in
#                SEED_LLM_SQL_EXCEPTIONS and the subcommand checks its own
#                command trace against that list before it exits.
#                An operator normally does this from the admin UI instead
#                (Configurations → add an LLM provider), once logged in
#   seed-index   LOCAL-MODEL CONVENIENCE, not required for a working stack:
#                vector store + an indexable toolkit, so the index-start route
#                and its SSE stream can be driven (#93). It creates the
#                embedding model row only when no row exists yet, so it can
#                never overwrite the name seed-llm wrote (#468)
#   check        verify the gateway mTLS hop, the runtime plane and the chat
#                critical path (delegates the last to chat-smoke.py), plus the
#                embedding path (embedding-path-check.sh) and a REAL elitea-sdk
#                EliteAClient (sdk-client-check.sh). It counts
#                passes, failures AND skips, and a skipped assertion exits
#                non-zero: add --allow-skips to accept an unmeasured
#                precondition on purpose (#429). It also DERIVES how many
#                assertions it is written to make, and a run that reports a
#                different number exits non-zero whatever the flags say
#                (#422, #534)
#   down         tear down (add -v yourself to drop volumes)
#
# ── Operator quick start (no seeders) ───────────────────────────────────────
# Schema bootstrap, the agentstate database, the vector extension, the
# artifact bucket, RBAC roles, a per-user PAT and a personal project are all
# automatic — the `db-init`, `elitea-migrate`, `elitea-agentstate-migrate`,
# `rustfs-bucket-init` and `runtime-session-init` compose services, plus
# first-login provisioning in cmd/elitea-main, do them. None of the seed*
# subcommands above are required to reach a working, logged-in stack:
#
#   deploy/scripts/standalone-stack.sh certs
#   deploy/scripts/standalone-stack.sh build
#   deploy/scripts/standalone-stack.sh up
#   # log in through the browser at http://localhost:${STANDALONE_PORT:-8084}/app/
#   #   (oidc-mock accepts any username; ELITEA_INITIAL_GLOBAL_ADMINS on the
#   #   elitea-main service, or E2E_ADMIN_EMAIL-shaped identities via `seed`,
#   #   decides who becomes a global administrator on first login)
#   # configure an LLM provider from the admin UI (Configurations), which
#   #   sets GATEWAY_EGRESS_ALLOWLIST as the bootstrap floor for reaching a
#   #   private model host — see deploy/README.md's compose section
#
# The seed* subcommands below remain useful for local development and CI: a
# scripted admin account and RBAC fixture set (`seed`), a credential typed
# from the shell instead of the UI (`seed-llm`), an indexable toolkit
# (`seed-index`), and a manual fallback for the workload-session row
# (`seed-runtime`).
#
# Typical E2E/dev run (all seeders, none required for the operator path above):
#   deploy/scripts/standalone-stack.sh certs
#   deploy/scripts/standalone-stack.sh up
#   deploy/scripts/standalone-stack.sh seed
#   deploy/scripts/standalone-stack.sh seed-runtime
#   deploy/scripts/standalone-stack.sh seed-llm            # offline mock
#   OPENAI_API_KEY=sk-... deploy/scripts/standalone-stack.sh seed-llm   # real provider
#   LLM_PROVIDER=vllm LLM_API_BASE=http://192.168.1.10:8000 LLM_MODEL=Qwen/Qwen3-8B \
#     deploy/scripts/standalone-stack.sh seed-llm                       # self-hosted
#   deploy/scripts/standalone-stack.sh seed-index          # index plane (#93)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
# Overridable so a second, disposable stack can be brought up for verification
# without touching a running one. Note that the oidc-mock port is fixed at 9400
# and cannot be remapped, so two stacks can only coexist if the second one drops
# that port publication with an override file.
PROJECT="${STANDALONE_PROJECT:-elitea-standalone}"
COMPOSE_F="-p ${PROJECT} -f ${REPO_ROOT}/deploy/docker-compose.standalone-full.yml"

# Overlay files, applied after the base in the order given. Every subcommand
# has to see the same set: an overlay that replaces a service's image (the Rust
# agent runtime does exactly that) would otherwise be present for `up` and
# absent for `build`, so `build` would rebuild the service the overlay replaced
# and `up` would run whatever stale tag the overlay names.
#
#   STANDALONE_WORKER=rust  deploy/scripts/standalone-stack.sh build
#
# is the supported way to select the native Rust worker; STANDALONE_OVERLAY
# takes an explicit space-separated list of paths for anything else.
OVERLAY="${STANDALONE_OVERLAY:-}"
case "${STANDALONE_WORKER:-python}" in
  python) ;;
  rust) OVERLAY="${REPO_ROOT}/deploy/docker-compose.standalone-rust-agent.yml ${OVERLAY}" ;;
  *) echo "ERROR: STANDALONE_WORKER must be python or rust (got '${STANDALONE_WORKER}')." >&2; exit 1 ;;
esac
for overlay_file in $OVERLAY; do
  if [ ! -f "$overlay_file" ]; then
    echo "ERROR: overlay '$overlay_file' does not exist." >&2
    exit 1
  fi
  COMPOSE_F="${COMPOSE_F} -f ${overlay_file}"
done

# Shared with apps/elitea-web/scripts/e2e-stack.sh: CI has the docker compose
# v2 plugin, this machine has podman.
# shellcheck source=../../apps/elitea-web/scripts/lib/compose-detect.sh
. "${REPO_ROOT}/apps/elitea-web/scripts/lib/compose-detect.sh"
# shellcheck source=lib/seeded-driver.sh
. "${REPO_ROOT}/deploy/scripts/lib/seeded-driver.sh"
# shellcheck source=../../scripts/lib/assertion-floor.sh
. "${REPO_ROOT}/scripts/lib/assertion-floor.sh"
detect_compose_bin

# The one-SQL-string reader `resolve_seeded_driver` calls, in the same shape
# sdk-client-check.sh and embedding-path-check.sh already define for
# themselves. `|| true` keeps a failing read from aborting the caller under
# `set -e`: an empty answer is a state those checks report on, not a crash.
standalone_psql_read() {
  $COMPOSE_BIN $COMPOSE_F exec -T postgres \
    psql -U elitea -d elitea -tAc "$1" 2>/dev/null | tr -d '\r' || true
}

PORT="${STANDALONE_PORT:-8084}"
# SEED_EXTRA_PROJECTS: comma-separated project ids that `seed-llm` and
# `seed-index` write model rows into IN ADDITION to project 1 and every
# personal project. The gateway resolves a model per project (a row in
# p_<id>.configuration, or a shared row in the public project), so a journey
# that runs a model call under some OTHER seeded project — the DeepWiki
# real-engine run bills project 90200, the E2E seed's — gets nothing but
# `model is not configured for this project` without its own rows.
SEED_EXTRA_PROJECTS="${SEED_EXTRA_PROJECTS:-}"

# How long `check` waits for the agent worker to join its Redis consumer group.
# The worker has no healthcheck, so `compose up -d --wait` returns before it has
# finished starting; see the wait in the "agent worker" assertion.
WORKER_JOIN_TIMEOUT="${STANDALONE_WORKER_JOIN_TIMEOUT:-90}"

# ─── The embedding model name: one variable, one resolver, one writer (#468) ──
#
# `seed-llm` and `seed-index` both need this name. Both touch ONE row:
# p_<id>.configuration with elitea_title = 'standalone-embedding'.
#
# Until #468 the two steps read two variables — LLM_EMBEDDING_MODEL and
# INDEX_EMBEDDING_MODEL — and both wrote `data`. The step that ran last
# overwrote the other step's name. The two defaults agree only for the `mock`
# provider, so the defect stayed invisible on every default run and on every
# continuous-integration job.
#
# The rule now:
#
#   1. LLM_EMBEDDING_MODEL is the ONLY variable that names the model.
#   2. `seed-llm` OWNS the name. It seeds the credential, so it is the only
#      step that knows which provider must serve the model.
#   3. `seed-index` never overwrites the name. Its conflict action leaves
#      `data` alone. It writes a name only when the row does not exist yet, so
#      it still provisions a complete index plane on its own.
#   4. The toolkit's copy of the name is READ OUT of that row, in SQL. No step
#      copies a shell variable into it, so the two cannot drift apart.
#
# Rules 3 and 4 are what make the order harmless. Do not add a second writer of
# `data` for this row: two writers put the outcome back in the hands of the
# order, and an index run then starts, becomes durable, and dies in the worker
# with a 404.

# stack_is_settled reports whether every container of this project is in its
# final good state: each long-running service running (and healthy where it has
# a healthcheck), each one-shot job exited 0.
#
# It exists because `compose up -d --wait` returns 1 on a stack that is
# COMPLETELY healthy. `--wait` waits for every container to become healthy OR to
# exit, and it counts an exit — any exit, including 0 — as a failure. This stack
# has NINE one-shot jobs (db-init, elitea-migrate, elitea-agentstate-migrate,
# rustfs-bucket-init, runtime-material, runtime-bootstrap, runtime-session-init,
# worker-spool-init, mcp-mock-trust), so a perfect `up` ends with:
#
#   Container elitea-standalone-elitea-agentstate-migrate-1  Healthy
#   container elitea-standalone-elitea-agentstate-migrate-1 exited (0)
#   Error: ... exit status 1
#
# Reproduced on Docker Compose v2.40.1 with a two-service project — one long
# runner with a healthcheck, one `echo done` — which is the whole shape.
#
# The post-check reads `compose ps --all --format json`. Measured output on that
# version is ONE JSON OBJECT PER LINE, not an array:
#
#   {"Name":"psprobe-longrunner-1","Service":"longrunner","State":"running",
#    "Health":"healthy","ExitCode":0,"Status":"Up 8 seconds"}
#   {"Name":"psprobe-oneshot-1","Service":"oneshot","State":"exited",
#    "Health":"","ExitCode":0,"Status":"Exited (0) 8 seconds ago"}
#
# Other builds emit a JSON array, so the reader accepts both. `--all` is
# required: without it `ps` hides every exited container, and a one-shot that
# failed would then be INVISIBLE — the check would pass by finding nothing,
# which is the failure mode it exists to prevent.
#
# ExitCode is 0 on a running container too, so it is read only in the exited
# branch. A container whose health is `starting` or `unhealthy` is NOT settled:
# this must never turn a real failure green.
stack_is_settled() {
  $COMPOSE_BIN $COMPOSE_F ps --all --format json 2>/dev/null | python3 -c '
import json, re, sys

raw = sys.stdin.read().strip()
if not raw:
    print("compose ps returned no containers", file=sys.stderr)
    raise SystemExit(1)

try:
    parsed = json.loads(raw)
    containers = parsed if isinstance(parsed, list) else [parsed]
except json.JSONDecodeError:
    containers = [json.loads(line) for line in raw.splitlines() if line.strip()]

exited = re.compile(r"Exited \((\d+)\)")
unsettled = []
for container in containers:
    name = container.get("Name") or container.get("Service") or "?"
    state = (container.get("State") or "").lower()
    health = (container.get("Health") or "").lower()
    status = container.get("Status") or ""
    if state == "running":
        if health in ("", "healthy"):
            continue
        unsettled.append("%s: running but %s" % (name, health or "health unknown"))
        continue
    if state == "exited":
        code = container.get("ExitCode")
        if code is None:
            match = exited.search(status)
            code = int(match.group(1)) if match else None
        if code == 0:
            continue
        unsettled.append("%s: exited (%s)" % (name, code if code is not None else status))
        continue
    unsettled.append("%s: %s" % (name, state or status or "unknown state"))

for line in unsettled:
    print(line, file=sys.stderr)
raise SystemExit(1 if unsettled else 0)
'
}

# reject_retired_embedding_var stops a run that still sets the retired variable.
# A silent no-op would hide the reason the value stopped taking effect.
reject_retired_embedding_var() {
  if [ -n "${INDEX_EMBEDDING_MODEL:-}" ]; then
    echo "ERROR: INDEX_EMBEDDING_MODEL is retired (#468)." >&2
    echo "       Set LLM_EMBEDDING_MODEL instead. One variable names the" >&2
    echo "       embedding model for seed-llm and for seed-index." >&2
    exit 1
  fi
}

# resolve_llm_provider prints the provider this stack seeds a credential for.
# `seed-index` calls it too, so both steps compute ONE default.
resolve_llm_provider() {
  local provider="${LLM_PROVIDER:-}"
  if [ -z "$provider" ] && [ -z "${OPENAI_API_KEY:-}" ] && [ -z "${ANTHROPIC_API_KEY:-}" ]; then
    provider="mock"
  fi
  printf '%s' "${provider:-open_ai}"
}

# resolve_embedding_model prints the embedding model name for one provider.
# An empty result means the provider serves no embeddings API.
resolve_embedding_model() {
  local provider="$1" name
  case "$provider" in
    mock)    name="${LLM_EMBEDDING_MODEL:-vllm/E2E-MOCK-EMBEDDING}" ;;
    # A self-hosted chat server usually serves no embeddings model, and naming
    # one that does not exist is worse than naming none: `seed-index` would
    # write a row the gateway cannot resolve. Opt in with LLM_EMBEDDING_MODEL.
    vllm)    name="${LLM_EMBEDDING_MODEL:-}" ;;
    open_ai) name="${LLM_EMBEDDING_MODEL:-text-embedding-3-small}" ;;
    # Anthropic serves no embeddings API, so there is no default to seed. Set
    # LLM_EMBEDDING_MODEL to name a model some other provider serves.
    anthropic) name="${LLM_EMBEDDING_MODEL:-}" ;;
    # `seed-llm` rejects an unknown provider before it calls this function.
    # `seed-index` calls this function first, so the same refusal must live
    # here too. Without it a typed LLM_PROVIDER makes `seed-index` seed no
    # name at all, and the operator sees only a missing row.
    *) echo "ERROR: LLM_PROVIDER must be mock, vllm, open_ai or anthropic (got '$provider')." >&2
       exit 1 ;;
  esac
  # The mock wire name must carry the provider prefix, for the same reason as
  # the chat model below: bifrost reads the provider from the model string
  # alone (ParseModelString), and its default is EMPTY.
  if [ "$provider" = "mock" ] && [ -n "$name" ]; then
    name="vllm/${name#vllm/}"
  fi
  printf '%s' "$name"
}

# ─── The SQL exception allowlist, and the assertion that enforces it ─────────
#
# `seed-llm` used to be six INSERTs into `p_<id>.configuration`. It is now a
# sequence of calls to the platform's own configuration write route
# (deploy/scripts/seed-llm-api.py). Two database reads/writes survive, and both
# are named here.
#
# A script that quietly keeps one psql call while calling itself API-driven is
# exactly the failure this change exists to remove, so the claim is CHECKED
# rather than stated: the whole subcommand runs under `set -x`, and
# `assert_seed_llm_sql_exceptions` reads that command trace back and refuses any
# line that reached psql without naming an exception on this list.
#
# The two exceptions, and what blocks each of them:
#
#   seed_llm_writer_identities
#       Which projects to seed, and WHOSE credential writes into each one.
#       Blocked on two things at once. (a) The gateway resolves a provider
#       credential from the CALLER's own personal project (#290), so a project
#       per user has to be seeded, and only the database knows which users have
#       one. (b) Writing into user X's personal project needs a token that
#       BELONGS to user X — a project role, not a global admin — and a PAT is
#       stored as an opaque uuid that only its owner ever saw. There is no API
#       that hands an operator another user's token, and there should not be.
#       Set STANDALONE_API_TOKEN to a real operator token to skip the minting
#       half of this; the project list still comes from here.
#
#   seed_llm_toolkit_embedding_repair
#       Bringing `p_<id>.elitea_tools.settings->>'embedding_model'` back into
#       step with the embedding row this step just wrote (#468). Blocked
#       because the toolkit row is not a configuration: no route in
#       internal/api/v2 writes elitea_tools.settings, and the index resolver
#       matches a run on the two names being equal. It runs only where
#       `seed-index` already created the toolkit.
#
# Adding a name here is a deliberate act. Read the two entries above and write
# down what blocks the new one before you add a third.
SEED_LLM_SQL_EXCEPTIONS="seed_llm_writer_identities seed_llm_toolkit_embedding_repair"

# db_exception runs ONE allowlisted database call.
#
# It is the only place `seed-llm` may reach psql. The `-v elitea_sql_exception`
# variable is what makes the call attributable in the command trace — psql
# ignores an unreferenced variable, and the assertion below reads it.
db_exception() {
  local exception_name="$1"
  shift
  # One traced line per invocation, and exactly one. The DEBUG trap records a
  # function CALL twice — once as the command, once on entering the function —
  # so counting the call site would double every call. This no-op is in the
  # body, so it fires once, and assert_seed_llm_sql_exceptions counts it
  # against the psql commands that must follow.
  : db_exception_invocation
  case " ${SEED_LLM_SQL_EXCEPTIONS} " in
    *" ${exception_name} "*) ;;
    *)
      echo "ERROR: '${exception_name}' is not a named SQL exception." >&2
      echo "       Named exceptions: ${SEED_LLM_SQL_EXCEPTIONS}" >&2
      echo "       Write the row through the product API, or add the name to" >&2
      echo "       SEED_LLM_SQL_EXCEPTIONS with the reason that blocks it." >&2
      exit 1
      ;;
  esac
  $COMPOSE_BIN $COMPOSE_F exec -T postgres \
    psql -v elitea_sql_exception="${exception_name}" -U elitea -d elitea "$@"
}

# assert_seed_llm_sql_exceptions reads a command trace and refuses any database
# call that is not one of the named exceptions above.
#
# THIS ASSERTION IS THE DELIVERABLE, not the green line it prints.
#
# HOW THE TRACE IS TAKEN, and why not `set -x`
# A DEBUG trap with `set -T` (functrace), writing $BASH_COMMAND to a file. Three
# reasons that beat the obvious `set -x` into a redirected stderr:
#
#   1. It cannot be suppressed by a redirection. MEASURED on bash 3.2: bash
#      prints an xtrace line for a command BEFORE applying its redirections, so
#      `db_exception … 2>/dev/null` traces the CALL and silently suppresses the
#      trace of everything inside it — including the psql line this assertion
#      exists to read. "No offending line" would then read as a pass. A DEBUG
#      trap fires whatever the command's redirections are.
#   2. It leaves the operator's stderr alone, so a failing seed still prints its
#      own error where the operator is looking.
#   3. $BASH_COMMAND is the command BEFORE expansion, so the trace holds
#      `--api-key "$TARGET_API_KEY"` and never the key itself.
#
# WHAT IT REFUSES
#   * A trace with no line at all. "No psql found" would be a true statement
#     about an empty file — the "absence reads as correctness" shape this
#     repository keeps rediscovering. An empty trace fails.
#   * Any traced command holding `psql` that does not go through db_exception,
#     which is the only place the literal `elitea_sql_exception=` appears.
#   * A `db_exception` call naming something outside the allowlist. db_exception
#     refuses it at runtime too; this is the second lock, on the trace.
#   * A mismatch between the number of db_exception calls and the number of psql
#     commands they produced — one is missing, or one arrived from elsewhere.
assert_seed_llm_sql_exceptions() {
  local trace_file="$1" traced_lines offender_count exception_name
  local call_count psql_count
  traced_lines="$(grep -c . "$trace_file" || true)"
  if [ -z "$traced_lines" ] || [ "$traced_lines" -eq 0 ]; then
    echo "ERROR: the command trace holds no traced command, so this assertion" >&2
    echo "       measured nothing. Tracing did not start." >&2
    exit 1
  fi
  offender_count=0
  while IFS= read -r traced; do
    [ -n "$traced" ] || continue
    case "$traced" in
      *elitea_sql_exception=*) continue ;;
    esac
    echo "ERROR: seed-llm reached psql outside the named exceptions:" >&2
    echo "         ${traced}" >&2
    offender_count=$((offender_count + 1))
  done <<EOF
$(grep -F 'psql' "$trace_file" || true)
EOF
  while IFS= read -r traced; do
    [ -n "$traced" ] || continue
    exception_name="$(printf '%s' "$traced" | awk '{print $2}')"
    case " ${SEED_LLM_SQL_EXCEPTIONS} " in
      *" ${exception_name} "*) ;;
      *)
        echo "ERROR: seed-llm made a database call under the unlisted name" >&2
        echo "       '${exception_name}': ${traced}" >&2
        offender_count=$((offender_count + 1))
        ;;
    esac
  done <<EOF
$(grep -E '^[[:space:]]*db_exception[[:space:]]' "$trace_file" || true)
EOF
  # One db_exception call produces exactly one psql command. The DEBUG trap
  # records both, so the two counts must agree: a shortfall means a call
  # reached the database without being recorded, and a surplus means a psql
  # command arrived from somewhere that is not db_exception.
  call_count="$(grep -cF ': db_exception_invocation' "$trace_file" || true)"
  psql_count="$(grep -cF 'elitea_sql_exception=' "$trace_file" || true)"
  if [ "${call_count:-0}" -ne "${psql_count:-0}" ]; then
    echo "ERROR: ${call_count} allowlisted database call(s) were traced, and" >&2
    echo "       ${psql_count} psql command(s) were. The two must agree." >&2
    offender_count=$((offender_count + 1))
  fi
  if [ "$offender_count" -ne 0 ]; then
    echo "ERROR: ${offender_count} database call(s) bypassed the product API." >&2
    echo "       seed-llm writes configuration rows through" >&2
    echo "       deploy/scripts/seed-llm-api.py. Only these names may reach the" >&2
    echo "       database directly: ${SEED_LLM_SQL_EXCEPTIONS}" >&2
    exit 1
  fi
  echo "→ SQL exception check: ${traced_lines} traced command(s), ${call_count} database"
  echo "  call(s), every one of them named: ${SEED_LLM_SQL_EXCEPTIONS}"
}

# seed_llm_main is the body of `seed-llm`. It is a function, and not the case
# arm itself, because the arm runs it inside a traced subshell so that
# assert_seed_llm_sql_exceptions can read back what it actually ran.
seed_llm_main() {
  reject_retired_embedding_var
  PROVIDER="$(resolve_llm_provider)"
  case "$PROVIDER" in
    mock)
      # `vllm`, not `open_ai`, and that is load-bearing rather than a label:
      # bifrost only lifts its SSRF guard for the self-hosted classes
      # (internal/account/account.go:235 — schemas.VLLM and schemas.Ollama),
      # so a private compose address must be one of them. api_base carries no
      # /v1: bifrost appends /v1/chat/completions itself. A distinct host also
      # keeps it clear of the self-referential-origin guard on the platform's
      # own /llm origin.
      API_KEY="mock-key-not-used"; MODEL="${LLM_MODEL:-E2E-MOCK-MODEL}"
      API_BASE="http://llm-mock:8090"; CRED_TYPE="vllm"
      ;;
    vllm)
      # A REAL self-hosted OpenAI-compatible server (vLLM, Ollama, LM Studio,
      # TGI…) at an address on the operator's own network. Same credential
      # class as `mock` and for the same load-bearing reason — bifrost lifts
      # its SSRF guard only for the self-hosted classes (account.go:235), so a
      # private address MUST be seeded as `vllm`; an `open_ai` credential
      # pointing at 192.168.x.x is refused by the dialer whatever the
      # allowlist says.
      #
      # LLM_API_BASE carries NO /v1 — bifrost appends /v1/chat/completions
      # itself. Passing the /v1 the server's own docs show produces
      # /v1/v1/chat/completions and a 404 that looks like a routing bug.
      #
      # LLM_API_KEY is optional because most self-hosted servers ignore it,
      # but it cannot be EMPTY: the guard below treats an empty key as
      # "operator forgot to pass one" and refuses.
      API_KEY="${LLM_API_KEY:-not-used-by-self-hosted}"; KEY_VAR="LLM_API_KEY"
      MODEL="${LLM_MODEL:-}"; API_BASE="${LLM_API_BASE:-}"; CRED_TYPE="vllm"
      if [ -z "$API_BASE" ] || [ -z "$MODEL" ]; then
        echo "ERROR: LLM_PROVIDER=vllm needs LLM_API_BASE and LLM_MODEL." >&2
        echo "       e.g. LLM_PROVIDER=vllm LLM_API_BASE=http://192.168.1.10:8000 \\" >&2
        echo "            LLM_MODEL=Qwen/Qwen3-8B $0 seed-llm" >&2
        echo "       LLM_API_BASE must NOT end in /v1 — bifrost appends it." >&2
        exit 1
      fi
      case "$API_BASE" in
        */v1|*/v1/) echo "ERROR: strip the trailing /v1 from LLM_API_BASE ('$API_BASE')." >&2; exit 1 ;;
      esac
      ;;
    open_ai)   API_KEY="${OPENAI_API_KEY:-}";    KEY_VAR="OPENAI_API_KEY";    MODEL="${LLM_MODEL:-gpt-4o-mini}"; API_BASE=""; CRED_TYPE="open_ai" ;;
    anthropic) API_KEY="${ANTHROPIC_API_KEY:-}"; KEY_VAR="ANTHROPIC_API_KEY"; MODEL="${LLM_MODEL:-claude-sonnet-4-5}"; API_BASE=""; CRED_TYPE="anthropic" ;;
    *) echo "ERROR: LLM_PROVIDER must be mock, vllm, open_ai or anthropic (got '$PROVIDER')." >&2; exit 1 ;;
  esac
  # One resolver, shared with `seed-index`. Read the block above resolve_llm_provider.
  EMBEDDING_MODEL="$(resolve_embedding_model "$PROVIDER")"
  if [ -z "$API_KEY" ]; then
    echo "ERROR: \$$KEY_VAR is empty. Usage: $KEY_VAR=... $0 seed-llm" >&2
    exit 1
  fi
  if [ "$PROVIDER" = "mock" ] || [ "$PROVIDER" = "vllm" ]; then
    # The wire name must carry the provider prefix. bifrost resolves the
    # provider from the model string alone (ParseModelString) with an EMPTY
    # default, so a bare `E2E-MOCK-MODEL` reaches core with no provider and
    # never gets as far as the credential. The chat model row below is
    # therefore titled `vllm/E2E-MOCK-MODEL`, which is what the model picker
    # hands to the SDK and what the SDK puts on the wire.
    #
    # resolve_embedding_model applies the same prefix to the embedding name.
    MODEL="vllm/${MODEL#vllm/}"
  fi

  # ── How this step talks to the platform ───────────────────────────────────
  #
  # Through the published edge, exactly as any API client would. The write
  # route is mounted only when ELITEA_CONFIGURATIONS_ENABLED is true, which
  # deploy/docker-compose.standalone-full.yml sets; on a Kubernetes install use
  # deploy/helm/elitea/values-auth-minimal.yaml (or values-standalone.yaml),
  # which turn on the same flag and the production authentication it requires.
  API_BASE_URL="${STANDALONE_API_BASE_URL:-http://localhost:${PORT}}"
  # The deployment's PAT signing key. `certs` writes it here; override it when
  # the material lives elsewhere (a Kubernetes Secret, another checkout).
  # Ignored entirely when STANDALONE_API_TOKEN is set.
  PAT_SIGNING_KEY="${STANDALONE_PAT_SIGNING_KEY:-${REPO_ROOT}/deploy/certs/runtime/auth-pat-signing-key}"

  # ── Exception 1 of 2: whose credential writes into which project ──────────
  #
  # Read the SEED_LLM_SQL_EXCEPTIONS block for what blocks this. In short: the
  # gateway resolves a credential from the CALLER's own personal project
  # (#290), so every personal project needs its own rows, and writing into
  # user X's project needs a token that belongs to user X.
  #
  # The row is selected on the PERMISSION the write route gates on, not on a
  # role name: `configurations.configuration.create` resolved in DEFAULT mode
  # is exactly what middleware.RequireResolvedPermissions asks for. A role
  # rename cannot silently empty this list.
  #
  # Restricted to schemas that actually exist, for the same reason as before: a
  # project row whose tenant migrations have not run yet would fail rather than
  # be skipped.
  SEED_WRITERS="$(db_exception seed_llm_writer_identities -tAc \
      "SELECT DISTINCT ON (p.id) p.id || ' ' || t.uuid
         FROM centry.project p
         JOIN public.auth_core__project_user_role pur ON pur.project_id = p.id
         JOIN public.auth_core__project_role pr
           ON pr.id = pur.role_id AND pr.project_id = p.id
         JOIN public.auth_core__project_role_permission prp
           ON prp.role_id = pr.id AND prp.project_id = p.id
          AND prp.permission = 'configurations.configuration.create'
         JOIN public.auth_core__token t
           ON t.user_id = pur.user_id AND t.uuid IS NOT NULL
          AND (t.expires IS NULL OR t.expires > (clock_timestamp() AT TIME ZONE 'UTC'))
        WHERE (p.id = 1 OR p.name LIKE 'project\_user\_%'
               OR p.id = ANY(string_to_array(NULLIF('${SEED_EXTRA_PROJECTS:-}', ''), ',')::int[]))
          AND EXISTS (SELECT 1 FROM pg_catalog.pg_namespace
                       WHERE nspname = 'p_' || p.id::text)
        ORDER BY p.id, pur.user_id, t.id" | tr -d '\r')"
  if [ -z "$SEED_WRITERS" ]; then
    echo "ERROR: no project has both a tenant schema and a token that may write" >&2
    echo "       configurations into it. Run:  $0 seed" >&2
    echo "       (that step provisions the schemas, the roles and the PATs)" >&2
    exit 1
  fi
  if [ ! -r "$PAT_SIGNING_KEY" ] && [ -z "${STANDALONE_API_TOKEN:-}" ]; then
    echo "ERROR: no PAT signing key at '${PAT_SIGNING_KEY}'." >&2
    echo "       Run '$0 certs', point STANDALONE_PAT_SIGNING_KEY at the file," >&2
    echo "       or set STANDALONE_API_TOKEN to a token you minted in the UI." >&2
    exit 1
  fi

  TARGET_PROJECTS=""
  while IFS= read -r writer_row; do
    [ -n "$writer_row" ] || continue
    TARGET="$(printf '%s' "$writer_row" | awk '{print $1}')"
    WRITER_PAT="$(printf '%s' "$writer_row" | awk '{print $2}')"
    [ -n "$TARGET" ] && [ -n "$WRITER_PAT" ] || continue
    TARGET_PROJECTS="${TARGET_PROJECTS} ${TARGET}"

    # The mock's key NAMES THE PROJECT (#470).
    #
    # The gateway resolves the credential from one project's own rows, and the
    # key it then sends upstream is the only part of that decision the upstream
    # can see. The mock records it in its request journal, so a test can assert
    # WHICH PROJECT the gateway resolved, not merely that a vector came back.
    # One shared key for every project would make that unknowable.
    #
    # The value is not a secret, and the mock ignores it for authentication.
    # A real provider keeps one key for all projects: the key belongs to the
    # operator, and this stack gives no project a key of its own.
    TARGET_API_KEY="$API_KEY"
    if [ "$PROVIDER" = "mock" ]; then
      TARGET_API_KEY="mock-key-project-${TARGET}"
    fi

    # The public-project pair (#458, asserted by #470). TWO extra embedding
    # rows, in project 1 only, under titles no other project holds — so a
    # caller in another project can reach them only through the
    # platform-shared scope. They are a matched pair and the pair is the
    # point: `standalone-shared-embedding` (shared) must dispatch for any
    # caller, `standalone-private-embedding` must be invisible to every other
    # project. One row alone would prove only half, and a gateway that read the
    # public project with no `shared` predicate would pass the first assertion
    # and fail the second — a tenant-isolation fault, not a routing one.
    # deploy/scripts/embedding-path-check.sh asserts both directions.
    PUBLIC_PAIR=""
    if [ "$TARGET" = "1" ]; then
      PUBLIC_PAIR="--public-embedding-pair"
    fi

    # A token the operator minted in the product wins over re-signing this
    # deployment's own PAT row. It is only useful where ONE identity holds
    # `configurations.configuration.create` in every target project, which a
    # per-user personal project by definition does not — so the minted form
    # stays the default.
    SEED_AUTH_ARGS=(--pat-uuid "$WRITER_PAT" --signing-key "$PAT_SIGNING_KEY")
    if [ -n "${STANDALONE_API_TOKEN:-}" ]; then
      SEED_AUTH_ARGS=(--token "$STANDALONE_API_TOKEN")
    fi

    echo "→ Seeding a $PROVIDER credential (type=$CRED_TYPE) + model rows into project ${TARGET} through the API…"
    python3 "${REPO_ROOT}/deploy/scripts/seed-llm-api.py" \
      --base-url "$API_BASE_URL" \
      --project "$TARGET" \
      "${SEED_AUTH_ARGS[@]}" \
      --credential-title "standalone-${CRED_TYPE}" \
      --credential-type "$CRED_TYPE" \
      --api-key "$TARGET_API_KEY" \
      --api-base "$API_BASE" \
      --model "$MODEL" \
      --embedding-model "$EMBEDDING_MODEL" \
      $PUBLIC_PAIR

    # ── Exception 2 of 2: the toolkit's copy of the embedding model name ────
    #
    # The index plane keeps a SECOND copy of the name, in the toolkit's
    # settings, and `seed-llm` OWNS the name (#468). The resolver matches an
    # index run on configuration.data->>'name'
    # (db/queries/configurations.sql, FindCurrentEmbeddingConfigurations), so a
    # stale toolkit copy admits the run, makes it durable, and then kills it in
    # the worker with a 404.
    #
    # It is SQL because elitea_tools is not a configuration: no route in
    # internal/api/v2 writes elitea_tools.settings. The new value is READ OUT
    # of the row the API just wrote rather than copied from a shell variable,
    # so the two names cannot disagree — `seed-index` builds the same setting
    # the same way.
    if [ -n "$EMBEDDING_MODEL" ]; then
      db_exception seed_llm_toolkit_embedding_repair -v ON_ERROR_STOP=1 \
        -v schema="p_${TARGET}" <<'SQL'
UPDATE :"schema".elitea_tools AS t
   SET settings = jsonb_set(t.settings, '{embedding_model}', to_jsonb(c.data->>'name'))
  FROM :"schema".configuration AS c
 WHERE c.elitea_title = 'standalone-embedding'
   AND t.name = 'standalone-artifact-index'
   AND t.settings ? 'embedding_model'
   AND t.settings->>'embedding_model' IS DISTINCT FROM c.data->>'name';
SQL
    fi
  done <<EOF
$SEED_WRITERS
EOF

  echo "→ Seeded projects:${TARGET_PROJECTS}. Model alias: $MODEL"

  # Name BOTH names (#380). A caller sends the catalogue name; the gateway
  # maps it onto the provider's own name before it dispatches. When the two
  # disagree the failure is a bare 404 that names only one of them.
  if [ -n "$EMBEDDING_MODEL" ]; then
    echo "→ Embedding model: $EMBEDDING_MODEL"
    echo "   The catalogue stores it as elitea_title 'standalone-embedding'"
    echo "   with data.name '${EMBEDDING_MODEL}'. A caller may send either name."
    echo "   The gateway reports the name AFTER it splits the provider prefix,"
    echo "   so a 404 for '${EMBEDDING_MODEL#*/}' names this same row."
  else
    # A self-hosted provider points the credential at a PRIVATE host, and the
    # gateway refuses private egress unless that exact host:port is on its
    # allowlist (SSRF guard). Nothing downstream says so: the turn runs, the
    # agent starts, the model call comes back a bare
    # `500 … EGRESS_HOST_NOT_ALLOWED`, and the UI shows an empty answer. The
    # allowlist lives on the GATEWAY's environment, so seeding cannot set it —
    # but it can say which value is now required, and whether it is missing.
    if [ "$PROVIDER" = "vllm" ] || [ "$PROVIDER" = "ollama" ]; then
      EGRESS_NEEDED="$(printf '%s' "$LLM_API_BASE" | sed -E 's#^https?://##; s#/.*$##')"
      # `ENGINE` is only set inside `check`; derive it the same way here
      # (the container engine is the first word of the compose command).
      EGRESS_HAVE="$("${COMPOSE_BIN%% *}" inspect "${PROJECT}-elitea-llm-gateway-1" \
          --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null \
          | sed -n 's/^GATEWAY_EGRESS_ALLOWLIST=//p')"
      case ",${EGRESS_HAVE}," in
        *",${EGRESS_NEEDED},"*)
          echo "→ gateway egress already allows '${EGRESS_NEEDED}'." ;;
        *)
          echo "   ! the gateway does NOT allow egress to '${EGRESS_NEEDED}' (allowlist: '${EGRESS_HAVE:-<unset>}')."
          echo "     Every turn will reach the model and come back 500 EGRESS_HOST_NOT_ALLOWED."
          echo "     Restart the gateway with it allowed:"
          echo "       GATEWAY_EGRESS_ALLOWLIST=\"${EGRESS_HAVE:-llm-mock:8090},${EGRESS_NEEDED},10.0.0.0/8,172.16.0.0/12,192.168.0.0/16\" \\"
          echo "         $0 up          # or: compose up -d --force-recreate elitea-llm-gateway" ;;
      esac
    fi
    echo "→ NO embedding model seeded: provider '$PROVIDER' serves no embeddings API."
    echo "   The embedding hop will answer 404 and '$0 check' will FAIL."
    echo "   Set LLM_EMBEDDING_MODEL to a model some provider serves, then"
    echo "   run '$0 seed-llm' again."
  fi
  if [ "$PROVIDER" = "mock" ]; then
    echo "  (offline mock — no provider key used. Set OPENAI_API_KEY to seed a real one instead.)"
  fi
}

case "${1:-}" in
  certs)
    # Two independent trust roots, minted by two scripts: the gateway CA for the
    # elitea-main → Bifrost egress hop, and the runtime CA that signs workload
    # identities authorized to call into elitea-main. Merging them would let an
    # egress client cert authenticate as a workload.
    "${REPO_ROOT}/deploy/scripts/gen-gateway-certs.sh"
    # After the gateway's: the DeepWiki script REUSES a fresh deploy/certs CA
    # and client certificate, and only issues the provider's server cert —
    # one trust root for both mTLS hops out of elitea-main.
    DEEPWIKI_CERT_SANS="DNS:elitea-deepwiki,DNS:localhost,IP:127.0.0.1" \
      "${REPO_ROOT}/deploy/scripts/gen-deepwiki-certs.sh"
    # The second provider's server cert, off the same CA and beside the same
    # client cert. Its own script, because a deployment may run one provider
    # and not the other.
    INVENTORY_CERT_SANS="DNS:elitea-inventory,DNS:localhost,IP:127.0.0.1" \
      "${REPO_ROOT}/deploy/scripts/gen-inventory-certs.sh"
    exec "${REPO_ROOT}/deploy/scripts/gen-runtime-certs.sh"
    ;;

  up)
    # Fail early with an actionable message rather than letting elitea-main die
    # in a restart loop on a missing client cert.
    if [ ! -f "${REPO_ROOT}/deploy/certs/client.crt" ]; then
      echo "ERROR: mTLS material missing. Run: $0 certs" >&2
      exit 1
    fi
    # Same, for the runtime plane. Without it runtime-material aborts and every
    # dependent service is stuck in `created` with no obvious cause.
    if [ ! -f "${REPO_ROOT}/deploy/certs/runtime/runtime-ca.crt" ]; then
      echo "ERROR: runtime-plane material missing. Run: $0 certs" >&2
      exit 1
    fi
    if [ ! -f "${REPO_ROOT}/deploy/certs/deepwiki-server.crt" ]; then
      echo "ERROR: DeepWiki provider material missing. Run: $0 certs" >&2
      exit 1
    fi
    if [ ! -f "${REPO_ROOT}/deploy/certs/inventory-server.crt" ]; then
      echo "ERROR: Inventory provider material missing. Run: $0 certs" >&2
      exit 1
    fi
    # oidc-mock's published port must equal its container port (the issuer is
    # derived from the Host header), so 9400 cannot be remapped and the E2E
    # stack cannot be up at the same time. Detect that here: the raw failure is
    # an opaque "proxy already running" from the podman machine.
    #
    # Only a conflict if the listener is NOT this project's own oidc-mock:
    # `compose up -d --wait` is meant to be safely re-runnable, and without
    # this exemption a second `up` against an already-healthy standalone stack
    # would find its own container on 9400 and misreport it as the E2E stack.
    if ! $COMPOSE_BIN $COMPOSE_F ps --format '{{.Names}}' 2>/dev/null | grep -q 'oidc-mock' \
       && command -v lsof &>/dev/null && lsof -nP -iTCP:9400 -sTCP:LISTEN &>/dev/null; then
      echo "ERROR: port 9400 (oidc-mock) is already in use." >&2
      echo "       The E2E stack cannot run alongside this one. Stop it with:" >&2
      echo "         apps/elitea-web/scripts/e2e-stack.sh down" >&2
      exit 1
    fi
    echo "→ Bringing up the full standalone stack (${COMPOSE_BIN})…"
    # `--wait` returns 1 on a stack that is COMPLETELY healthy, because it reads
    # a one-shot job's exit as a failure even when the code is 0. This stack has
    # nine of them, so `up` reported failure on every good run and every caller
    # — an operator, a CI step, a `&&` chain — read a working stack as broken.
    # See stack_is_settled for the measured behaviour and the ps output shape.
    #
    # The exit status is not simply discarded. It is re-derived from the
    # container states, so a REAL failure — a service stuck `starting`, an
    # unhealthy container, a migration that exited non-zero — still exits 1.
    up_status=0
    $COMPOSE_BIN $COMPOSE_F up -d --wait || up_status=$?
    if [ "$up_status" -ne 0 ]; then
      if stack_is_settled; then
        echo "→ compose --wait exited ${up_status} because a one-shot job finished;"
        echo "   every service is healthy and every job exited 0."
      else
        echo "ERROR: the stack did not come up. The containers above are not settled." >&2
        $COMPOSE_BIN $COMPOSE_F ps --all >&2 || true
        exit "$up_status"
      fi
    fi
    echo "→ Stack ready."
    echo "     web app       http://localhost:${PORT}/app/"
    echo "     admin console http://localhost:${PORT}/admin/app/"
    echo "     API           http://localhost:${PORT}/api/v2"
    echo "     OIDC provider http://localhost:9400"
    echo "     gateway       https://localhost:${STANDALONE_GATEWAY_PORT:-8085} (mTLS)"
    echo "   Next: $0 seed && $0 seed-runtime && $0 seed-llm && $0 check"
    ;;

  build)
    # `up` reuses whatever image already carries the tag, so a source change
    # reaches a running stack only when the build is asked for explicitly.
    # Verifying a code change against this stack without it silently tests the
    # PREVIOUS build — which has cost this effort several confusing runs.
    $COMPOSE_BIN $COMPOSE_F build "${@:2}"
    ;;

  down)
    $COMPOSE_BIN $COMPOSE_F down --remove-orphans "${@:2}"
    ;;

  seed)
    # Reuse the E2E seeder verbatim — schema bootstrap, oidc-mock users, RBAC
    # grants and the Fernet vault blobs are identical between the two stacks.
    #
    # It targets a compose project via E2E_PROJECT (#228/#236 renamed this from
    # STACK_PROJECT and dropped STACK_COMPOSE_FILE entirely — the seeder's
    # container lookups run `podman ps` / `docker ps` unscoped and filter by
    # project-name PREFIX via scripts/lib/container-lookup.sh, so which compose
    # file produced the containers never mattered; only the project name does).
    # Passing the old STACK_PROJECT var here would silently no-op: the seeder
    # would fall back to its own E2E_PROJECT default (`elitea-e2e`) and, if an
    # E2E stack happened to be up at the same time, seed ITS database instead of
    # this one's — exactly the hazard this reconciliation exists to close.
    echo "→ Seeding base data via the E2E seeder…"
    E2E_PROJECT="$PROJECT" \
    COMPOSE_BIN="$COMPOSE_BIN" \
      "${REPO_ROOT}/apps/elitea-web/scripts/e2e-stack.sh" seed
    # The seeder inserts projects (the chat personas' personal projects, #290,
    # and the admin fixtures), and a project without its `p_<id>` schema is
    # worse than absent: the resolver hands it out and the very next query
    # fails on a missing relation. elitea-migrate enumerates centry.project, so
    # re-running it here creates exactly the schemas the seed just added —
    # otherwise they appear only after the NEXT `up`, which is what the migrate
    # service's own comment already anticipates.
    echo "→ Applying tenant migrations for newly seeded projects…"
    $COMPOSE_BIN $COMPOSE_F run --rm --no-deps elitea-migrate -all-tenants
    ;;

  seed-runtime)
    # Authorize the agent worker's certificate identity.
    #
    # NORMALLY UNNECESSARY: `up` already runs this same authorization through
    # the `runtime-session-init` compose service (deploy/runtime/
    # provision-runtime-session.sh), ordered before the worker starts. This
    # subcommand is a manual fallback for a database restored from a backup
    # that predates that row, or a worker brought up with `--no-deps`, not a
    # required step of a fresh install.
    #
    # Nothing in the repository inserts into elitea_runtime.workload_sessions —
    # by design. WorkloadSessionsRepository "exposes no process-local
    # registration or fallback allowlist"; sessions are provisioned by the
    # deployment control plane before a worker connects, so a worker can never
    # mint its own authority. In this stack the control plane is this command
    # (or, ordinarily, `runtime-session-init`).
    #
    # The tuple must match exactly what workloadauth presents: the identity
    # derived from the client certificate (one SPIFFE URI SAN — see
    # internal/auth/workloadidentity), plus the session and producer ids the
    # worker sends. All three are cross-checked in one query, so a mismatch in
    # any of them is an indistinguishable "unauthorized".
    #
    # Keep these three values in sync with the worker's runtime.json (#282) and
    # with RUNTIME_WORKER_SPIFFE_ID in gen-runtime-certs.sh.
    echo "→ seed-runtime is normally unnecessary: 'up' already authorizes the"
    echo "  worker through the runtime-session-init service. Re-running this"
    echo "  now (harmless — the upsert is idempotent and re-stamps expires_at)…"
    WORKER_IDENTITY="${RUNTIME_WORKER_SPIFFE_ID:-spiffe://elitea.standalone/ns/default/sa/agent-worker}"
    WORKER_SESSION_ID="${RUNTIME_WORKER_SESSION_ID:-standalone-agent-worker-1}"
    WORKER_PRODUCER_ID="${RUNTIME_WORKER_PRODUCER_ID:-standalone-agent-worker-1}"
    echo "→ Authorizing workload identity ${WORKER_IDENTITY}…"
    $COMPOSE_BIN $COMPOSE_F exec -T postgres \
      psql -v ON_ERROR_STOP=1 -U elitea -d elitea \
        -v identity="$WORKER_IDENTITY" \
        -v session="$WORKER_SESSION_ID" \
        -v producer="$WORKER_PRODUCER_ID" <<'SQL'
-- issued_at defaults to clock_timestamp(); the verifier requires
-- issued_at <= now() < expires_at AND revoked_at IS NULL.
INSERT INTO elitea_runtime.workload_sessions
    (workload_session_id, workload_identity, producer_id, expires_at)
VALUES
    (:'session', :'identity', :'producer', clock_timestamp() + INTERVAL '365 days')
ON CONFLICT (workload_session_id) DO UPDATE
    SET workload_identity = EXCLUDED.workload_identity,
        producer_id       = EXCLUDED.producer_id,
        expires_at        = EXCLUDED.expires_at,
        revoked_at        = NULL;
SQL
    echo "→ Authorized. session=${WORKER_SESSION_ID} producer=${WORKER_PRODUCER_ID}"

    # Give every seeded user an active PAT.
    #
    # Not optional, and not obvious: when the worker claims an execution it POSTs
    # to the content listener for a client token, which resolves through
    # ActorTokenIssuer → LocalIssuer.IssueToken. That issuer "never creates,
    # rotates, or stores a PAT" — it re-signs an EXISTING active one
    # (GetActivePATForUser). With no row the claim fails at stage
    # `actor_pat_issuance` and the execution dies before any model call, with no
    # hint that a database row is what was missing.
    #
    # The E2E seeder creates users but no auth_core__token rows, so this is the
    # only place it happens. The uuid is the credential the Go side re-signs with
    # the PAT HS512 key from the runtime material; expires NULL means no expiry,
    # which the issuer's query accepts.
    echo "→ Issuing PATs for users without one…"
    $COMPOSE_BIN $COMPOSE_F exec -T postgres \
      psql -v ON_ERROR_STOP=1 -U elitea -d elitea <<'SQL'
INSERT INTO public.auth_core__token (uuid, expires, user_id, name)
SELECT gen_random_uuid()::text, NULL, u.id, 'standalone-runtime'
FROM public.auth_core__user AS u
WHERE u.suspended = false
  AND NOT EXISTS (
      SELECT 1 FROM public.auth_core__token AS t
      WHERE t.user_id = u.id AND t.uuid IS NOT NULL
        AND (t.expires IS NULL OR t.expires > (clock_timestamp() AT TIME ZONE 'UTC'))
  );
SQL
    ;;

  seed-llm)
    # The credential the gateway serves completions with, the chat model, the
    # model-picker row and the embedding model — all written THROUGH THE
    # PRODUCT'S OWN API (deploy/scripts/seed-llm-api.py), not with INSERT.
    #
    # WHAT CHANGED, AND WHY IT IS NOT COSMETIC
    # This step used to run six INSERTs into `p_<id>.configuration`, each of
    # them asserting `status_ok = true`. That column is the platform's own
    # answer to "may a runtime use this row?", reached by expanding the row's
    # declared references and redeeming its hidden secrets. Writing it by hand
    # made a fresh install's seed structurally different from a credential a
    # user saves in the product, and stored `api_key` in a column
    # migrations/shared/0072 grants the project VIEWER role to read.
    #
    # The write route decides `status_ok` inline, in the same request, from the
    # same admission decision the configuration lifecycle reaches (#457). That
    # is what ELITEA_CONFIGURATIONS_ENABLED composes — NOT
    # ELITEA_CONFIGURATIONS_MUTATION_ENABLED, which stays off here and in every
    # deployment file: it composes a second write route that wins the path from
    # this one, takes a different request shape, and makes `status_ok`
    # asynchronous.
    #
    # THE ONE BEHAVIOURAL DIFFERENCE, and it is a required one: every model row
    # now carries a `data.ai_credentials` link to the credential written beside
    # it. A model row that names no credential is unmanaged — admission declines
    # to decide, the row is stored with `status_ok = false`, and every reader
    # ignores it. Measured on a running stack: the same POST with the link
    # answers `"status_ok":true`, without it `"status_ok":false`.
    #
    # THE THREE (now four) SECTIONS are unchanged and still easy to confuse:
    # `ai_credentials` (what the gateway's Account reads), `llm`/`llm_model`
    # (the chat catalogue), `models` (what the web model picker reads) and
    # `embedding` (the embedding route). seed-llm writes all four.
    #
    # Mock mode (#283) is still the DEFAULT when no provider key is present, so
    # a fresh stack completes a model turn offline. With OPENAI_API_KEY (or
    # ANTHROPIC_API_KEY, or an explicit LLM_PROVIDER) set, the real credential
    # is seeded and the mock is not, so nothing can quietly answer a request the
    # operator meant to bill to a provider.
    #
    # Read SEED_LLM_SQL_EXCEPTIONS above for the two database calls that
    # survive and what blocks each of them. The command trace of this whole
    # subcommand is checked against that list before it exits.
    SEED_LLM_TRACE="$(mktemp "${TMPDIR:-/tmp}/elitea-seed-llm-trace.XXXXXX")"
    trap 'rm -f "$SEED_LLM_TRACE"' EXIT
    # `set -T` makes the DEBUG trap fire inside functions, which is where every
    # database call lives. $BASH_COMMAND is the command BEFORE expansion, so
    # this file holds no provider key. Read assert_seed_llm_sql_exceptions for
    # why this rather than `set -x`.
    set -T
    trap 'printf "%s\n" "$BASH_COMMAND" >>"$SEED_LLM_TRACE"' DEBUG
    set +e
    seed_llm_main
    seed_llm_status=$?
    set -e
    trap - DEBUG
    set +T
    assert_seed_llm_sql_exceptions "$SEED_LLM_TRACE"
    exit "$seed_llm_status"
    ;;
  seed-index)
    # Provision the INDEX plane's data (#93 Surface A). The transport half is
    # compose + migrations; this is the three rows without which
    # POST /api/v2/elitea_core/test_toolkit_tool/prompt_lib/{projectID}
    # cannot get past resolution.
    #
    # Project 1 AND every personal project, exactly as `seed-llm` above, and
    # for a related reason. `models.applications.tool.patch` — the index-start
    # route's permission — is checked against the project in the URL, while the
    # embedding hop underneath resolves the CALLER's PERSONAL project. So a run
    # can only complete in a project that is BOTH: the E2E seeder now grants
    # the index permissions inside the driver persona's personal project, and
    # this seeds that project's rows. Seeding project 1 alone left the only
    # permitted callers without a personal project and the run died on
    # `project_not_resolved` before the toolkit was ever loaded.
    #
    # Read the KNOWN LIMIT at the end of this block before concluding that a
    # run which indexes nothing means a broken stack.
    reject_retired_embedding_var
    TOOLKIT_ID="${INDEX_TOOLKIT_ID:-9002}"
    # The SAME resolver `seed-llm` uses (#468). This value is a BOOTSTRAP only:
    # the statement below writes it when the row does not exist yet, and leaves
    # an existing name alone.
    EMBEDDING_MODEL="$(resolve_embedding_model "$(resolve_llm_provider)")"
    INDEX_TARGETS="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
        psql -U elitea -d elitea -tAc \
        "SELECT p.id FROM centry.project p
          WHERE (p.id = 1 OR p.name LIKE 'project\_user\_%'
                 OR p.id = ANY(string_to_array(NULLIF('${SEED_EXTRA_PROJECTS:-}', ''), ',')::int[]))
            AND EXISTS (SELECT 1 FROM pg_catalog.pg_namespace
                         WHERE nspname = 'p_' || p.id::text)
          ORDER BY p.id" 2>/dev/null | tr -d '\r')"
    if [ -z "$INDEX_TARGETS" ]; then
      echo "ERROR: no tenant schema to seed into. Run: $0 seed" >&2
      exit 1
    fi
    for TARGET in $INDEX_TARGETS; do
    echo "→ Seeding the index plane into project ${TARGET} (toolkit id ${TOOLKIT_ID})…"
    $COMPOSE_BIN $COMPOSE_F exec -T postgres \
      psql -v ON_ERROR_STOP=1 -U elitea -d elitea \
        -v toolkit="$TOOLKIT_ID" -v embedding="$EMBEDDING_MODEL" \
        -v pid="$TARGET" -v schema="p_${TARGET}" <<'SQL'
-- 1. The vector store the run writes into.
--
-- `postgresql+psycopg://` is NOT interchangeable with `postgresql://` here even
-- though internal/infra/pgvector/index_meta.go accepts both. The SDK's
-- runtime/langchain/store_manager.py:_parse_connection_string only assigns its
-- `url` variable inside `if conn_str.startswith("postgresql+psycopg://")`, so
-- any other scheme reaches urlparse with the variable unbound and the toolkit
-- dies with `UnboundLocalError: cannot access local variable 'url'` — measured,
-- and it surfaces only as a generic "Indexing reported an error." node event.
--
-- It points at this stack's own postgres, which is why that service runs a
-- pgvector image: the start request creates `… embedding vector …` in this
-- database before it answers.
INSERT INTO :"schema".configuration
    (project_id, elitea_title, label, type, section, data, meta, shared, status_ok, source, created_at, updated_at)
VALUES
    (:pid, 'elitea-pgvector', 'elitea-pgvector', 'pgvector', 'vectorstorage',
     jsonb_build_object('connection_string','postgresql+psycopg://elitea:elitea@postgres:5432/elitea'),
     '{}', false, true, 'system', NOW(), NOW())
ON CONFLICT (elitea_title) DO UPDATE
    SET data = EXCLUDED.data, type = EXCLUDED.type, section = EXCLUDED.section,
        status_ok = true, updated_at = NOW();

-- 2. The embedding model the resolver binds.
--
-- `seed-llm` OWNS the model NAME in this row (#468). This statement is a
-- BOOTSTRAP: it creates the row when no row exists, so `seed-index` still
-- provisions a complete index plane on its own.
--
-- `data` is deliberately ABSENT from the conflict action below. That absence is
-- the whole fix for #468: it makes an existing name survive this step, so the
-- order of the two steps cannot change the outcome. Do not add `data` back.
--
-- section='embedding' + type='embedding_model' is a FOURTH configuration section,
-- distinct from the three `seed-llm` writes. The gateway reads this section
-- through `addressableModelSections` (#398), so the row serves the embedding
-- route AND appears in GET /llm/v1/models. The web model picker reads
-- elitea-main's catalogue and not that route, so the row can never be selected
-- as a chat model. `label` must be non-null for the same
-- reason as the chat model row: repos/models.go REJECTS an unlabelled row
-- with an error rather than skipping it, emptying the whole catalogue.
--
-- The gateway reads THIS section for POST /llm/v1/embeddings — it is one of the
-- pairs in addressableModelSections (internal/llmproxy/models.go). Do not add a
-- duplicate `llm`/`llm_model` row to make the embedding hop dispatch: that
-- section is the chat catalogue the web model picker reads, and the embedding
-- model would become a selectable chat model.
-- `data` may contain ONLY `name` and optionally `ai_credentials` — the decoder
-- uses DisallowUnknownFields, so any extra key is an invalid binding (503).
INSERT INTO :"schema".configuration
    (project_id, elitea_title, label, type, section, data, meta, shared, status_ok, source, created_at, updated_at)
SELECT :pid, 'standalone-embedding', 'standalone-embedding', 'embedding_model', 'embedding',
       jsonb_build_object('name', :'embedding'), '{}'::jsonb, true, true, 'user', NOW(), NOW()
 WHERE :'embedding' <> ''
ON CONFLICT (elitea_title) DO UPDATE
    SET type = EXCLUDED.type, section = EXCLUDED.section,
        label = EXCLUDED.label, shared = true, status_ok = true, updated_at = NOW();

-- 3. An indexable toolkit.
--
-- `artifact` is the one indexable type in the pinned toolkit schema snapshot
-- that needs no third-party credential and no egress — it indexes this stack's
-- own RustFS bucket. (`memory` is lighter still and starts fine, but it is not a
-- BaseIndexerToolkit and has no index_data tool at all, so a run against it
-- always terminates in error.)
--
-- `bucket` is not in the Go schema snapshot — that snapshot only declares
-- configuration-typed properties — but the SDK toolkit raises KeyError without
-- it. Settings are frozen and forwarded verbatim, so it simply has to be here.
--
-- `embedding_model` is READ OUT of the row above, not copied from the shell
-- variable (#468). The resolver matches an index run on
-- configuration.data->>'name', so these two names must be one name. A SELECT
-- from that row makes them one name by construction. A shell variable here
-- makes them agree only by luck.
--
-- The SELECT also gates the whole statement: with no embedding row there is no
-- toolkit either, and the assertion after this block names the missing row.
INSERT INTO :"schema".elitea_tools (id, name, type, description, owner_id, author_id, meta, settings)
SELECT :toolkit, 'standalone-artifact-index', 'artifact', 'index plane (#93)', :pid, 1, '{}'::jsonb,
       jsonb_build_object(
           'bucket', 'elitea-artifacts',
           'embedding_model', c.data->>'name',
           'pgvector_configuration',
               jsonb_build_object('elitea_title', 'elitea-pgvector', 'private', false))
  FROM :"schema".configuration AS c
 WHERE c.elitea_title = 'standalone-embedding'
ON CONFLICT (id) DO UPDATE
    SET settings = EXCLUDED.settings, type = EXCLUDED.type, meta = EXCLUDED.meta;
SQL

    # Assert the invariant this step exists to keep (#468). The two names are
    # stored twice, so read both back and compare them. A seed that leaves them
    # different must stop here, not in the worker on the first index run.
    SEEDED_EMBEDDING="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
        psql -U elitea -d elitea -tAc \
        "SELECT data->>'name' FROM p_${TARGET}.configuration
          WHERE elitea_title = 'standalone-embedding'" 2>/dev/null | tr -d '[:space:]')"
    TOOLKIT_EMBEDDING="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
        psql -U elitea -d elitea -tAc \
        "SELECT settings->>'embedding_model' FROM p_${TARGET}.elitea_tools
          WHERE id = ${TOOLKIT_ID}" 2>/dev/null | tr -d '[:space:]')"
    if [ -z "$SEEDED_EMBEDDING" ]; then
      echo "ERROR: project ${TARGET} has no embedding model row, and this step" >&2
      echo "       has no name to write. Run:" >&2
      echo "         LLM_EMBEDDING_MODEL=<model> $0 seed-llm" >&2
      exit 1
    fi
    if [ "$TOOLKIT_EMBEDDING" != "$SEEDED_EMBEDDING" ]; then
      echo "ERROR: project ${TARGET} stores two different embedding model names (#468)." >&2
      echo "       configuration.data->>'name'            = ${SEEDED_EMBEDDING}" >&2
      echo "       elitea_tools.settings.embedding_model  = ${TOOLKIT_EMBEDDING:-<none>}" >&2
      exit 1
    fi
    echo "   embedding model ${SEEDED_EMBEDDING}: catalogue row and toolkit setting agree"
    done
    INDEX_DRIVER_PROJECT="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
        psql -U elitea -d elitea -tAc \
        "SELECT p.id FROM centry.project p
          JOIN public.auth_core__user u ON u.id = p.owner_id
         WHERE p.name = 'project_user_' || u.id::text
           AND u.email = 'e2e-chat@autotest.local'" 2>/dev/null | tr -d '\r')"
    echo "→ Seeded. Start a run as the driver persona (project ${INDEX_DRIVER_PROJECT:-<none>}):"
    echo "     POST /api/v2/elitea_core/test_toolkit_tool/prompt_lib/${INDEX_DRIVER_PROJECT:-1}?await_response=false&execution_contract=index.ingest.v1"
    echo "     {\"toolkit_config\":{\"toolkit_id\":${TOOLKIT_ID}},\"tool_name\":\"index_data\","
    echo "      \"tool_params\":{\"index_name\":\"smoke\"}}"
    echo "   then GET /api/v2/executions/${INDEX_DRIVER_PROJECT:-1}/{task_id}/events"
    echo
    echo "   An EMPTY bucket reaches"
    echo "     event: index.ingest.completed"
    echo "     data: {\"status\":\"ok\",\"message\":\"No new documents to index.\"}"
    echo "   which is green but vacuous. Seed an artifact first (below) and the"
    echo "   same run reaches — measured, 2 files in the bucket —"
    echo "     event: index.ingest.completed"
    echo "     data: {\"status\":\"ok\",\"message\":\"Successfully indexed 2 documents (2 chunks).\"}"
    echo "   with indexed:2 / state:completed on the preceding"
    echo "   agent_index_data_status node event, and the chunk text in"
    echo "   {toolkit_id}.langchain_pg_embedding equal to the files' own bytes."
    echo
    echo "   Both S3-shaped calls the toolkit makes are now served by"
    echo "   elitea-main, root-mounted rather than under /api/v2 — that is"
    echo "   where the SDK asks for them:"
    echo "     GET /artifacts/s3/{bucket}?project_id=…&list-type=2   (listing)"
    echo "     GET /artifacts/s3/{bucket}/{key}?project_id=…         (bytes)"
    echo "   The listing alone was not enough: the toolkit lists the bucket and"
    echo "   then downloads every listed key (elitea-sdk runtime/tools/"
    echo "   artifact.py, _base_loader then _extend_data), and a 404 on the"
    echo "   download is logged and swallowed — so the run used to settle"
    echo "   execution.failed, and a partially-working download would index"
    echo "   files with EMPTY content while still reporting success."
    echo
    echo "   PUT, DELETE and HEAD on {bucket}/{key} are served too. Commit"
    echo "   2ef4f462 added the S3 object write verbs. Measured end to end"
    echo "   through the edge: create bucket 200, PUT 200, GET returns the"
    echo "   same bytes, the listing reports the right size, DELETE 204, and"
    echo "   the GET after the delete 404. An index run needs GET alone; the"
    echo "   SDK uses the others for artifact create/delete and existence checks."
    echo
    echo "   To seed an artifact, use the native route:"
    echo "     POST /api/v2/artifacts/buckets/{projectID}  {\"name\":\"elitea-artifacts\"}"
    echo "     POST /api/v2/artifacts/objects/{projectID}/elitea-artifacts?overwrite=true"
    echo "   The multipart filename IS the key, so a filename of docs/sub/f.txt"
    echo "   seeds a nested key (UploadObject parses Content-Disposition"
    echo "   directly rather than through part.FileName, which would truncate)."
    echo "   Measured separately, and working: POST /llm/v1/embeddings answers"
    echo "   200 with a 1536-wide vector from the offline mock, so the embedding"
    echo "   hop itself is NOT the blocker."
    ;;

  check)
    CERTS="${REPO_ROOT}/deploy/certs"
    GW_PORT="${STANDALONE_GATEWAY_PORT:-8085}"

    # ── Result accounting (issues #429 and #422) ───────────────────────────────────────
    #
    # Three counters, not one. "Nothing failed" and "something was measured" are
    # different statements, and only the second one is a pass.
    #
    # Every skip arm below used to print `~ SKIPPED`, raise nothing and leave
    # the exit status alone. So a stack with no PAT, one user and no index
    # toolkit printed six skips and reported the runtime plane healthy. Each
    # skip names a precondition the operator was told to seed, so an absent one
    # is a failure of this check, not a soft result.
    #
    # `--allow-skips` restores the old soft behaviour for a caller that wants
    # it. The count and the reasons are printed either way.
    #
    # A FOURTH number, and the reason it is here (#422). The three counters
    # above measure the assertions that reached a counter. They cannot see an
    # assertion that reached none: a deleted block, a `case` whose arms all
    # miss, an early exit, or ONE guard that stands in for many assertions and
    # raises a SINGLE skip.
    #
    # That last shape is not hypothetical here. The `if [ -z "$LLM_PROBE" ]`
    # guard below gates ELEVEN assertions and raises one skip, and
    # `apps/elitea-web/scripts/chat-stream-e2e.sh` calls this subcommand with
    # `--allow-skips`. On that path one skip line stands for eleven unmeasured
    # assertions, and the run passes.
    #
    # EXPECTED_ASSERTIONS is a floor: `passes + failures + skips` must equal
    # it. `--allow-skips` does NOT lift this rule, because that flag accepts a
    # NAMED and COUNTED skip, not an assertion that reported nothing.
    #
    # The number is DERIVED from this file, not written down (issue #534). A
    # stated number is true only when the pull request merges. An assertion
    # that a later merge ADDS, with the number left alone, made the floor
    # under-count in silence for ever — which is worse than the skipped
    # assertion the floor exists to catch.
    #
    # Each assertion in this subcommand holds exactly one accepting arm, so the
    # accepting arms are the assertions. One site sits inside a loop and makes
    # one assertion per listener, so the listener list adds its extra rounds.
    # The list is declared here, and counted here, so the two cannot disagree.
    # Read scripts/lib/assertion-floor.sh.
    RUNTIME_LISTENERS=("control 9443" "output 9444" "content 9445")
    ASSERTION_SITE_PATTERN='(^|[^[:alnum:]_])ok[[:space:]]+"'
    ASSERTION_SITE_RANGE='/^  check)$/,/^    ;;$/'
    ASSERTION_SITES="$(derive_assertion_floor "$0" "$ASSERTION_SITE_PATTERN" "$ASSERTION_SITE_RANGE")"
    EXPECTED_ASSERTIONS=$(( ASSERTION_SITES + ${#RUNTIME_LISTENERS[@]} - 1 ))
    ALLOW_SKIPS=0
    for check_arg in "${@:2}"; do
      case "$check_arg" in
        --allow-skips) ALLOW_SKIPS=1 ;;
        *) echo "ERROR: unknown option for check: ${check_arg} (only --allow-skips)" >&2; exit 1 ;;
      esac
    done

    # ── Runtime plane (issue #281) ───────────────────────────────────────────
    # Each assertion below distinguishes "provisioned" from "process happens to
    # be running". A healthy elitea-main proves nothing on its own: with the
    # runtime flag off it is equally healthy and every route here 404s.
    runtime_failures=0
    runtime_passes=0
    runtime_skips=0
    ok()   { echo "  ✓ $1"; runtime_passes=$((runtime_passes + 1)); }
    fail() { echo "  ✗ $1" >&2; runtime_failures=$((runtime_failures + 1)); }
    skip() { echo "  ~ SKIPPED: $1" >&2; runtime_skips=$((runtime_skips + 1)); }

    # The verdict runs from an EXIT trap, not from a block at the foot of this
    # subcommand (#422). `set -e` can end `check` before that block is reached,
    # and the run that stops early is the run whose count matters most.
    # Measured on this stack with the database stopped: the first psql read
    # ended the subcommand, and the operator saw no count at all.
    report_check_result() {
      check_status=$?
      check_total=$((runtime_passes + runtime_failures + runtime_skips))
      echo "→ runtime plane: ${runtime_passes} assertion(s) passed, ${runtime_failures} failed, ${runtime_skips} skipped; ${check_total} of ${EXPECTED_ASSERTIONS} expected assertions reported a result."
      if [ "$check_status" -ne 0 ]; then
        echo "→ FAILED: the check ended early with status ${check_status}." >&2
        echo "   Every assertion after that point did not run. Read the last" >&2
        echo "   line of output above this one for the command that ended it." >&2
        exit "$check_status"
      fi
      # The floor, and --allow-skips cannot lift it. A shortfall means an
      # assertion reported NOTHING: it was deleted, it became unreachable, or
      # one guard stood in for many and raised a single result.
      if [ "$check_total" -ne "$EXPECTED_ASSERTIONS" ]; then
        echo "→ FAILED: ${check_total} assertion(s) reported a result, and ${EXPECTED_ASSERTIONS} were expected." >&2
        echo "   An assertion that reports nothing is not a passed assertion." >&2
        echo "   Seed the preconditions the SKIPPED lines name. If you added or" >&2
        echo "   removed an assertion, move EXPECTED_ASSERTIONS with it." >&2
        echo "   --allow-skips does not lift this rule." >&2
        exit 1
      fi
      if [ "$runtime_failures" -ne 0 ]; then
        echo "→ ${runtime_failures} runtime-plane check(s) failed." >&2
        exit 1
      fi
      if [ "$runtime_skips" -ne 0 ]; then
        if [ "$ALLOW_SKIPS" -eq 1 ]; then
          echo "→ ${runtime_skips} check(s) did not run. --allow-skips was given, so they do not fail this run." >&2
        else
          echo "→ FAILED: ${runtime_skips} check(s) did not run. A skipped check is not a passed check." >&2
          echo "   Seed the preconditions the SKIPPED lines name, or state the choice:" >&2
          echo "     $0 check --allow-skips" >&2
          exit 1
        fi
      fi
      echo "→ runtime plane OK."
    }
    trap report_check_result EXIT

    # ── The gateway mTLS hop ─────────────────────────────────────────────────
    # Talk to the gateway directly over mTLS. /llm routes need signed identity
    # headers that only elitea-main can produce, so this checks transport and
    # liveness, not the completion path.
    echo "→ gateway mTLS on https://localhost:${GW_PORT}:"
    # --http1.1 is required, not cosmetic: curl offers h2 via ALPN, the gateway
    # accepts it, and the response then fails with an INTERNAL_ERROR stream
    # reset. elitea-main's own transport pins NextProtos to http/1.1 for the
    # same reason, so this matches the real client.
    #
    # The answer is CAPTURED and compared (#422). The probe used to end in
    # `&& echo`, which makes the curl a non-final member of an AND list. `set -e`
    # does not act on such a command, and nothing counted the result, so the
    # FIRST assertion of `check` could not turn `check` red.
    gw_health="$(curl -sS --http1.1 --cert "$CERTS/client.crt" --key "$CERTS/client.key" \
         --cacert "$CERTS/ca.crt" --resolve "elitea-llm-gateway:${GW_PORT}:127.0.0.1" \
         -w '\nHTTP %{http_code}' \
         "https://elitea-llm-gateway:${GW_PORT}/healthz" 2>&1 || true)"
    case "$gw_health" in
      *'"status":"ok"'*'HTTP 200'*)
        ok "the gateway answers /healthz over mTLS (HTTP 200)" ;;
      *) fail "the gateway did not answer /healthz over mTLS: $(printf '%s' "$gw_health" | tr '\n' ' ' | cut -c1-200)" ;;
    esac

    # The negative control, and the reason the assertion above is not enough on
    # its own. A 200 proves that the gateway ANSWERED a caller that offered a
    # certificate. It does not prove the listener REQUIRED one: a listener that
    # loses RequireAndVerifyClientCert answers the same 200, because the client
    # offers a certificate the server never asks for. So repeat the request with
    # NO client certificate. It must be refused in the handshake.
    #
    # The failure MODE is read as well. A gateway that is not running refuses
    # this request too, and calling that a pass reports "mTLS is enforced" about
    # a dead process. curl reports a refused or timed-out connection as (7) or
    # (28); neither reached a handshake, so neither measures the requirement.
    gw_anon="$(curl -sS --http1.1 --cacert "$CERTS/ca.crt" \
         --resolve "elitea-llm-gateway:${GW_PORT}:127.0.0.1" \
         -o /dev/null -w 'HTTP %{http_code}' \
         "https://elitea-llm-gateway:${GW_PORT}/healthz" 2>&1 || true)"
    case "$gw_anon" in
      *'HTTP 200'*)
        fail "the gateway served /healthz to a caller with NO client certificate. The listener does not require one, so the elitea-main → gateway hop is not mutually authenticated" ;;
      *'curl: (7)'*|*'curl: (28)'*|*'Failed to connect'*|*'Connection refused'*|*'Could not resolve'*)
        fail "the no-certificate probe never reached a TLS handshake, so the mTLS requirement was not measured: $(printf '%s' "$gw_anon" | tr '\n' ' ' | cut -c1-160)" ;;
      *) ok "the gateway refuses a caller with no client certificate ($(printf '%s' "$gw_anon" | tr '\n' ' ' | cut -c1-120))" ;;
    esac

    echo "→ elitea-main /llm reachability:"
    # NOT a mount check, despite appearances. Auth runs before route matching, so
    # every /api/v2 path answers 401 to an unauthenticated caller whether or not
    # it is registered — measured against a stack with the route absent. A
    # non-401 here means elitea-main is not answering at all.
    #
    # The code is CAPTURED and compared (#422). It used to be printed beside a
    # comment that named 401 and 403 as the passing values, with `|| true`
    # discarding the status and nothing reading the code. A dead elitea-main
    # printed `HTTP 000` and the step passed.
    main_code="$(curl -sS -o /dev/null -w '%{http_code}' \
         "http://localhost:${PORT}/api/v2/llm/v1/models" 2>/dev/null || true)"
    case "$main_code" in
      401|403) ok "elitea-main answers /api/v2/llm/v1/models under auth (HTTP ${main_code})" ;;
      *) fail "elitea-main answered HTTP ${main_code:-000} on /api/v2/llm/v1/models. An unauthenticated caller gets 401 or 403 from a live elitea-main, so this one is not answering" ;;
    esac

    # ── Edge identity-header strip (#326) ────────────────────────────────────
    # elitea-main accepts X-Auth-Type + X-Auth-Id as a finished identity from
    # any source address inside auth.form.yml's trusted_proxy_cidrs, which is
    # the whole compose network. The browser edge sits on that network, so
    # without deploy/traefik/dynamic.e2e.yml's strip-client-identity middleware
    # a caller with no credential at all picks its own user id. Measured on this
    # stack before the fix: `curl -H 'X-Auth-ID: 4'` returned that user's
    # /social/author record with HTTP 200.
    #
    # Written to be DISCRIMINATING. A bare "spoof gets 401" would also pass on a
    # stack where the edge is down, the route is unmounted, or nothing is
    # seeded — every one of those answers 401 too. So each spoof is paired with
    # the same request carrying a real PAT, which must return 200 AND must
    # report the PAT's own user. Two things therefore have to hold: the
    # legitimate path still works through the middleware, and the forged header
    # neither authenticates on its own nor overrides a genuine credential.
    echo "→ edge identity-header strip (#326):"
    spoof_uuid="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
        psql -U elitea -d elitea -tAc \
        "SELECT uuid FROM public.auth_core__token
          WHERE uuid IS NOT NULL ORDER BY user_id LIMIT 1" 2>/dev/null | tr -d '[:space:]')"
    spoof_user="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
        psql -U elitea -d elitea -tAc \
        "SELECT user_id FROM public.auth_core__token
          WHERE uuid = '${spoof_uuid}'" 2>/dev/null | tr -d '[:space:]')"
    if [ -z "$spoof_uuid" ] || [ -z "$spoof_user" ]; then
      skip "no PAT to contrast the spoof against (run: $0 seed-runtime)"
    else
      # Spelled out rather than $RUNTIME_CERTS: that variable is assigned below
      # this block, and under `set -u` reading it here aborts the whole check.
      spoof_jwt="$(python3 - "$spoof_uuid" "${REPO_ROOT}/deploy/certs/runtime/auth-pat-signing-key" <<'PY'
import base64, hashlib, hmac, json, pathlib, sys
key = pathlib.Path(sys.argv[2]).read_bytes()
b64 = lambda b: base64.urlsafe_b64encode(b).rstrip(b"=").decode()
header = b64(json.dumps({"alg": "HS512", "typ": "JWT"}, separators=(",", ":")).encode())
payload = b64(json.dumps({"uuid": sys.argv[1], "expires": None}, separators=(",", ":")).encode())
print(f"{header}.{payload}." + b64(hmac.new(key, f"{header}.{payload}".encode(), hashlib.sha512).digest()))
PY
)"
      # /social/author is the probe because it ECHOES the caller: a status code
      # alone cannot tell "authenticated as the right user" from
      # "authenticated as somebody else", which is the entire defect.
      spoof_route="http://localhost:${PORT}/api/v2/social/author"

      code="$(curl -s -o /dev/null -w '%{http_code}' \
                -H 'X-Auth-Type: user' -H "X-Auth-ID: ${spoof_user}" "$spoof_route" || true)"
      case "$code" in
        401|403) ok "forged X-Auth-ID alone is rejected (HTTP ${code})" ;;
        *) fail "forged X-Auth-ID alone got HTTP ${code} — the edge is forwarding client identity headers (#326)" ;;
      esac

      body="$(curl -s -H "Authorization: Bearer ${spoof_jwt}" "$spoof_route" || true)"
      case "$body" in
        *"\"id\":\"${spoof_user}\""*) ok "a real PAT still authenticates through the middleware" ;;
        *) fail "PAT ${spoof_user} did not authenticate through the edge — the strip broke the legitimate path" ;;
      esac

      # The impersonation that is easiest to miss: the header used to WIN over a
      # genuine bearer, so a real low-privilege user could act as anyone.
      #
      # The victim id is queried rather than hardcoded to `1`. `1` is the lowest
      # user_id, which is also what the PAT query above picks — so a hardcoded
      # `1` forges the caller's OWN identity and the assertion passes against a
      # completely unstripped edge. Verified: it did.
      spoof_other="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
          psql -U elitea -d elitea -tAc \
          "SELECT id FROM public.auth_core__user
            WHERE id <> ${spoof_user} ORDER BY id LIMIT 1" 2>/dev/null | tr -d '[:space:]')"
      if [ -z "$spoof_other" ]; then
        skip "only one user exists, so there is nobody to impersonate"
      else
        body="$(curl -s -H "Authorization: Bearer ${spoof_jwt}" \
                  -H 'X-Auth-Type: user' -H "X-Auth-ID: ${spoof_other}" "$spoof_route" || true)"
        case "$body" in
          *"\"id\":\"${spoof_user}\""*) ok "a forged X-Auth-ID cannot override a genuine bearer" ;;
          *) fail "PAT ${spoof_user} + forged X-Auth-ID: ${spoof_other} did not resolve to ${spoof_user} — the header still overrides the credential (#326)" ;;
        esac
      fi
    fi

    # One-off container on the stack's network with the host material mounted.
    # `compose run` is not usable for these probes: both candidate services
    # declare an entrypoint script, and `run` appends the command as arguments
    # to it rather than replacing it.
    ENGINE="${COMPOSE_BIN%% *}"
    NETWORK="${PROJECT}_default"
    RUNTIME_CERTS="${REPO_ROOT}/deploy/certs/runtime"
    probe() {
      # postgres:18 rather than the alpine-based images: this needs the openssl
      # CLI, and neither alpine:3.20 nor redis:7-alpine ships one.
      $ENGINE run --rm --network "$NETWORK" -v "${RUNTIME_CERTS}:/m:ro" \
        --entrypoint sh docker.io/library/postgres:18 -c "$1" 2>&1 || true
    }

    echo "→ runtime listeners (mTLS, from inside the network):"
    # A TLS 1.3 handshake that gets as far as "certificate required" proves the
    # listener is bound AND enforcing RequireAndVerifyClientCert. A bare TCP
    # connect would also succeed against a half-configured listener.
    for entry in "${RUNTIME_LISTENERS[@]}"; do
      set -- $entry
      name="$1"; port="$2"
      out="$(probe "openssl s_client -connect elitea-main:${port} -CAfile /m/runtime-ca.crt -tls1_3 </dev/null")"
      case "$out" in
        *"certificate required"*|*"peer did not return a certificate"*|*"Verify return code: 0"*)
          ok "${name} :${port} — TLS 1.3 listener up, client cert required" ;;
        *) fail "${name} :${port} — no mTLS listener (runtime disabled or PKI wrong)" ;;
      esac
    done

    echo "→ dispatch streams:"
    STREAM="commands.v1.agent.execute.agent.shared.1.0"
    GROUP="elitea-agent-worker-v1"
    # Read the group over TLS as the bootstrap ACL user. XINFO GROUPS naming the
    # group proves the stream exists AND the group was created — a plain
    # EXISTS check would pass on a stream that XADD created with no consumer.
    groups="$($ENGINE run --rm --network "$NETWORK" \
                -v "${RUNTIME_CERTS}:/m:ro" --entrypoint sh docker.io/library/redis:7-alpine -c \
                "redis-cli --tls --cacert /m/runtime-ca.crt -h runtime-redis -p 6380 \
                   --user bootstrap --pass \"\$(cat /m/redis-bootstrap-password)\" \
                   --no-auth-warning XINFO GROUPS '${STREAM}'" 2>&1 || true)"
    case "$groups" in
      *"$GROUP"*) ok "${STREAM} has consumer group ${GROUP}" ;;
      *) fail "${STREAM} has no ${GROUP} group — runtime-bootstrap did not run" ;;
    esac

    echo "→ runtime composition:"
    # Deliberately a log assertion and NOT an HTTP probe.
    #
    # The obvious check — "POST /api/v2/elitea_core/messages/... must not be
    # 404" — does not discriminate, and measuring it proves it: every /api/v2
    # path answers 401 to an unauthenticated caller on BOTH a runtime-enabled
    # and a runtime-disabled stack, because auth runs before route matching.
    # GET/PUT/DELETE and the events path behave identically. So a passing "not
    # 404" assertion would have said the routes were mounted on a stack where
    # they were never registered.
    #
    # Authenticating would not rescue it either: the runtime routes compose
    # PrincipalValidator + ForwardedIdentityVerifier with no session-cookie
    # fallback (internal/api/production_runtime.go:31-37), so a browser session
    # yields 401 whether or not the route exists.
    #
    # The two signals that DO separate the cases are this log line and the mTLS
    # listeners above — with the runtime off, nothing binds 9443/9444/9445 and
    # the probe gets ECONNREFUSED rather than a handshake.
    MAIN_CONTAINER="$($ENGINE ps --format '{{.Names}}' | grep -m1 "${PROJECT}.*elitea-main" || true)"
    # Captured rather than piped into `grep -q`: this script runs under
    # `set -o pipefail`, and grep -q closes the pipe on its first match, so the
    # producer dies of SIGPIPE and the pipeline reports failure precisely when
    # the assertion SUCCEEDS — an inverted check that reads as a real failure.
    MAIN_LOGS="$($ENGINE logs "$MAIN_CONTAINER" 2>&1 || true)"
    case "$MAIN_LOGS" in
      "") fail "elitea-main container not found in project ${PROJECT}" ;;
      *'"runtime_enabled":true'*)
        ok "elitea-main started with runtime_enabled=true" ;;
      *) fail "elitea-main did not log runtime_enabled=true — the flag or its env block is incomplete" ;;
    esac

    echo "→ workload session row:"
    rows="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
              psql -U elitea -d elitea -tAc \
              "SELECT count(*) FROM elitea_runtime.workload_sessions
                WHERE revoked_at IS NULL AND expires_at > clock_timestamp()" 2>/dev/null || echo 0)"
    if [ "${rows:-0}" -ge 1 ]; then
      ok "${rows} active workload session(s)"
    else
      fail "no active workload session — run: $0 seed-runtime"
    fi

    echo "→ agent worker:"
    # A consumer registered on the group is the discriminating signal. "Container
    # is running" is not: the worker restarts on failure, so a crash-looping one
    # still shows as up between attempts, and its own logs are not proof it got
    # as far as Redis. XINFO CONSUMERS reports only consumers that actually
    # joined the group, so a name here means TLS, the ACL user, the stream and
    # the group all worked.
    #
    # WAITED FOR, not sampled once. The worker has no healthcheck, so
    # `compose up -d --wait` does not wait for it, and it joins the group only
    # after a Python start-up that imports the whole SDK — seconds after the
    # stack reports healthy. A single sample here reads a race: CI has failed
    # this assertion in a run whose chat critical path then streamed its events
    # from this very group, which cannot happen unless the worker joined. The
    # wait costs nothing when the worker is already there and does not weaken
    # the check: a worker that never joins still fails, just later.
    #
    # The poll runs INSIDE one container rather than spawning one per attempt:
    # a `run --rm` costs about as much as the interval itself on a loaded
    # runner, which would make the real timeout something other than the one
    # named here. The last reply is printed either way, so a failure still shows
    # what Redis actually answered.
    consumers="$($ENGINE run --rm --network "$NETWORK" \
      -v "${RUNTIME_CERTS}:/m:ro" --entrypoint sh docker.io/library/redis:7-alpine -c \
      "deadline=\$(( \$(date +%s) + ${WORKER_JOIN_TIMEOUT} ))
       while : ; do
         reply=\$(redis-cli --tls --cacert /m/runtime-ca.crt -h runtime-redis -p 6380 \
                    --user bootstrap --pass \"\$(cat /m/redis-bootstrap-password)\" \
                    --no-auth-warning XINFO CONSUMERS '${STREAM}' '${GROUP}' 2>&1 || true)
         case \"\$reply\" in *standalone-agent-worker*) break ;; esac
         [ \$(date +%s) -lt \$deadline ] || break
         sleep 2
       done
       printf '%s' \"\$reply\"" 2>&1 || true)"
    case "$consumers" in
      *standalone-agent-worker*) worker_joined="yes" ;;
      *) worker_joined="" ;;
    esac
    if [ -n "$worker_joined" ]; then
      ok "worker joined ${GROUP}"
    else
      # "check its logs" is not something CI can act on, so print them here.
      fail "no consumer on ${GROUP} after ${WORKER_JOIN_TIMEOUT}s — the worker never reached Redis"
      echo "    redis answered: ${consumers}" >&2
      WORKER_CONTAINER="$($ENGINE ps -a --format '{{.Names}}' | grep -m1 "${PROJECT}.*elitea-worker" || true)"
      if [ -n "$WORKER_CONTAINER" ]; then
        echo "    ── last 40 lines of ${WORKER_CONTAINER} ──" >&2
        $ENGINE logs --tail 40 "$WORKER_CONTAINER" 2>&1 | sed 's/^/    /' >&2 || true
      fi
    fi

    # The worker's platform_origin must terminate TLS with the runtime CA's
    # edge certificate; the SDK verifies it against that CA alone. A plain
    # connect would pass against any TLS listener, so verify the chain.
    edge="$(probe "openssl s_client -connect elitea-platform-edge:443 -CAfile /m/runtime-ca.crt -servername elitea-platform-edge </dev/null")"
    case "$edge" in
      *"Verify return code: 0"*) ok "platform-edge TLS verifies against the runtime CA" ;;
      *) fail "platform-edge did not present a runtime-CA certificate for elitea-platform-edge" ;;
    esac

    echo "→ execution actor PATs:"
    # Without an active PAT the worker's claim dies at actor_pat_issuance, long
    # before any model call, so this is a precondition rather than a nicety.
    pats="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
              psql -U elitea -d elitea -tAc \
              "SELECT count(*) FROM public.auth_core__token
                WHERE uuid IS NOT NULL
                  AND (expires IS NULL OR expires > (clock_timestamp() AT TIME ZONE 'UTC'))" 2>/dev/null || echo 0)"
    if [ "${pats:-0}" -ge 1 ]; then
      ok "${pats} active PAT(s)"
    else
      fail "no active PAT — run: $0 seed-runtime (executions fail at actor_pat_issuance)"
    fi

    echo "→ model completion through the gateway:"
    # The full hop: elitea-main → gateway (mTLS) → llm-mock. Runs as a real
    # caller, because the gateway resolves per-project credentials from the
    # authenticated identity — an unauthenticated probe would only ever prove
    # that auth works.
    #
    # A PAT bearer is minted here from the same material elitea-main validates
    # with, rather than reusing a browser session: the /llm mount takes bearer
    # tokens and this avoids driving an OIDC login from a shell script.
    #
    # `project_not_resolved` is reported as SKIPPED rather than FAILED because
    # it names a different cause: the caller has no personal project, which is a
    # seeding state (`$0 seed`), not a broken LLM path. It is still counted, and
    # a skip still exits non-zero (#429) — the distinction is in the message the
    # operator reads, not in whether the run passes.
    # The caller must OWN a personal project, because that is what the /llm
    # route resolves the provider credential from. A bare `LIMIT 1` picks
    # dev@elitea.ai, who has none, and the hop then reports
    # `project_not_resolved` — a true statement about the wrong caller, which
    # made this check permanently SKIPPED even on a correctly seeded stack.
    #
    # AND IT MUST BE THE SEEDED CALLER, not merely A caller with a personal
    # project. See deploy/scripts/lib/seeded-driver.sh for why the obvious
    # `ORDER BY t.user_id LIMIT 1` stopped naming the seeded persona, and why
    # the selection lives there rather than in each of the three scripts that
    # need it.
    LLM_PROBE_ROW="$(resolve_seeded_driver standalone_psql_read)"
    LLM_PROBE="$(printf '%s' "$LLM_PROBE_ROW" | awk '{print $1}')"
    LLM_PROJECT="$(printf '%s' "$LLM_PROBE_ROW" | awk '{print $2}')"
    if [ -z "$LLM_PROBE" ]; then
      skip "no PAT to authenticate with (run: $0 seed-runtime)"
    else
      LLM_JWT="$(python3 - "$LLM_PROBE" "${RUNTIME_CERTS}/auth-pat-signing-key" <<'PY'
import base64, hashlib, hmac, json, pathlib, sys
key = pathlib.Path(sys.argv[2]).read_bytes()
b64 = lambda b: base64.urlsafe_b64encode(b).rstrip(b"=").decode()
header = b64(json.dumps({"alg": "HS512", "typ": "JWT"}, separators=(",", ":")).encode())
payload = b64(json.dumps({"uuid": sys.argv[1], "expires": None}, separators=(",", ":")).encode())
print(f"{header}.{payload}." + b64(hmac.new(key, f"{header}.{payload}".encode(), hashlib.sha512).digest()))
PY
)"
      # probe() runs postgres:18, which ships openssl but no python3; the mock
      # image is the only one in the stack with a python runtime, and it runs as
      # an unprivileged uid that cannot read the 0600 host material — so this
      # one probe runs that image as root.
      llm_probe() {
        $ENGINE run --rm --network "$NETWORK" -v "${RUNTIME_CERTS}:/m:ro" --user 0:0 \
          --entrypoint python3 ghcr.io/eliteaai/elitea-mock-llm:standalone -c "$1" 2>&1 || true
      }
      # ── A credential the PRODUCT created, not the seed (#457) ─────────────
      #
      # Every assertion above this one runs on rows `seed-llm` wrote with SQL,
      # and those statements set status_ok = true themselves. The LLM gateway
      # admits only status_ok = true. So a stack whose write route can never
      # produce a usable credential passes every check above, exactly as it did
      # before #457 was found.
      #
      # This step closes that hole. It writes through the product's own HTTP
      # route, and it makes the model turn depend on what that route stored. It
      # fails when a saved credential is invisible to the gateway.
      #
      # The rows go in the CALLER's personal project, because that is the
      # project the /llm hop resolves the credential from (#290).
      echo "→ configuration write route → gateway visibility (#457):"
      WRITE_USER="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
          psql -U elitea -d elitea -tAc \
          "SELECT user_id FROM public.auth_core__token WHERE uuid = '${LLM_PROBE}'" 2>/dev/null | tr -d '[:space:]')"
      WRITE_PROJECT="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
          psql -U elitea -d elitea -tAc \
          "SELECT id FROM centry.project WHERE name = 'project_user_${WRITE_USER}'" 2>/dev/null | tr -d '[:space:]')"
      if [ -z "$WRITE_PROJECT" ]; then
        skip "the probe caller owns no personal project (run: $0 seed)"
      else
        WRITE_CRED="api-created-credential"
        WRITE_MODEL="vllm/API-CREATED-MODEL"
        WRITE_DANGLING="vllm/API-DANGLING-MODEL"
        # Remove the rows of a previous run so the step is repeatable. This
        # deletes; it never writes status_ok. elitea_title is UNIQUE, so a
        # second run would otherwise fail on the insert and say nothing about
        # this defect.
        $COMPOSE_BIN $COMPOSE_F exec -T postgres \
          psql -q -U elitea -d elitea -c \
          "DELETE FROM p_${WRITE_PROJECT}.configuration
            WHERE elitea_title IN ('${WRITE_CRED}', '${WRITE_MODEL}', '${WRITE_DANGLING}')" >/dev/null 2>&1 || true

        write_configuration() {
          curl -sS -X POST \
            -H "Authorization: Bearer ${LLM_JWT}" -H 'Content-Type: application/json' \
            "http://localhost:${PORT}/api/v2/configurations/configurations/${WRITE_PROJECT}" \
            -d "$1" 2>&1 || true
        }

        # 1. The credential. Its shape is the one `seed-llm` writes with SQL:
        # type vllm, a literal api_key and the mock's compose address.
        CRED_OUT="$(write_configuration "{\"elitea_title\":\"${WRITE_CRED}\",\"type\":\"vllm\",\"section\":\"ai_credentials\",\"data\":{\"api_key\":\"mock-key-not-used\",\"api_base\":\"http://llm-mock:8090\"}}")"
        case "$CRED_OUT" in
          *'"status_ok":true'*) ok "a credential saved through the API is usable" ;;
          *'"status_ok":false'*)
            fail "the API stored the credential with status_ok = false (#457).
       The gateway admits only status_ok = true, so this credential is
       invisible to it. Check that ELITEA_CONFIGURATIONS_ENABLED is \"true\",
       which is what composes the admission decision.
       Raw: $(printf '%s' "$CRED_OUT" | tr '\n' ' ' | cut -c1-200)" ;;
          *) fail "the credential write failed: $(printf '%s' "$CRED_OUT" | tr '\n' ' ' | cut -c1-200)" ;;
        esac

        # 2. The model, bound to that credential. elitea_title is the alias the
        # caller sends; data.name is the wire name the gateway dispatches.
        #
        # data.name must NOT be the seeded model's name. The web chat model
        # picker reads elitea-main's catalogue, which keys a model on
        # data.name, so a second row carrying the seeded name collapses with
        # the seeded row in that list and the picker then offers only one of
        # the two, under this row's label. Measured: the #284 chat journey
        # failed with "the seeded model E2E-MOCK-MODEL must be offered".
        # llm-mock echoes whatever model it is asked for, so a distinct name
        # costs nothing here.
        MODEL_OUT="$(write_configuration "{\"elitea_title\":\"${WRITE_MODEL}\",\"label\":\"${WRITE_MODEL}\",\"type\":\"llm_model\",\"section\":\"llm\",\"data\":{\"name\":\"${WRITE_MODEL}\",\"ai_credentials\":{\"elitea_title\":\"${WRITE_CRED}\"}}}")"
        case "$MODEL_OUT" in
          *'"status_ok":true'*) ok "a model saved through the API is usable" ;;
          *) fail "the API stored the model as unusable (#457): $(printf '%s' "$MODEL_OUT" | tr '\n' ' ' | cut -c1-200)" ;;
        esac

        # 3. The negative control. A model whose credential reference does not
        # resolve must stay refused. Without it, a step that simply stored
        # `true` for everything would also pass step 2.
        DANGLING_OUT="$(write_configuration "{\"elitea_title\":\"${WRITE_DANGLING}\",\"label\":\"${WRITE_DANGLING}\",\"type\":\"llm_model\",\"section\":\"llm\",\"data\":{\"name\":\"${WRITE_DANGLING}\",\"ai_credentials\":{\"elitea_title\":\"no-such-credential\"}}}")"
        case "$DANGLING_OUT" in
          *'"status_ok":false'*) ok "a model whose credential does not resolve stays refused" ;;
          *) fail "an unresolvable model was stored as usable: $(printf '%s' "$DANGLING_OUT" | tr '\n' ' ' | cut -c1-200)" ;;
        esac

        # 4. One completion through the API-created rows. This is the assertion
        # that cannot pass on a seeded row: the alias exists nowhere else.
        #
        # Retried, and for a stated reason. The gateway caches each project's
        # model list for DefaultModelsCacheTTL — 60 s in
        # internal/llmproxy/models.go — so a row created after that cache was
        # filled answers `model_not_found` until the entry expires. A single
        # attempt therefore reports a stale cache as a missing credential. The
        # loop waits the cache out and no longer; a row that is genuinely
        # invisible still fails the step.
        WRITE_LLM_OUT=""
        WRITE_WAITED=0
        while : ; do
          WRITE_LLM_OUT="$(llm_probe "
import json, ssl, urllib.error, urllib.request
context = ssl.create_default_context(cafile='/m/runtime-ca.crt')
body = json.dumps({'model': '${WRITE_MODEL}',
                   'messages': [{'role': 'user', 'content': 'api-created credential check'}]}).encode()
request = urllib.request.Request(
    'https://elitea-platform-edge/llm/v1/chat/completions', data=body,
    headers={'Authorization': 'Bearer ${LLM_JWT}', 'Content-Type': 'application/json'})
try:
    print('OK', urllib.request.urlopen(request, context=context, timeout=30).read().decode())
except urllib.error.HTTPError as error:
    print('HTTPERR', error.code, error.read().decode()[:200])
except Exception as error:
    print('ERR', type(error).__name__, error)
")"
          case "$WRITE_LLM_OUT" in
            *model_not_found*)
              [ "$WRITE_WAITED" -ge 70 ] && break
              [ "$WRITE_WAITED" -eq 0 ] && echo "  · waiting for the gateway model cache to expire (up to 70 s)…"
              sleep 10
              WRITE_WAITED=$((WRITE_WAITED + 10))
              ;;
            *) break ;;
          esac
        done
        case "$WRITE_LLM_OUT" in
          *'OK'*'api-created credential check'*)
            ok "a completion ran on a credential and a model the API created" ;;
          *model_not_found*)
            fail "the gateway does not serve the API-created model (#457).
       The row exists, so its status_ok is false or the gateway cannot read it.
       Check it: SELECT elitea_title, status_ok FROM p_${WRITE_PROJECT}.configuration
         WHERE elitea_title = '${WRITE_MODEL}';
       Raw: $(printf '%s' "$WRITE_LLM_OUT" | tr '\n' ' ' | cut -c1-200)" ;;
          *) fail "the API-created completion failed: $(printf '%s' "$WRITE_LLM_OUT" | tr '\n' ' ' | cut -c1-200)" ;;
        esac

        # 5. Put the project back as it was found. `check` runs before the #284
        # chat journey on a shared stack, and these are real product rows: a
        # model row in the `llm` section is a SELECTABLE chat model, so leaving
        # them behind changes what the next reader sees. The delete goes
        # through the product's own route, so it is one more thing this step
        # proves rather than a side channel. It runs whether or not the
        # assertions above passed.
        #
        # ONE assertion, on the END STATE, and the count is why (#422). The
        # per-row status made the number of results depend on how many rows the
        # writes above happened to create, so a step that wrote nothing reported
        # nothing. It also believed the status code: a 204 that deleted no row
        # passed. Counting what is LEFT in the table answers both.
        write_codes=""
        for write_title in "$WRITE_CRED" "$WRITE_MODEL" "$WRITE_DANGLING"; do
          write_id="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
              psql -U elitea -d elitea -tAc \
              "SELECT id FROM p_${WRITE_PROJECT}.configuration
                WHERE elitea_title = '${write_title}'" 2>/dev/null | tr -d '[:space:]')"
          [ -z "$write_id" ] && continue
          write_code="$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
              -H "Authorization: Bearer ${LLM_JWT}" \
              "http://localhost:${PORT}/api/v2/configurations/configuration/${WRITE_PROJECT}/${write_id}" || true)"
          write_codes="${write_codes}${write_title}=${write_code} "
        done
        write_left="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
            psql -U elitea -d elitea -tAc \
            "SELECT count(*) FROM p_${WRITE_PROJECT}.configuration
              WHERE elitea_title IN ('${WRITE_CRED}', '${WRITE_MODEL}', '${WRITE_DANGLING}')" 2>/dev/null | tr -d '[:space:]')"
        if [ "${write_left:-1}" = "0" ]; then
          ok "the API-created rows were removed through the product's own delete route"
        else
          fail "${write_left} API-created row(s) stay in p_${WRITE_PROJECT}.configuration after the delete (${write_codes:-no delete was attempted}); a leftover row in the llm section stays a selectable chat model"
        fi
      fi

      # Probe the model names the SEED wrote. Read them from the same project
      # the probe authenticates as, because the gateway resolves the model set
      # from the caller's own project.
      #
      # A hardcoded pair of names was wrong in both directions (#468). It made
      # `check` FAIL on a stack an operator seeded correctly with
      # LLM_PROVIDER=open_ai. It made `check` PASS on a stack whose two seed
      # steps had written two different embedding names, because the name it
      # probed was the one `seed-index` happened to write last.
      CHAT_MODEL_TITLE="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
          psql -U elitea -d elitea -tAc \
          "SELECT elitea_title FROM p_${LLM_PROJECT}.configuration
            WHERE section = 'llm' AND type = 'llm_model' AND status_ok = true
            ORDER BY id LIMIT 1" 2>/dev/null | tr -d '[:space:]')"
      CHAT_MODEL_NAME="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
          psql -U elitea -d elitea -tAc \
          "SELECT data->>'name' FROM p_${LLM_PROJECT}.configuration
            WHERE section = 'llm' AND type = 'llm_model' AND status_ok = true
            ORDER BY id LIMIT 1" 2>/dev/null | tr -d '[:space:]')"
      EMB_MODEL_NAME="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
          psql -U elitea -d elitea -tAc \
          "SELECT data->>'name' FROM p_${LLM_PROJECT}.configuration
            WHERE elitea_title = 'standalone-embedding'" 2>/dev/null | tr -d '[:space:]')"
      # Counted in BOTH directions (#422). A bare `if empty then fail` reported
      # nothing on the normal path, so the count could not tell a run that read
      # these rows from a run that never reached this code.
      if [ -n "$CHAT_MODEL_NAME" ]; then
        ok "project ${LLM_PROJECT} holds a usable chat model row ('${CHAT_MODEL_NAME}')"
      else
        fail "project ${LLM_PROJECT} holds no chat model row — run: $0 seed-llm"
      fi
      if [ -n "$EMB_MODEL_NAME" ]; then
        ok "project ${LLM_PROJECT} holds an embedding model row ('${EMB_MODEL_NAME}')"
      else
        fail "project ${LLM_PROJECT} holds no embedding model row — run: $0 seed-llm"
      fi

      LLM_OUT="$(llm_probe "
import json, ssl, urllib.error, urllib.request
context = ssl.create_default_context(cafile='/m/runtime-ca.crt')
body = json.dumps({'model': '${CHAT_MODEL_NAME}',
                   'messages': [{'role': 'user', 'content': 'standalone check'}]}).encode()
request = urllib.request.Request(
    'https://elitea-platform-edge/llm/v1/chat/completions', data=body,
    headers={'Authorization': 'Bearer ${LLM_JWT}', 'Content-Type': 'application/json'})
try:
    payload = json.loads(urllib.request.urlopen(request, context=context, timeout=30).read())
    choices = payload.get('choices') or []
    content = ((choices[0].get('message') or {}).get('content') or '') if choices else ''
    print('LEN', len(content), repr(content[:80]))
except urllib.error.HTTPError as error:
    print('HTTPERR', error.code, error.read().decode()[:200])
except Exception as error:
    print('ERR', type(error).__name__, error)
")"
      # Assert on the LENGTH of the answer, not on the mock's echo. A real
      # provider answers a real completion, so an echo assertion made `check`
      # fail on every stack that was not the mock.
      case "$LLM_OUT" in
        *'LEN 0'*)                 fail "completion hop returned an empty answer: $(printf '%s' "$LLM_OUT" | tr '\n' ' ' | cut -c1-160)" ;;
        *'LEN '[1-9]*)             ok "completion returned an answer from '${CHAT_MODEL_NAME}'" ;;
        *project_not_resolved*)    skip "caller has no personal project (run: $0 seed)" ;;
        *'HTTPERR 502'*)           fail "gateway could not reach llm-mock — is GATEWAY_EGRESS_ALLOWLIST set? (see the compose comment)" ;;
        *)                         fail "completion hop failed: $(printf '%s' "$LLM_OUT" | tr '\n' ' ' | cut -c1-160)" ;;
      esac

      # The EMBEDDING hop, over the same edge and gateway (#93 Surface A). It is
      # checked separately from the completion above because it is a different
      # upstream route (`/v1/embeddings`), a different model row and a different
      # response shape — a stack whose completions work can still have no
      # embeddings at all, which is what an index run actually needs.
      #
      # The assertion is on the WIDTH of the returned vector, not on a 200: an
      # OpenAI-shaped error body and an empty `data` list both answer 200, and a
      # vector of the wrong width fails only much later, at insert time, inside
      # the worker. The `openai` client asks for base64 unless told otherwise,
      # so `encoding_format` is pinned to `float` here to keep the probe's own
      # decoding trivial and the failure attributable to the hop.
      EMB_OUT="$(llm_probe "
import json, ssl, urllib.error, urllib.request
context = ssl.create_default_context(cafile='/m/runtime-ca.crt')
body = json.dumps({'model': '${EMB_MODEL_NAME}',
                   'encoding_format': 'float',
                   'input': 'standalone embedding check'}).encode()
request = urllib.request.Request(
    'https://elitea-platform-edge/llm/v1/embeddings', data=body,
    headers={'Authorization': 'Bearer ${LLM_JWT}', 'Content-Type': 'application/json'})
try:
    payload = json.loads(urllib.request.urlopen(request, context=context, timeout=30).read())
    vectors = payload.get('data') or []
    print('DIM', len(vectors[0].get('embedding') or []) if vectors else 0)
except urllib.error.HTTPError as error:
    print('HTTPERR', error.code, error.read().decode()[:200])
except Exception as error:
    print('ERR', type(error).__name__, error)
")"
      case "$EMB_OUT" in
        *'DIM 0'*)              fail "embedding hop returned no vector: $(printf '%s' "$EMB_OUT" | tr '\n' ' ' | cut -c1-160)" ;;
        *'DIM '[1-9]*)          ok "embedding hop returned a vector ($(printf '%s' "$EMB_OUT" | tr -dc '0-9 ' | awk '{print $1}') dimensions)" ;;
        *project_not_resolved*) skip "caller has no personal project (run: $0 seed)" ;;
        *model_not_found*)
          # Name the cause (#380). A bare 404 sent people to the gateway's
          # routing when the catalogue simply held no embedding row, or held
          # one the gateway could not read.
          fail "embedding hop: the gateway serves no such embedding model.
       Check the row: SELECT elitea_title, type, section, data->>'name'
         FROM p_<id>.configuration WHERE section = 'embedding';
       It must be type='embedding_model' AND section='embedding'.
       The probe asked for '${EMB_MODEL_NAME}', which is that row's own
       data->>'name' in project ${LLM_PROJECT}.
       No row at all means seed-llm did not seed one — run: $0 seed-llm
       Raw: $(printf '%s' "$EMB_OUT" | tr '\n' ' ' | cut -c1-160)" ;;
        *)                      fail "embedding hop failed: $(printf '%s' "$EMB_OUT" | tr '\n' ' ' | cut -c1-160)" ;;
      esac

      # GET /llm/v1/models must list BOTH models (#398). The gateway reads
      # every pair in `addressableModelSections`, so the list carries the chat
      # model and the embedding model. This is intended, and DECISIONS.md
      # records why: OpenAI's own /v1/models lists embedding models, the legacy
      # LiteLLM list did too, and the BFF.3 parity gate asserts they stay
      # present. The web model picker does NOT read this route — it reads
      # elitea-main's catalogue — so an embedding model here never becomes
      # selectable as a chat model.
      #
      # Assert the list here, because no other check in this repository reads
      # it on a live stack.
      MODELS_OUT="$(llm_probe "
import json, ssl, urllib.error, urllib.request
context = ssl.create_default_context(cafile='/m/runtime-ca.crt')
request = urllib.request.Request(
    'https://elitea-platform-edge/llm/v1/models',
    headers={'Authorization': 'Bearer ${LLM_JWT}'})
try:
    payload = json.loads(urllib.request.urlopen(request, context=context, timeout=30).read())
    print('IDS', json.dumps([m.get('id') for m in payload.get('data') or []]))
except urllib.error.HTTPError as error:
    print('HTTPERR', error.code, error.read().decode()[:200])
except Exception as error:
    print('ERR', type(error).__name__, error)
")"
      # Both names are required, so an empty list cannot pass. A list that
      # holds the chat model alone means the embedding section is invisible to
      # the resolver, which is the defect #398 corrected.
      #
      # The list reports elitea_title as `id` (llmproxy/models.go, modelObject),
      # so compare against the TITLES the seed wrote, not against a hardcoded
      # mock name (#468).
      case "$MODELS_OUT" in
        *"${CHAT_MODEL_TITLE}"*)
          case "$MODELS_OUT" in
            *'standalone-embedding'*)
              ok "GET /llm/v1/models lists the chat model and the embedding model" ;;
            *)
              fail "GET /llm/v1/models omits the embedding model (#398).
       The resolver reads every pair in addressableModelSections, so the
       embedding row must appear. Check the row:
         SELECT elitea_title, type, section FROM p_<id>.configuration
           WHERE section = 'embedding';
       Raw: $(printf '%s' "$MODELS_OUT" | tr '\n' ' ' | cut -c1-200)" ;;
          esac ;;
        *) fail "GET /llm/v1/models did not list the seeded chat model '${CHAT_MODEL_TITLE}': $(printf '%s' "$MODELS_OUT" | tr '\n' ' ' | cut -c1-200)" ;;
      esac

      # The two stored names must agree (#468). The catalogue row is what the
      # gateway serves; the toolkit setting is what an index run asks for. Two
      # different names admit the run, make it durable, and then kill it in the
      # worker with a 404, so a static comparison here is worth more than the
      # live hop above.
      #
      # SKIPPED rather than failed when no toolkit row exists: `seed-index` is a
      # separate step and the `chat-stream` job never runs it. It is counted,
      # and a counted skip still exits non-zero unless the caller passes
      # --allow-skips (#429). A caller that deliberately skips seed-index says
      # so on the command line.
      TOOLKIT_EMBEDDING="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
          psql -U elitea -d elitea -tAc \
          "SELECT settings->>'embedding_model' FROM p_${LLM_PROJECT}.elitea_tools
            WHERE name = 'standalone-artifact-index'" 2>/dev/null | tr -d '[:space:]')"
      if [ -z "$TOOLKIT_EMBEDDING" ]; then
        skip "project ${LLM_PROJECT} holds no index toolkit (run: $0 seed-index)"
      elif [ "$TOOLKIT_EMBEDDING" != "$EMB_MODEL_NAME" ]; then
        fail "the index toolkit and the catalogue name two different embedding models (#468).
       p_${LLM_PROJECT}.configuration data->>'name'           = ${EMB_MODEL_NAME}
       p_${LLM_PROJECT}.elitea_tools settings.embedding_model = ${TOOLKIT_EMBEDDING}
       The index resolver matches on data->>'name', so this run starts, becomes
       durable, and then fails in the worker with a 404.
       Re-run: $0 seed-llm && $0 seed-index"
      else
        ok "the index toolkit and the catalogue name one embedding model ('${EMB_MODEL_NAME}')"
      fi
    fi

    # ── The embedding path (#470) ────────────────────────────────────────────
    # Kept in its own script because it makes 13 assertions and reads the mock's
    # request journal, which no other check does. The probe above asserts a
    # vector WIDTH against one hardcoded model name; a width identifies no model
    # and a 200 identifies no project. Read that script's own assertion lines.
    #
    # It applies only when the MOCK serves the embeddings. Its facts 2 and 3 —
    # "the provider was really called", "with which model name and which
    # project's credential" — are read out of llm-mock's request journal,
    # which is the only record of what went on the wire. A stack whose
    # embedding credential points anywhere else (the `real-llm` profile's
    # llm-real, an operator's own server) keeps no such journal, so the
    # script reports four "no upstream call" failures that are statements
    # about the harness, not the gateway. Measured on the first
    # `chat-stream-real` run: every hop assertion above passed and this one
    # failed on "Journal: COUNT 0".
    #
    # The gate is the seeded credential's api_base, read out of the database
    # through the embedding row's own link — not LLM_PROVIDER, which a
    # mislabelled invocation could set to anything. Same shape as the SDK
    # client gate below (the running image, not $STANDALONE_WORKER). A stack
    # that holds no embedding row at all still runs the script, which aborts
    # with its own message naming the seed step.
    # The api_base of the credential a catalogue row links to, for the row
    # the predicate selects. Through standalone_psql_read, whose `|| true`
    # keeps a failing read from ending `check` under `set -e` with its error
    # hidden; an empty answer takes the first arm below and RUNS the script.
    # Shared by this gate and the SDK gate further down, so the join and the
    # mock's address are spelled once.
    LLM_MOCK_API_BASE="http://llm-mock:8090"
    linked_credential_api_base() {
      standalone_psql_read \
        "SELECT c.data->>'api_base'
           FROM p_${LLM_PROJECT:-1}.configuration m
           JOIN p_${LLM_PROJECT:-1}.configuration c
             ON c.section = 'ai_credentials'
            AND c.elitea_title = m.data->'ai_credentials'->>'elitea_title'
          WHERE $1" | tr -d '[:space:]'
    }
    EMB_CRED_API_BASE="$(linked_credential_api_base "m.section = 'embedding' AND m.elitea_title = 'standalone-embedding'")"
    case "${EMB_CRED_API_BASE%/}" in
      ""|"$LLM_MOCK_API_BASE")
        if STANDALONE_PROJECT="$PROJECT" "${REPO_ROOT}/deploy/scripts/embedding-path-check.sh"; then
          ok "the embedding path check passed all of its own assertions"
        else
          fail "the embedding path check failed — read its assertion lines above"
        fi
        ;;
      *)
        skip "the embedding path check does not apply: it reads llm-mock's request journal, and this stack's embedding credential points at ${EMB_CRED_API_BASE}, which keeps none. The hop itself was asserted above (vector width, model list)."
        ;;
    esac

    # ── The REAL elitea-sdk client ───────────────────────────────────────────
    # Every /llm assertion above this line — this script's own probes and the
    # embedding path check — speaks HTTP the way we BELIEVE elitea-sdk speaks
    # it. None of them imports the SDK, so none of them can fail when the SDK's
    # defaults, its client libraries or its error reader disagree with what the
    # platform serves. That is exactly how the budget refusal defect survived:
    # a correct-looking body, a correct status, a passing unit test, and an SDK
    # that still returned None.
    #
    # This script drives the real EliteAClient inside the running worker's own
    # image, so the SDK under test is the SDK the product runs. It makes 15
    # assertions of its own; read its lines above, not this one alone.
    #
    # Its 15 assertions count as ONE assertion here, so a WRAPPER that exits 0
    # without ever starting the python satisfies this check and the total
    # below. Replace sdk-client-check.sh with "exit 0" and every count still
    # adds up (measured). So do not read the exit status alone: require the
    # completion line the python prints, and require the count in it. The
    # python refuses a partial run on its own; this catches a run that never
    # happened.
    # The check runs the SDK inside the RUNNING worker's image. On the native
    # Rust runtime that image is a Rust binary with no python and no SDK — the
    # SDK is not part of that leg's execution path at all, so running it there
    # can only measure the image's lack of python3 (measured: "exec: python3:
    # executable file not found"). That gate is the running container's image,
    # not $STANDALONE_WORKER, so a mislabelled invocation cannot dodge it; the
    # second gate below is the seeded upstream, read from the database.
    # SDK conformance still rides every python-worker leg on the mock.
    SDK_WORKER_IMAGE="$($ENGINE ps --filter "name=${PROJECT}" --format '{{.Names}} {{.Image}}' 2>/dev/null | sed -n 's/^[^ ]*elitea-worker[^ ]* //p' | head -1)"
    # A second gate, on the upstream: sdk-client-check.py asserts the nonce
    # echo, llm-mock's request journal and a 1536-wide embedding — the mock's
    # protocol. Against any other upstream (the `real-llm` profile) those
    # assertions fail by construction while the SDK itself is fine, so the
    # check is a named skip there. Read for the SAME chat row this check's
    # own completion probe used (CHAT_MODEL_TITLE), not an arbitrary one.
    SDK_CRED_API_BASE="$(linked_credential_api_base "m.section = 'llm' AND m.elitea_title = '${CHAT_MODEL_TITLE:-}'")"
    if [ -n "$SDK_WORKER_IMAGE" ] && [[ "$SDK_WORKER_IMAGE" == *worker-rust* ]]; then
      skip "the elitea-sdk client check does not apply: the worker is the native runtime (${SDK_WORKER_IMAGE}), which carries no SDK"
    elif [ -n "$SDK_CRED_API_BASE" ] && [ "${SDK_CRED_API_BASE%/}" != "$LLM_MOCK_API_BASE" ]; then
      skip "the elitea-sdk client check does not apply: its nonce, journal and wire-model assertions are written against llm-mock's protocol, and this stack's chat credential points at ${SDK_CRED_API_BASE}"
    else
    sdk_check_log="$(mktemp)"
    if STANDALONE_PROJECT="$PROJECT" "${REPO_ROOT}/deploy/scripts/sdk-client-check.sh" 2>&1 | tee "$sdk_check_log"; then
      sdk_check_ran="$(sed -n 's/^→ elitea-sdk client: \([0-9][0-9]*\) assertion(s) ran.*/\1/p' "$sdk_check_log" | tail -1)"
      if [ -z "$sdk_check_ran" ]; then
        fail "the elitea-sdk client check exited 0 but printed no assertion count — the wrapper did not run the python"
      elif [ "$sdk_check_ran" -lt 15 ]; then
        fail "the elitea-sdk client check reported only ${sdk_check_ran} assertion(s), and 15 is its floor"
      else
        ok "the elitea-sdk client check passed all ${sdk_check_ran} of its own assertions"
      fi
    else
      fail "the elitea-sdk client check failed — read its assertion lines above"
    fi
    rm -f "$sdk_check_log"
    fi

    echo "→ chat critical path (#284 smoke):"
    # Precondition first, because the failure it produces is a bare HTTP 500
    # that names nothing. Three tenant chat tables the agent-execution queries
    # depend on are created by no migration in this repo — they are owned by
    # pylon's tenant-schema lifecycle, which a pylon-free stack does not have.
    # See #287; agent_chat_baseline.sql states the assumption in its header.
    # The chat driver: a PAT whose user owns a personal project AND holds the
    # start route's permission IN it. That project — not project 1 — is where
    # the turn runs, because the /llm hop resolves the caller's PERSONAL project
    # to find the credential (#290).
    #
    # The permission join is what keeps this probe pointed at the SEEDED
    # persona now that elitea-main provisions a personal project for any caller
    # that lacks one (internal/application/personalproject). Such a project
    # carries project ROLES and no auth_core__project_role_permission rows —
    # projectprovisioning writes roles only, on purpose — so it cannot match
    # here. Read that alongside the LLM probe above, which had no permission
    # join and did start picking those projects.
    CHAT_ROW="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
        psql -U elitea -d elitea -tAc \
        "SELECT t.uuid || ' ' || t.user_id || ' ' || p.id
           FROM public.auth_core__token t
           JOIN centry.project p ON p.name = 'project_user_' || t.user_id::text
           JOIN public.auth_core__project_user_role pur
             ON pur.project_id = p.id AND pur.user_id = t.user_id
           JOIN public.auth_core__project_role_permission perm
             ON perm.role_id = pur.role_id AND perm.project_id = p.id
            AND perm.permission = 'models.chat.messages.create'
          WHERE t.uuid IS NOT NULL
          ORDER BY t.user_id
          LIMIT 1" 2>/dev/null | tr -d '\r')"
    CHAT_PROJECT="$(printf '%s' "$CHAT_ROW" | awk '{print $3}')"
    CHAT_PROJECT="${CHAT_PROJECT:-1}"
    MISSING_CHAT_TABLES="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
        psql -U elitea -d elitea -tAc \
        "SELECT string_agg(missing.name, ',')
           FROM (VALUES ('chat_messages_text'),('chat_messages_context'),('chat_message_trace_step')) AS missing(name)
          WHERE to_regclass('p_${CHAT_PROJECT}.' || missing.name) IS NULL" 2>/dev/null | tr -d '[:space:]')"
    if [ -n "$MISSING_CHAT_TABLES" ]; then
      # Counted as a SKIP, not reported and forgotten (#429). #287 is a filed
      # product gap, and the gap is real — but the statement this check makes is
      # "the chat critical path was not measured", and that is a skip whatever
      # its cause. It was previously printed and NOT counted, which let a run
      # that never drove a single chat turn report the runtime plane healthy.
      #
      # An operator who knows about #287 and wants the rest of the check as a
      # gate passes --allow-skips. That is a decision the caller states, not one
      # this script makes on the caller's behalf.
      echo "  ! BLOCKED by #287 — p_${CHAT_PROJECT} is missing tenant chat tables; agent turns 500 here"
      echo "    (missing: $MISSING_CHAT_TABLES)"
      skip "the chat critical path did not run — see the #287 line above"
    else
      CHAT_PAT="$(printf '%s' "$CHAT_ROW" | awk '{print $1}')"
      CHAT_USER="$(printf '%s' "$CHAT_ROW" | awk '{print $2}')"
      if [ -z "$CHAT_PAT" ] || [ -z "$CHAT_USER" ]; then
        skip "no PAT to drive the turn (run: $0 seed-runtime)"
      else
        # Run INSIDE the compose network, not from the host: the events stream
        # is only reachable through platform-edge (#289), and the edge's
        # certificate has exactly one SAN — elitea-platform-edge — so a host
        # client would fail hostname verification even if the name resolved.
        # The mock image is used purely as a python runtime with the runtime CA
        # available; --user 0:0 so it can read the 0600 signing key.
        # Drive the turn with the model THIS STACK holds, not chat-smoke.py's
        # compiled-in default.
        #
        # That default names the offline mock. On a stack seeded against any real
        # provider no such model exists upstream, so every turn reached the model
        # call and came back `404 the model does not exist` — which this check
        # reported as "no streamed token chunk". That reads as "chat is broken"
        # when chat is fine and the harness asked for a model nobody serves. Same
        # shape as the hardcoded path that stopped gating: the value was right
        # when it was written and nothing re-checked it.
        #
        # `ORDER BY id LIMIT 1` deliberately matches the other model resolvers in
        # this script, so the check drives the row they would pick. NOTE that this
        # makes a re-seed's model INVISIBLE here: `seed-llm` upserts on the model
        # NAME, so seeding a DIFFERENT model ADDS a row and the first one seeded
        # still wins by id. `seed-llm` now says so when it happens, rather than
        # reporting a re-seed that changed nothing a consumer will read.
        CHAT_SMOKE_MODEL="$($COMPOSE_BIN $COMPOSE_F exec -T postgres \
            psql -U elitea -d elitea -tAc \
            "SELECT data->>'name' FROM p_${CHAT_PROJECT}.configuration
              WHERE section = 'llm' AND type = 'llm_model' AND status_ok = true
              ORDER BY id LIMIT 1" 2>/dev/null | tr -d '[:space:]')"
        if [ -n "$CHAT_SMOKE_MODEL" ]; then
          echo "  · driving the turn with this stack's chat model '${CHAT_SMOKE_MODEL}'"
        else
          echo "  · no chat model row in p_${CHAT_PROJECT}; using chat-smoke.py's default"
        fi
        set +e
        $ENGINE run --rm --network "$NETWORK" \
          -v "${RUNTIME_CERTS}:/m:ro" \
          -v "${REPO_ROOT}/deploy/scripts/chat-smoke.py:/opt/chat-smoke.py:ro" \
          --user 0:0 --entrypoint python3 \
          ghcr.io/eliteaai/elitea-mock-llm:standalone /opt/chat-smoke.py \
          --base-url "https://elitea-platform-edge" \
          --ca /m/runtime-ca.crt \
          --pat-uuid "$CHAT_PAT" \
          --signing-key /m/auth-pat-signing-key \
          --user-id "$CHAT_USER" \
          --project "$CHAT_PROJECT" \
          ${CHAT_SMOKE_MODEL:+--model "$CHAT_SMOKE_MODEL"}
        smoke_status=$?
        set -e
        # chat-smoke.py's exit codes, counted rather than swallowed (#429).
        # 2 and 3 both mean the turn did not run, and a turn that did not run
        # is not a turn that worked.
        case "$smoke_status" in
          0) ok "the chat critical path ran end to end" ;;
          2) skip "chat smoke reported a missing precondition (exit 2) — read its own lines above" ;;
          3) skip "chat smoke is blocked by a filed platform gap (exit 3) — read its own lines above" ;;
          *) fail "chat smoke failed (exit ${smoke_status})" ;;
        esac
      fi
    fi

    # The count line, the floor and the verdict all live in
    # report_check_result, which the EXIT trap above runs. Read that line, not
    # the exit status alone: it separates "nothing failed" from "everything was
    # measured". The embedding path check reports its own assertions on its own
    # line above; these are this script's.
    exit 0
    ;;

  *)
    # Print the header block: every comment line down to the first line of
    # code. A hardcoded line range stops printing the last option the moment
    # the header grows, and nothing reports that.
    awk 'NR > 1 { if (substr($0, 1, 1) != "#") exit; print }' "$0"
    exit 1
    ;;
esac
