#!/usr/bin/env bash
# Behaviour assertions for the EMBEDDING PATH of a running standalone stack
# (issue #470). Run it after `standalone-stack.sh seed-llm`.
#
#   STANDALONE_PROJECT=<your-own-name> deploy/scripts/embedding-path-check.sh
#
# ── What this adds that nothing else measured ────────────────────────────────
#
# `standalone-stack.sh check` asserted the WIDTH of the returned vector against
# one hardcoded model name. A width identifies no model, a 200 identifies no
# project, and a hardcoded name speaks only to the stack it was written for.
#
# This script reads the seeded names out of the database and then asserts, for
# each probe, three separate facts:
#
#   1. what the platform answered;
#   2. that the upstream provider was really called — read from the mock's own
#      request journal, which is the only record of what went on the wire; and
#   3. WHICH model name and WHICH project's credential that call carried.
#
# Fact 3 is why `seed-llm` gives the mock a per-project key. The credential the
# gateway sends names the project it resolved.
#
# ── The four probes ──────────────────────────────────────────────────────────
#
#   A  the seeded embedding model            -> 200, one upstream call, the
#                                               caller's own project credential
#   B  a model name no row holds             -> 404 that NAMES the model, and
#                                               NO upstream call at all
#   C  a public-project model, shared=true   -> listed, and 200 (issue #458)
#   D  a public-project model, shared=false  -> not listed, and 404
#
# Probe B's second half is the one that is easy to leave out. A refusal is only
# safe if nothing was dispatched. A gateway that substituted some other model
# would also answer 200, and only the journal can tell the two apart.
#
# ── Reading the result ───────────────────────────────────────────────────────
#
# The last line reports how many assertions RAN and how many failed. A run that
# skips its assertions is a FAILURE here, not a pass: the expected count is
# DERIVED from this file, and any other number exits non-zero. Read that line,
# not the exit code alone.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
PROJECT="${STANDALONE_PROJECT:-elitea-standalone}"
COMPOSE_F="-p ${PROJECT} -f ${REPO_ROOT}/deploy/docker-compose.standalone-full.yml"

# shellcheck source=../../apps/elitea-web/scripts/lib/compose-detect.sh
. "${REPO_ROOT}/apps/elitea-web/scripts/lib/compose-detect.sh"
# shellcheck source=lib/seeded-driver.sh
. "${REPO_ROOT}/deploy/scripts/lib/seeded-driver.sh"
# shellcheck source=../../scripts/lib/assertion-floor.sh
. "${REPO_ROOT}/scripts/lib/assertion-floor.sh"
detect_compose_bin

ENGINE="${COMPOSE_BIN%% *}"
NETWORK="${PROJECT}_default"
RUNTIME_CERTS="${REPO_ROOT}/deploy/certs/runtime"

# Every assertion this script is meant to make. A run that makes fewer has
# skipped one, and a skipped assertion proves nothing.
#
# DERIVED from this file, not written down (issue #534). A number an author
# states is true only when the pull request merges: an assertion added by a
# later merge, with the number left alone, makes the floor under-count in
# silence for ever. Each assertion below holds exactly one accepting arm, so
# the accepting arms are the assertions. Read scripts/lib/assertion-floor.sh.
ASSERTION_SITE_PATTERN='(^|[^[:alnum:]_])pass[[:space:]]+"'
EXPECTED_ASSERTIONS="$(derive_assertion_floor "$0" "$ASSERTION_SITE_PATTERN")"

ran=0
failed=0
pass() { ran=$((ran + 1)); echo "  ✓ $1"; }
fail() { ran=$((ran + 1)); failed=$((failed + 1)); echo "  ✗ $1" >&2; }
abort() { echo "ERROR: $1" >&2; exit 1; }

psql_read() {
  $COMPOSE_BIN $COMPOSE_F exec -T postgres \
    psql -U elitea -d elitea -tAc "$1" 2>/dev/null | tr -d '\r' || true
}

# The driver: a PAT whose user OWNS a personal project AND whose project holds
# the seeded chat model. The /llm hop resolves the credential from that project,
# so a caller without one reports `project_not_resolved` — a true statement
# about the wrong caller. Selected by deploy/scripts/lib/seeded-driver.sh, which
# every LLM check shares; read it for why the id ordering alone stopped working.
DRIVER="$(resolve_seeded_driver psql_read)"
DRIVER_PAT="$(printf '%s' "$DRIVER" | awk '{print $1}')"
DRIVER_PROJECT="$(printf '%s' "$DRIVER" | awk '{print $2}')"
[ -n "$DRIVER_PAT" ] || abort "no PAT owning a personal project. Run: deploy/scripts/standalone-stack.sh seed-runtime"
[ "${DRIVER_PROJECT:-1}" != "1" ] || abort "the driver's personal project is the public project, so the shared scope cannot be measured"

