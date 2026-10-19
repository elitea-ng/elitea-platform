#!/bin/bash
# elitea-sandbox-runtime: install gVisor (runsc, containerd-shim-runsc-v1 and
# runsc's gvisor-bin/ sidecars)
# on this node and register the `runsc` runtime with its containerd.
#
# Runs as the installer DaemonSet's init container: privileged, hostPID, the
# host's / at $HOST_ROOT. Every host command goes through $HOST_EXEC (nsenter
# into PID 1's namespaces), so systemctl talks to the host's systemd.
#
# Idempotent. Nothing is downloaded when the installed binaries already hash to
# the pinned release, nothing is written when the config is already current,
# and containerd (or k3s) is restarted exactly once per config change. A change
# that does not come back healthy is rolled back to the saved previous file.
#
# Environment (set by the chart):
#   MODE                 k3s | containerd
#   GVISOR_RELEASE       e.g. 20261005.0
#   GVISOR_BASE_URL      https://storage.googleapis.com/gvisor/releases/release
#   GVISOR_SHA512_X86_64, GVISOR_SHA512_AARCH64
#                        the published SHA-512 of gvisor.tar.bz2 per arch
#   K3S_SERVICE          optional: k3s or k3s-agent (default: detected)
#   CONTAINERD_CONFIG    containerd mode: default /etc/containerd/config.toml
#   CONTAINERD_DROPIN_DIR containerd mode: default /etc/containerd/config.d
#   PATCH_IMPORTS        containerd mode: "true" adds the drop-in directory to
#                        a config.toml that has no `imports` (default true)
#   RESTART_TIMEOUT      seconds to wait for a restarted unit (default 180)
# Test hooks: HOST_ROOT, HOST_EXEC, ALLOW_FILE_URL=true.
set -euo pipefail

HOST_ROOT=${HOST_ROOT:-/host}
HOST_EXEC=${HOST_EXEC:-nsenter --target 1 --mount --uts --ipc --net --pid --}
BIN_DIR=/usr/local/bin
STATE_DIR=/var/lib/elitea-sandbox-runtime
RESTART_TIMEOUT=${RESTART_TIMEOUT:-180}
BEGIN="# BEGIN elitea-sandbox-runtime (managed; do not edit)"
END="# END elitea-sandbox-runtime"

log() { printf 'elitea-sandbox-runtime: %s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }
# Word splitting of HOST_EXEC is intended: it is a command prefix.
# shellcheck disable=SC2086
host() { $HOST_EXEC "$@"; }

: "${MODE:?MODE is required}" "${GVISOR_RELEASE:?GVISOR_RELEASE is required}"
: "${GVISOR_BASE_URL:=https://storage.googleapis.com/gvisor/releases/release}"
case "$MODE" in k3s|containerd) ;; *) die "MODE must be k3s or containerd, not $MODE" ;; esac
[[ "$GVISOR_RELEASE" =~ ^[0-9]{8}(\.[0-9]+)?$ ]] || die "GVISOR_RELEASE must be a dated release (YYYYMMDD[.N]), never latest"

case "$(uname -m)" in
  x86_64|amd64) ARCH=x86_64; EXPECTED=${GVISOR_SHA512_X86_64:-} ;;
  aarch64|arm64) ARCH=aarch64; EXPECTED=${GVISOR_SHA512_AARCH64:-} ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac
[[ "$EXPECTED" =~ ^[a-f0-9]{128}$ ]] || die "no pinned SHA-512 for $ARCH (gvisor.sha512.$ARCH)"

mkdir -p "$HOST_ROOT$STATE_DIR/backup"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

sha512() { sha512sum "$1" | awk '{print $1}'; }

# ── 1. The binaries ─────────────────────────────────────────────────────────
STATE="$HOST_ROOT$STATE_DIR/gvisor.state"
# Every file the state lists (runsc, the shim and runsc's gvisor-bin/
# sidecars) is installed and hashes as recorded for the pinned archive.
binaries_current() {
  [ -f "$STATE" ] || return 1
  grep -qx "tarball=$EXPECTED" "$STATE" || return 1
  grep -q '^file:runsc=' "$STATE" && grep -q '^file:containerd-shim-runsc-v1=' "$STATE" || return 1
  local line name
  while IFS= read -r line; do
    case "$line" in file:*) ;; *) continue ;; esac
    name=${line#file:}; name=${name%%=*}
    [ -x "$HOST_ROOT$BIN_DIR/$name" ] || return 1
    [ "file:$name=$(sha512 "$HOST_ROOT$BIN_DIR/$name")" = "$line" ] || return 1
  done < "$STATE"
}

