#!/usr/bin/env bash
# The Support Assistant widget journey (Playwright project `support-stack`)
# against the FULL standalone stack — the only stack where a support turn
# can run an agent to completion (runtime plane + worker + mock LLM).
#
#   apps/elitea-web/scripts/support-e2e.sh            # up + seed + run
#   apps/elitea-web/scripts/support-e2e.sh --keep     # leave the stack up
#
# Everything is chat-stream-e2e.sh: the same stack, certificates, seeds and
# stack assertions; only the Playwright project and the compose project /
# port differ. Beside the E2E stack, yes; beside another STANDALONE stack,
# no: oidc-mock's port is fixed at 9400 (see that script), so a kept
# chat-stream, deepwiki, index-stream or real-engine stack must come down
# first.
set -euo pipefail
export PLAYWRIGHT_PROJECT=support-stack
export CHAT_STREAM_PROJECT="${SUPPORT_STREAM_PROJECT:-elitea-supportassistant}"
export CHAT_STREAM_PORT="${SUPPORT_STREAM_PORT:-8088}"
exec "$(dirname "$0")/chat-stream-e2e.sh" "$@"