# The seeded names, READ rather than assumed. A hardcoded probe model reports a
# failure on a stack an operator seeded correctly with another provider, and
# reports success on a stack whose seed steps disagree (issue #468).
SEEDED_TITLE='standalone-embedding'
SEEDED_MODEL="$(psql_read "SELECT data->>'name' FROM p_${DRIVER_PROJECT}.configuration
   WHERE section = 'embedding' AND elitea_title = '${SEEDED_TITLE}'")"
[ -n "$SEEDED_MODEL" ] || abort "project ${DRIVER_PROJECT} holds no '${SEEDED_TITLE}' row. Run: deploy/scripts/standalone-stack.sh seed-llm"
# The provider prefix selects the credential and is stripped before dispatch,
# so the upstream sees the bare name.
WIRE_MODEL="${SEEDED_MODEL#*/}"
EXPECTED_CREDENTIAL="mock-key-project-${DRIVER_PROJECT}"
UNKNOWN_MODEL="E2E-NO-SUCH-EMBEDDING-MODEL"

echo "→ embedding path (issue #470), driver project ${DRIVER_PROJECT}:"
echo "   catalogue title '${SEEDED_TITLE}' -> model '${SEEDED_MODEL}' -> wire name '${WIRE_MODEL}'"

DRIVER_JWT="$(python3 - "$DRIVER_PAT" "${RUNTIME_CERTS}/auth-pat-signing-key" <<'PY'
import base64, hashlib, hmac, json, pathlib, sys
key = pathlib.Path(sys.argv[2]).read_bytes()
b64 = lambda b: base64.urlsafe_b64encode(b).rstrip(b"=").decode()
header = b64(json.dumps({"alg": "HS512", "typ": "JWT"}, separators=(",", ":")).encode())
payload = b64(json.dumps({"uuid": sys.argv[1], "expires": None}, separators=(",", ":")).encode())
print(f"{header}.{payload}." + b64(hmac.new(key, f"{header}.{payload}".encode(), hashlib.sha512).digest()))
PY
)"

# One container on the stack's network with the runtime CA mounted. The mock
# image is the only one in the stack with a python runtime; --user 0:0 so it can
# read the 0600 signing material. The same pattern `check` uses.
MOCK_IMAGE="ghcr.io/eliteaai/elitea-mock-llm:standalone"
probe_py() {
  $ENGINE run --rm --network "$NETWORK" -v "${RUNTIME_CERTS}:/m:ro" --user 0:0 \
    --entrypoint python3 "$MOCK_IMAGE" -c "$1" 2>&1 || true
}

# ── The journal ──────────────────────────────────────────────────────────────
# Read from INSIDE the network, so this script does not depend on the mock's
# published host port.
journal_reset() {
  probe_py "
import urllib.request
request = urllib.request.Request('http://llm-mock:8090/__journal', method='DELETE')
urllib.request.urlopen(request, timeout=10).read()
print('RESET')
" >/dev/null
}
journal_read() {
  probe_py "
import json, urllib.request
print('JOURNAL', urllib.request.urlopen('http://llm-mock:8090/__journal', timeout=10).read().decode())
"
}

llm_post() {
  # $1 route, $2 JSON body
  probe_py "
import json, ssl, urllib.error, urllib.request
context = ssl.create_default_context(cafile='/m/runtime-ca.crt')
context.load_cert_chain('/m/agent-worker-client.crt', '/m/agent-worker-client.key')
request = urllib.request.Request(
    'https://elitea-main:9446/llm/v1/$1', data=json.dumps($2).encode(),
    headers={'Authorization': 'Bearer ${DRIVER_JWT}', 'Content-Type': 'application/json'})
try:
    print('OK', urllib.request.urlopen(request, context=context, timeout=30).read().decode()[:400])
except urllib.error.HTTPError as error:
    print('HTTPERR', error.code, error.read().decode()[:400])
except Exception as error:
    print('ERR', type(error).__name__, error)
"
}

