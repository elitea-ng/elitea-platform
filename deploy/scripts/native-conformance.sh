#!/usr/bin/env bash
# The ADR-0025 native client conformance run (WP7): the Go suite in
# conformance/nativeclient against a fresh full standalone stack.
#
#   deploy/scripts/native-conformance.sh run           # whole lifecycle, then down -v
#   deploy/scripts/native-conformance.sh run --keep    # leave the stack up afterwards
#   deploy/scripts/native-conformance.sh test          # the suite only, against a stack
#                                                      #   this script already brought up
#
# The stack is the full standalone stack (the one CI's chat lanes run) plus
# deploy/docker-compose.native-conformance.yml. Its browser sign-in is OIDC
# through oidc-provider-mock; the suite also authors a SAML provider at run
# time and signs in through it (leg sso).
#
# WHY THERE IS NO FORM LEG HERE. The stack's Form plane is structural (see
# deploy/runtime/auth.form.yml): its public origin is the worker's private
# edge, and no edge in front of the browser runs the ForwardAuth hop that
# turns a Form session cookie into an identity. The native continue route
# reads the browser principal through the API group's own authentication,
# which accepts the OIDC/SAML session cookie and edge-forwarded identities,
# never the Form cookie directly (plan-native-auth §3.4 step 5). Measured
# 2026-10-04 with OIDC unset: the Form login succeeds and the continue route
# bounces three times into its loop guard ("Sign-in completed but this server
# could not read the browser session"). A Form leg needs a Form-plane browser
# edge with ForwardAuth, which this compose stack does not have.
#
# Variables (all optional):
#   STANDALONE_PROJECT     compose project      (elitea-native-conformance)
#   STANDALONE_PORT        traefik port         (8084); the origin is http://localhost:<port>
#   E2E_OIDC_PORT          oidc-mock port       (9400)
#   STANDALONE_SKIP_BUILD  1 = the caller built and asserted the images (CI's bake)
#   STANDALONE_OVERLAY     extra overlays, appended after this script's own
#   MOCK_LLM_CHUNK_DELAY_MS  mock token pacing (150): the stream scenario reads
#                          three frames of a LIVE turn before it disconnects
#
# The suite registers its own native client through the admin API after it
# has proved the no-client leg, so nothing here registers one.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
export STANDALONE_PROJECT="${STANDALONE_PROJECT:-elitea-native-conformance}"
export STANDALONE_PORT="${STANDALONE_PORT:-8084}"
export E2E_OIDC_PORT="${E2E_OIDC_PORT:-9400}"
export MOCK_LLM_CHUNK_DELAY_MS="${MOCK_LLM_CHUNK_DELAY_MS:-150}"
export STANDALONE_OVERLAY="${REPO_ROOT}/deploy/docker-compose.native-conformance.yml${STANDALONE_OVERLAY:+ ${STANDALONE_OVERLAY}}"

stack() { "${REPO_ROOT}/deploy/scripts/standalone-stack.sh" "$@"; }

run_suite() {
  echo "→ Running the native client conformance suite…"
  (
    cd "${REPO_ROOT}/conformance/nativeclient"
    ELITEA_CONFORMANCE_ORIGIN="http://localhost:${STANDALONE_PORT}" \
      go test -tags conformance -count=1 -v -timeout 15m ./...
  )
}

case "${1:-}" in
  test)
    stack seed-native
    run_suite
    ;;

  run)
    KEEP=0
    [ "${2:-}" = "--keep" ] && KEEP=1
    cleanup() {
      status=$?
      if [ "$status" -ne 0 ]; then
        echo "→ Stack logs (${STANDALONE_PROJECT}) — the run exited ${status}:"
        STANDALONE_LOG_TAIL="${STANDALONE_LOG_TAIL:-200}" stack logs elitea-main elitea-worker 2>&1 || true
      fi
      if [ "$KEEP" -eq 0 ]; then
        echo "→ Tearing down ${STANDALONE_PROJECT}…"
        stack down -v >/dev/null 2>&1 || true
      else
        echo "→ Leaving ${STANDALONE_PROJECT} up (--keep). Re-run the suite with:"
        echo "   STANDALONE_PROJECT=${STANDALONE_PROJECT} STANDALONE_PORT=${STANDALONE_PORT} $0 test"
      fi
    }
    trap cleanup EXIT
    stack certs
    if [ "${STANDALONE_SKIP_BUILD:-0}" = "1" ]; then
      echo "→ STANDALONE_SKIP_BUILD=1: images were built and asserted by the caller; not rebuilding"
    else
      stack build
    fi
    stack up
    stack seed
    stack seed-runtime
    stack seed-llm
    stack seed-native
    run_suite
    ;;

  *)
    sed -n '2,/^set -euo pipefail/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
    exit 2
    ;;
esac
