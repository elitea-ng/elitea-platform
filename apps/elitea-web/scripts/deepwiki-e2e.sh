#!/usr/bin/env bash
# The provider-backed DeepWiki journeys (Playwright project `deepwiki-stack`)
# against the FULL standalone stack — the only stack where the DeepWiki
# facade can be composed (it needs production Form authentication; the E2E
# stack is OIDC-only and has no token validator, so neither the facade nor
# the provider's callback bearer could authenticate there).
#
#   apps/elitea-web/scripts/deepwiki-e2e.sh            # up + seed + run
#   apps/elitea-web/scripts/deepwiki-e2e.sh --keep     # leave the stack up
#
# Everything is chat-stream-e2e.sh: the same stack, certificates, seeds and
# stack assertions; only the Playwright project and the compose project /
# port differ. Beside the E2E stack, yes; beside another STANDALONE stack,
# no: oidc-mock's port is fixed at 9400 (see that script), so a kept
# chat-stream or real-engine stack must come down first.
set -euo pipefail
export PLAYWRIGHT_PROJECT=deepwiki-stack
export CHAT_STREAM_PROJECT="${DEEPWIKI_STREAM_PROJECT:-elitea-deepwiki}"
export CHAT_STREAM_PORT="${DEEPWIKI_STREAM_PORT:-8086}"
# The fixture engine's pause between progress steps (the compose default is
# 1s). DWIKI-005/006 stop and reload over a run IN FLIGHT; at 1s a 5-step
# generation lasts ~5s, which one slow invoke + poll round trip on a CI runner
# can swallow whole — the first poll then carries every step and the terminal
# status, the panel never shows `running`, and the run lands its wiki. 3s
# (~15s a run) leaves room for a reload; Stop ends those runs early anyway.
export DEEPWIKI_FIXTURE_STEP_SECONDS="${DEEPWIKI_FIXTURE_STEP_SECONDS:-3}"
exec "$(dirname "$0")/chat-stream-e2e.sh" "$@"