llm_models() {
  probe_py "
import json, ssl, urllib.error, urllib.request
context = ssl.create_default_context(cafile='/m/runtime-ca.crt')
context.load_cert_chain('/m/agent-worker-client.crt', '/m/agent-worker-client.key')
request = urllib.request.Request(
    'https://elitea-main:9446/llm/v1/models',
    headers={'Authorization': 'Bearer ${DRIVER_JWT}'})
try:
    payload = json.loads(urllib.request.urlopen(request, context=context, timeout=30).read())
    print('IDS', json.dumps([m.get('id') for m in payload.get('data') or []]))
except urllib.error.HTTPError as error:
    print('HTTPERR', error.code, error.read().decode()[:200])
except Exception as error:
    print('ERR', type(error).__name__, error)
"
}

# Count the embedding entries of a journal dump, and report their model names
# and credentials. Written in python because the journal is JSON and a shell
# substring match would confuse a model name with a credential label.
journal_summary() {
  printf '%s' "$1" | python3 -c "
import json, sys
raw = sys.stdin.read()
marker = raw.find('JOURNAL ')
if marker < 0:
    print('UNREADABLE')
    raise SystemExit(0)
try:
    payload = json.loads(raw[marker + len('JOURNAL '):].strip())
except Exception:
    print('UNREADABLE')
    raise SystemExit(0)
rows = [r for r in payload.get('data') or [] if r.get('path') == '/v1/embeddings']
print('COUNT', len(rows))
print('MODELS', json.dumps(sorted({str(r.get('model')) for r in rows})))
print('CREDENTIALS', json.dumps(sorted({str(r.get('credential')) for r in rows})))
"
}

field() { printf '%s' "$1" | awk -v key="$2" '$1 == key {sub(/^[^ ]+ /, ""); print; exit}'; }

# ── Probe A — the seeded model, end to end ───────────────────────────────────
journal_reset
A_OUT="$(llm_post embeddings "{'model': '${SEEDED_MODEL}', 'encoding_format': 'float', 'input': 'embedding path probe A'}")"
case "$A_OUT" in
  OK*'"embedding"'*) pass "the seeded model answered with an embedding" ;;
  *) fail "the seeded model '${SEEDED_MODEL}' did not answer with an embedding: $(printf '%s' "$A_OUT" | tr '\n' ' ' | cut -c1-200)" ;;
esac

A_JOURNAL="$(journal_summary "$(journal_read)")"
A_COUNT="$(field "$A_JOURNAL" COUNT)"
A_MODELS="$(field "$A_JOURNAL" MODELS)"
A_CREDENTIALS="$(field "$A_JOURNAL" CREDENTIALS)"
if [ "${A_COUNT:-0}" -ge 1 ] 2>/dev/null; then
  pass "the provider was really called (${A_COUNT} embedding request(s))"
else
  fail "no embedding request reached the provider. A 200 with no upstream call means nothing was embedded. Journal: ${A_JOURNAL}"
fi
case "$A_MODELS" in
  *"\"${WIRE_MODEL}\""*) pass "the call carried the model name '${WIRE_MODEL}'" ;;
  *) fail "the call carried ${A_MODELS:-nothing}, not '${WIRE_MODEL}'. A vector width would not have shown this." ;;
esac
case "$A_CREDENTIALS" in
  *"\"${EXPECTED_CREDENTIAL}\""*) pass "the gateway resolved project ${DRIVER_PROJECT} (credential ${EXPECTED_CREDENTIAL})" ;;
  *) fail "the gateway used ${A_CREDENTIALS:-no credential}, not ${EXPECTED_CREDENTIAL} — it resolved the wrong project" ;;
esac

# ── Probe B — a model name no row holds ──────────────────────────────────────
journal_reset
B_OUT="$(llm_post embeddings "{'model': '${UNKNOWN_MODEL}', 'encoding_format': 'float', 'input': 'embedding path probe B'}")"
case "$B_OUT" in
  *'HTTPERR 404'*) pass "an unknown embedding model is refused with 404" ;;
  OK*) fail "an unknown embedding model ANSWERED. The run silently used some other model: $(printf '%s' "$B_OUT" | tr '\n' ' ' | cut -c1-200)" ;;
  *) fail "an unknown embedding model failed with the wrong shape: $(printf '%s' "$B_OUT" | tr '\n' ' ' | cut -c1-200)" ;;
esac
case "$B_OUT" in
  *"${UNKNOWN_MODEL}"*) pass "the refusal names the model that was asked for" ;;
  *) fail "the refusal does not name '${UNKNOWN_MODEL}', so the operator cannot tell which name failed: $(printf '%s' "$B_OUT" | tr '\n' ' ' | cut -c1-200)" ;;
esac
B_JOURNAL="$(journal_summary "$(journal_read)")"
B_COUNT="$(field "$B_JOURNAL" COUNT)"
if [ "${B_COUNT:-1}" = "0" ]; then
  pass "the refused model made NO upstream call"
