#!/usr/bin/env bash
# ci-secure-test-env.sh — provision the secured NATS permission tests (#1076)
# on a CI runner, and export their environment to $GITHUB_ENV.
#
#   scripts/nats/ci-secure-test-env.sh
#
# 1. Renders the NATS chart's own nats.conf (render-secure-conf.sh), scale-1
#    and HA — the permission table and the route TLS under test are the
#    chart's, never a copy.
# 2. Downloads the nats CLI the bootstrap script runs (pinned version and
#    SHA-256), because the tests create the JetStream assets with the real
#    bootstrap.sh exactly as the hook Job does.
# 3. Copies nats-server out of the nats:2.12 image when ELITEA_TEST_NATS_SERVER_BIN
#    is not already set (ci-go.yml extracts it for the restart test already).
# 4. Sets ELITEA_REQUIRE_NATS_SECURE_TEST=1, so a missing piece FAILS the
#    tests instead of skipping them: a permission test that skipped would
#    read exactly like one that passed.
set -euo pipefail

: "${RUNNER_TEMP:?RUNNER_TEMP is not set; this script is for CI runners}"
: "${GITHUB_ENV:?GITHUB_ENV is not set; this script is for GitHub Actions}"
root="$(cd "$(dirname "$0")/../.." && pwd)"

NATSCLI_VERSION="0.4.0"
NATSCLI_SHA256="8dbd437c826b953dbd7432cf890ef22ba3c33dccc3dce5e71b3e8d055427849c"

python3 -c 'import yaml' 2>/dev/null \
  || python3 -m pip install --quiet --disable-pip-version-check pyyaml

conf="${RUNNER_TEMP}/nats-secure.conf"
bash "${root}/scripts/nats/render-secure-conf.sh" "$conf"
# The HA profile, for the route identity test (natstest.StartCluster).
ha_conf="${RUNNER_TEMP}/nats-secure-ha.conf"
bash "${root}/scripts/nats/render-secure-conf.sh" "$ha_conf" "${root}/deploy/helm/nats/values-ha.yaml"

zip="${RUNNER_TEMP}/natscli.zip"
curl -fsSL -o "$zip" \
  "https://github.com/nats-io/natscli/releases/download/v${NATSCLI_VERSION}/nats-${NATSCLI_VERSION}-linux-amd64.zip"
echo "${NATSCLI_SHA256}  ${zip}" | sha256sum -c -
unzip -o -q "$zip" -d "${RUNNER_TEMP}/natscli"
cli="${RUNNER_TEMP}/natscli/nats-${NATSCLI_VERSION}-linux-amd64/nats"
chmod +x "$cli"
"$cli" --version

server="${ELITEA_TEST_NATS_SERVER_BIN:-}"
if [ -z "$server" ]; then
  path_in_image="$(docker run --rm --entrypoint sh nats:2.12-alpine -c 'command -v nats-server')"
  ctr="$(docker create nats:2.12-alpine)"
  server="${RUNNER_TEMP}/nats-server"
  docker cp "${ctr}:${path_in_image}" "$server"
  docker rm "$ctr" >/dev/null
  chmod +x "$server"
fi
"$server" --version

{
  echo "ELITEA_TEST_NATS_SECURE_CONF=${conf}"
  echo "ELITEA_TEST_NATS_SECURE_HA_CONF=${ha_conf}"
  echo "ELITEA_TEST_NATS_CLI_BIN=${cli}"
  echo "ELITEA_TEST_NATS_SERVER_BIN=${server}"
  echo "ELITEA_REQUIRE_NATS_SECURE_TEST=1"
} >> "$GITHUB_ENV"
echo "secured NATS permission tests provisioned"