fetch() {
  python3 - "$1" "$2" <<'PY'
import os, shutil, sys, urllib.request
url, out = sys.argv[1], sys.argv[2]
if not (url.startswith("https://") or (url.startswith("file://") and os.environ.get("ALLOW_FILE_URL") == "true")):
    sys.exit(f"refusing a non-https URL: {url}")
with urllib.request.urlopen(url, timeout=120) as response, open(out, "wb") as sink:
    shutil.copyfileobj(response, sink, 1 << 20)
PY
}

install_binaries() {
  local url="$GVISOR_BASE_URL/$GVISOR_RELEASE/$ARCH/gvisor.tar.bz2"
  log "downloading $url"
  fetch "$url" "$WORK/gvisor.tar.bz2" || die "download failed: $url"
  fetch "$url.sha512" "$WORK/gvisor.tar.bz2.sha512" || die "download failed: $url.sha512"
  local actual published
  actual=$(sha512 "$WORK/gvisor.tar.bz2")
  published=$(awk 'NR==1{print $1}' "$WORK/gvisor.tar.bz2.sha512")
  # Both must hold: the pinned value (what the chart was reviewed with) and
  # the release's own published checksum.
  [ "$actual" = "$EXPECTED" ] || die "gvisor.tar.bz2 SHA-512 $actual is not the pinned $EXPECTED"
  [ "$published" = "$EXPECTED" ] || die "the published SHA-512 $published is not the pinned $EXPECTED"
  python3 - "$WORK/gvisor.tar.bz2" "$WORK" <<'PY'
import os, re, sys, tarfile
# runsc and the shim at the top, and the sidecars runsc runs from the
# gvisor-bin/ directory beside it (gvisor_sentry and the rest: since the
# 20260831 releases runsc refuses to start a sandbox without them). Flat
# names only; nothing else is extracted.
archive, out = sys.argv[1], sys.argv[2]
top = {"runsc", "containerd-shim-runsc-v1"}
sidecar = re.compile(r"^gvisor-bin/[A-Za-z0-9][A-Za-z0-9_.-]*$")
found = []
with tarfile.open(archive, "r:bz2") as tar:
    for member in tar:
        name = member.name[2:] if member.name.startswith("./") else member.name
        if member.isdir() and name.rstrip("/") == "gvisor-bin":
            continue
        if name not in top and not sidecar.match(name):
            continue
        if not member.isreg():
            sys.exit(f"{name} is not a regular file in the archive")
        os.makedirs(os.path.dirname(f"{out}/files/{name}"), exist_ok=True)
        with tar.extractfile(member) as source, open(f"{out}/files/{name}", "wb") as sink:
            sink.write(source.read())
        found.append(name)
missing = top - set(found)
if missing:
    sys.exit(f"the archive lacks {sorted(missing)}")
with open(f"{out}/files.list", "w") as listing:
    listing.write("\n".join(sorted(found)) + "\n")
PY
  local name
  : > "$WORK/state"
  echo "release=$GVISOR_RELEASE" >> "$WORK/state"
  echo "tarball=$EXPECTED" >> "$WORK/state"
  mkdir -p "$HOST_ROOT$BIN_DIR/gvisor-bin"
  # Sidecars first, runsc last: a runsc that is new never sees old sidecars.
  for name in $(grep '^gvisor-bin/' "$WORK/files.list") containerd-shim-runsc-v1 runsc; do
    install -m 0755 "$WORK/files/$name" "$HOST_ROOT$BIN_DIR/$name.elitea-new"
    mv -f "$HOST_ROOT$BIN_DIR/$name.elitea-new" "$HOST_ROOT$BIN_DIR/$name"
    echo "file:$name=$(sha512 "$HOST_ROOT$BIN_DIR/$name")" >> "$WORK/state"
  done
  install -m 0644 "$WORK/state" "$STATE"
  log "installed gVisor $GVISOR_RELEASE ($ARCH) in $BIN_DIR"
}

mkdir -p "$HOST_ROOT$BIN_DIR"
if binaries_current; then
  log "gVisor $GVISOR_RELEASE binaries are current"
else
  install_binaries
fi
host "$BIN_DIR/runsc" --version >/dev/null || die "runsc does not run on this host"

# ── 2. The containerd runtime ───────────────────────────────────────────────
# The runtime table for a containerd config version (2: containerd 1.7, 3:
# containerd 2.x).
runtime_block() {
  local table
  if [ "$1" = 3 ]; then
    table="[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]"
  else
    table='[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]'
  fi
  printf '%s\n%s\n  runtime_type = "io.containerd.runsc.v1"\n%s\n' "$BEGIN" "$table" "$END"
}