else
  fail "the refused model still reached the provider ${B_COUNT} time(s) — a different model was substituted. Journal: ${B_JOURNAL}"
fi

# ── Probes C and D — the public project's two rows (issue #458) ──────────────
MODELS_OUT="$(llm_models)"
case "$MODELS_OUT" in
  *'standalone-shared-embedding'*) pass "GET /llm/v1/models lists the public project's shared model" ;;
  *) fail "GET /llm/v1/models omits 'standalone-shared-embedding'. The gateway is reading one scope only, so no platform-shared model is served (#458). Raw: $(printf '%s' "$MODELS_OUT" | tr '\n' ' ' | cut -c1-300)" ;;
esac
case "$MODELS_OUT" in
  *'standalone-private-embedding'*) fail "GET /llm/v1/models lists 'standalone-private-embedding', which project 1 did NOT publish. The public scope is read without its shared predicate." ;;
  *) pass "the public project's unpublished model stays invisible" ;;
esac

journal_reset
C_OUT="$(llm_post embeddings "{'model': 'standalone-shared-embedding', 'encoding_format': 'float', 'input': 'embedding path probe C'}")"
case "$C_OUT" in
  OK*'"embedding"'*) pass "the public project's shared model dispatches" ;;
  *'HTTPERR 404'*) fail "the public project's shared model answered 404 (#458). elitea-main admits this model and the gateway cannot resolve it, so an index run starts and then dies in the worker. Raw: $(printf '%s' "$C_OUT" | tr '\n' ' ' | cut -c1-200)" ;;
  *) fail "the public project's shared model failed: $(printf '%s' "$C_OUT" | tr '\n' ' ' | cut -c1-200)" ;;
esac
C_JOURNAL="$(journal_summary "$(journal_read)")"
C_MODELS="$(field "$C_JOURNAL" MODELS)"
case "$C_MODELS" in
  *"\"${WIRE_MODEL}\""*) pass "the shared model dispatched the provider's own wire name '${WIRE_MODEL}'" ;;
  *) fail "the shared model dispatched ${C_MODELS:-nothing}, not '${WIRE_MODEL}'" ;;
esac

D_OUT="$(llm_post embeddings "{'model': 'standalone-private-embedding', 'encoding_format': 'float', 'input': 'embedding path probe D'}")"
case "$D_OUT" in
  *'HTTPERR 404'*) pass "the public project's unpublished model is refused" ;;
  OK*) fail "the public project's UNPUBLISHED model dispatched for project ${DRIVER_PROJECT}. That is a tenant-isolation fault." ;;
  *) fail "the unpublished model failed with the wrong shape: $(printf '%s' "$D_OUT" | tr '\n' ' ' | cut -c1-200)" ;;
esac

# ── The seed agreement (issue #468) ──────────────────────────────────────────
# The toolkit tells the worker which model to ask for. The configuration row is
# what elitea-main and the gateway both resolve. When the two disagree, the run
# is admitted and then fails inside the worker with a bare 404.
TOOLKIT_MODEL="$(psql_read "SELECT settings->>'embedding_model' FROM p_${DRIVER_PROJECT}.elitea_tools
   WHERE settings ? 'embedding_model' ORDER BY id LIMIT 1")"
if [ -z "$TOOLKIT_MODEL" ]; then
  echo "  · no indexable toolkit in project ${DRIVER_PROJECT}; run seed-index to include the agreement assertion"
  EXPECTED_ASSERTIONS=$((EXPECTED_ASSERTIONS - 1))
elif [ "$TOOLKIT_MODEL" = "$SEEDED_MODEL" ]; then
  pass "the toolkit and the catalogue name one model ('${SEEDED_MODEL}')"
else
  fail "the toolkit asks for '${TOOLKIT_MODEL}' and the catalogue holds '${SEEDED_MODEL}' (#468).
       An index run is admitted and then fails in the worker with 404.
       The two seed steps read different variables: LLM_EMBEDDING_MODEL and INDEX_EMBEDDING_MODEL."
fi

echo "→ embedding path: ${ran} assertion(s) ran, ${failed} failed (expected ${EXPECTED_ASSERTIONS} to run)."
if [ "$ran" -ne "$EXPECTED_ASSERTIONS" ]; then
  echo "→ FAILED: assertions were skipped. A skipped assertion is not a pass." >&2
  exit 1
fi
[ "$failed" -eq 0 ] || exit 1
echo "→ embedding path OK."
