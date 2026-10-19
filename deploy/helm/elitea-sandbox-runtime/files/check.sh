#!/bin/bash
# elitea-sandbox-runtime readiness: Ready only when runsc runs on the host and
# the node's containerd lists the runsc runtime.
set -euo pipefail
HOST_ROOT=${HOST_ROOT:-/host}
HOST_EXEC=${HOST_EXEC:-nsenter --target 1 --mount --uts --ipc --net --pid --}
# shellcheck disable=SC2086
host() { $HOST_EXEC "$@"; }

host /usr/local/bin/runsc --version >/dev/null
lists_runsc() { grep -q '"runsc"'; }
if [ "$MODE" = k3s ]; then
  # k3s bundles crictl; its `info` is the running containerd's CRI config.
  host k3s crictl info 2>/dev/null | lists_runsc
elif host sh -c 'command -v crictl' >/dev/null 2>&1; then
  host crictl --runtime-endpoint unix:///run/containerd/containerd.sock info 2>/dev/null | lists_runsc
else
  # No crictl on the host: the effective config registers runsc, and
  # containerd was restarted after the installer wrote it.
  host containerd config dump 2>/dev/null | grep -q 'io.containerd.runsc.v1'
  restarted=$(cat "$HOST_ROOT/var/lib/elitea-sandbox-runtime/containerd.restarted" 2>/dev/null || echo 0)
  # ActiveEnterTimestampMonotonic (microseconds of CLOCK_MONOTONIC, which
  # this pod shares with the host) converted to wall-clock seconds here. The
  # human-readable ActiveEnterTimestamp carries a zone abbreviation that GNU
  # date cannot parse for every zone. 0 means containerd never became active.
  since=$(host systemctl show containerd --property=ActiveEnterTimestampMonotonic --value)
  [[ "$since" =~ ^[1-9][0-9]*$ ]] || exit 1
  since=$(python3 -c 'import sys, time; print(int(time.time() - (time.monotonic() - int(sys.argv[1]) / 1e6)))' "$since")
  [ "$since" -ge "$restarted" ]
fi