config_version() {
  python3 - "$1" <<'PY'
import sys, tomllib
with open(sys.argv[1], "rb") as handle:
    print(tomllib.load(handle).get("version", 1))
PY
}

valid_toml() { python3 -c 'import sys, tomllib; tomllib.load(open(sys.argv[1], "rb"))' "$1"; }

# Strip our block from a file's content (stdin), keep everything else.
strip_block() {
  awk -v begin="$BEGIN" -v end="$END" '$0 == begin {skip=1; next} $0 == end {skip=0; next} !skip'
}

# Drop trailing blank lines.
trim_trailing() { sed -e :a -e '/^\n*$/{$d;N;ba' -e '}'; }

# Whether content outside our block already defines a runsc runtime.
foreign_runsc() { strip_block < "$1" | grep -q 'runtimes\.runsc\]'; }

backup() {
  local file=$1 copy
  copy="$HOST_ROOT$STATE_DIR/backup/$(basename "$file").$(date +%Y%m%dT%H%M%S)"
  if [ -e "$file" ]; then cp -p "$file" "$copy"; echo "$copy"; else echo ""; fi
}

restore() {
  local file=$1 copy=$2
  if [ -n "$copy" ]; then cp -p "$copy" "$file"; else rm -f "$file"; fi
}

wait_active() {
  local unit=$1 waited=0
  until host systemctl is-active --quiet "$unit"; do
    [ "$waited" -lt "$RESTART_TIMEOUT" ] || return 1
    sleep 2; waited=$((waited + 2))
  done
}

# Write $2 to $1 when it differs; echo "changed" or "unchanged".
converge_file() {
  local file=$1 desired=$2
  if [ -f "$file" ] && cmp -s "$file" "$desired"; then echo unchanged; return; fi
  echo changed
}

configure_k3s() {
  local dir="$HOST_ROOT/var/lib/rancher/k3s/agent/etc/containerd"
  local rendered="$dir/config.toml"
  [ -f "$rendered" ] || die "k3s has not rendered $rendered yet; is this a k3s node (try mode containerd)?"
  local version target
  version=$(config_version "$rendered")
  # The template k3s reads: config-v3.toml.tmpl for containerd 2.0 when
  # present, else config.toml.tmpl (https://docs.k3s.io/advanced#configuring-containerd).
  if [ "$version" = 3 ] && [ -f "$dir/config-v3.toml.tmpl" ]; then target="$dir/config-v3.toml.tmpl"
  elif [ -f "$dir/config.toml.tmpl" ]; then target="$dir/config.toml.tmpl"
  elif [ "$version" = 3 ]; then target="$dir/config-v3.toml.tmpl"
  else target="$dir/config.toml.tmpl"
  fi
  if [ -f "$target" ] && foreign_runsc "$target"; then
    log "$(basename "$target") already defines a runsc runtime outside this installer's block; leaving it"
    return 0
  fi
  runtime_block "$version" > "$WORK/block.toml"
  valid_toml "$WORK/block.toml" || die "the runtime block is not valid TOML"
  {
    # What is there (our old block removed, trailing blank lines dropped),
    # else k3s's own base template; then one blank line and our block.
    if [ -f "$target" ]; then strip_block < "$target" | trim_trailing; else printf '{{ template "base" . }}\n'; fi
    printf '\n'
    cat "$WORK/block.toml"
  } > "$WORK/desired.tmpl"
  [ "$(converge_file "$target" "$WORK/desired.tmpl")" = changed ] || { log "$(basename "$target") is current"; return 0; }

  local unit=${K3S_SERVICE:-}
  if [ -z "$unit" ]; then
    if host systemctl cat k3s-agent.service >/dev/null 2>&1; then unit=k3s-agent; else unit=k3s; fi
  fi
  local saved
  saved=$(backup "$target")
  install -m 0600 "$WORK/desired.tmpl" "$target"
  log "wrote $(basename "$target"); restarting $unit (once)"
  host systemctl restart "$unit" || true
  local waited=0
  until wait_active "$unit" && grep -q 'io.containerd.runsc.v1' "$rendered"; do
    waited=$((waited + 2))
    if [ "$waited" -ge "$RESTART_TIMEOUT" ]; then
      log "$unit did not come back with runsc; restoring the previous $(basename "$target")"
      restore "$target" "$saved"
      host systemctl restart "$unit" || true
      wait_active "$unit" || log "$unit is still not active after the rollback"
      die "the k3s containerd template change was rolled back"
    fi
    sleep 2
  done
  log "$unit is active and its containerd config registers runsc"
}

configure_containerd() {
  local config="$HOST_ROOT${CONTAINERD_CONFIG:-/etc/containerd/config.toml}"
  local dropin_dir=${CONTAINERD_DROPIN_DIR:-/etc/containerd/config.d}
  local dropin="$dropin_dir/50-elitea-runsc.toml"
  [ -f "$config" ] || die "$config does not exist; generate it first (containerd config default)"
  if foreign_runsc "$config"; then
    log "$(basename "$config") already defines a runsc runtime; leaving it"
    return 0
  fi
  local version
  version=$(config_version "$config")
  [ "$version" = 2 ] || [ "$version" = 3 ] || die "containerd config version $version is not supported (2 or 3)"
  runtime_block "$version" > "$WORK/dropin.toml"
  valid_toml "$WORK/dropin.toml" || die "the drop-in is not valid TOML"

  # The drop-in must be imported. Add the directory to a config with no
  # `imports`; refuse to rewrite an existing list that does not cover it.
  cp "$config" "$WORK/config.toml"
  local imports
  imports=$(python3 - "$config" "$dropin" <<'PY'
import fnmatch, sys, tomllib
config = tomllib.load(open(sys.argv[1], "rb"))
if "imports" not in config:
    print("absent")
elif any(fnmatch.fnmatch(sys.argv[2], pattern) for pattern in config["imports"]):
    print("covered")
else:
    print("uncovered")
PY
)
  case "$imports" in
    covered) ;;
    absent)
      [ "${PATCH_IMPORTS:-true}" = true ] || die "$(basename "$config") has no imports and PATCH_IMPORTS is off"
      # Top level, right after `version`, before any table.
      awk -v line="imports = [\"$dropin_dir/*.toml\"]" \
        '!done && /^version[[:space:]]*=/ {print; print line; done=1; next} {print} END {if (!done) exit 1}' \
        "$config" > "$WORK/config.toml" || die "$(basename "$config") has no top-level version line to add imports after"
      valid_toml "$WORK/config.toml" || die "adding imports would make $(basename "$config") invalid"
      ;;
    *) die "$(basename "$config") imports a list that does not cover $dropin; add it there or set the runtime by hand" ;;
  esac

  local config_state dropin_state
  config_state=$(converge_file "$config" "$WORK/config.toml")
  dropin_state=$(converge_file "$HOST_ROOT$dropin" "$WORK/dropin.toml")
  if [ "$config_state" = unchanged ] && [ "$dropin_state" = unchanged ]; then
    log "containerd config is current"; return 0
  fi

  # Validate with containerd itself before any restart: the merged config
  # must load, register runsc, and keep every setting it had.
  host containerd config dump > "$WORK/before.toml" 2>/dev/null || die "containerd config dump fails before any change"
  local saved_config saved_dropin
  saved_config=$(backup "$config")
  saved_dropin=$(backup "$HOST_ROOT$dropin")
  mkdir -p "$HOST_ROOT$dropin_dir"
  install -m 0644 "$WORK/dropin.toml" "$HOST_ROOT$dropin"
  install -m "$(stat -c %a "$config")" "$WORK/config.toml" "$config"
  rollback() {
    restore "$config" "$saved_config"
    restore "$HOST_ROOT$dropin" "$saved_dropin"
  }
  if ! host containerd config dump > "$WORK/after.toml" 2>"$WORK/dump.err"; then
    rollback; die "containerd rejects the new config: $(tail -1 "$WORK/dump.err")"
  fi
  if ! grep -q 'io.containerd.runsc.v1' "$WORK/after.toml"; then
    rollback; die "containerd does not see the runsc runtime through the import"
  fi
  # Every line of the old effective config must survive (an import that
  # replaced a whole plugin table would drop them).
  if grep -vxF -f "$WORK/after.toml" "$WORK/before.toml" | grep -v '^[[:space:]]*imports[[:space:]]*=' | grep -q .; then
    rollback; die "the import would drop existing containerd settings; add the runsc runtime to $(basename "$config") by hand"
  fi
  log "wrote the runsc drop-in; restarting containerd (once)"
  host systemctl restart containerd || true
  if ! wait_active containerd; then
    rollback
    host systemctl restart containerd || true
    die "containerd did not come back; the change was rolled back"
  fi
  date -u +%s > "$HOST_ROOT$STATE_DIR/containerd.restarted"
  log "containerd is active with the runsc runtime"
}

if [ "$MODE" = k3s ]; then configure_k3s; else configure_containerd; fi
log "done"
