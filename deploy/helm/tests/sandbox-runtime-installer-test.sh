#!/usr/bin/env bash
# The elitea-sandbox-runtime installer (files/install.sh) against a fake host:
# a scratch root and a fake `host` command that plays systemd, k3s (which
# re-renders config.toml from its template on restart), containerd
# (`config dump`, merging imports per config version) and crictl. Proves: one restart per
# change and none when current, no download when current, both checksums
# enforced, foreign templates kept, rollback on a failed restart, and the
# containerd-mode refusals.
#
# Needs Linux bash, coreutils and python3 >= 3.11 (tomllib), as on the CI
# runner; locally: podman run --rm -v "$PWD:/src:ro" -w /src python:3.12-slim-trixie \
#   bash deploy/helm/tests/sandbox-runtime-installer-test.sh
set -euo pipefail
cd "$(dirname "$0")/../../.."
INSTALL="$PWD/deploy/helm/elitea-sandbox-runtime/files/install.sh"
CHECK="$PWD/deploy/helm/elitea-sandbox-runtime/files/check.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fails=0
pass() { echo "  ok: $1"; }
fail() { echo "  FAIL: $1" >&2; fails=$((fails + 1)); }

# A fake release: the tarball layout gVisor ships since 20260831.
mkdir -p "$work/release/20261005.0/x86_64" "$work/release/20261005.0/aarch64" "$work/pkg/gvisor-bin"
printf '#!/bin/sh\necho "runsc version release-20261005.0"\n' > "$work/pkg/runsc"
printf '#!/bin/sh\nexit 0\n' > "$work/pkg/containerd-shim-runsc-v1"
printf 'x\n' > "$work/pkg/gvisor-bin/runsc-metric-server"
chmod +x "$work/pkg/runsc" "$work/pkg/containerd-shim-runsc-v1"
python3 - "$work/pkg" "$work/gvisor.tar.bz2" <<'PY'
import sys, tarfile
with tarfile.open(sys.argv[2], "w:bz2") as tar:
    for name in ["runsc", "containerd-shim-runsc-v1", "gvisor-bin"]:
        tar.add(f"{sys.argv[1]}/{name}", arcname=name)
PY
for arch in x86_64 aarch64; do
  cp "$work/gvisor.tar.bz2" "$work/release/20261005.0/$arch/"
  (cd "$work/release/20261005.0/$arch" && sha512sum gvisor.tar.bz2 > gvisor.tar.bz2.sha512)
done
SHA=$(sha512sum "$work/gvisor.tar.bz2" | awk '{print $1}')

# The fake `containerd config dump`: config.toml plus its imports, merged as
# containerd does. Version 2 (containerd 1.7) replaces each plugin table an
# import names wholesale; version 3 (containerd 2.x, PR #9982) merges per key.
# $root/replace-plugin forces the wholesale merge on version 3 too. Emits
# one fully-qualified table per section, sorted, so lines compare.
cat > "$work/fakedump.py" <<'PY'
import glob, json, os, re, sys, tomllib
root = sys.argv[1]
config = tomllib.load(open(f"{root}/etc/containerd/config.toml", "rb"))
wholesale = config.get("version") == 2 or os.path.exists(f"{root}/replace-plugin")
def deep(to, frm):
    for key, value in frm.items():
        if isinstance(value, dict) and isinstance(to.get(key), dict):
            deep(to[key], value)
        else:
            to[key] = value
for pattern in config.get("imports", []):
    for path in sorted(glob.glob(root + pattern)):
        imported = tomllib.load(open(path, "rb"))
        for key, value in imported.items():
            if key == "plugins" and wholesale:
                config.setdefault("plugins", {}).update(value)
            elif isinstance(value, dict):
                deep(config.setdefault(key, {}), value)
            elif key not in ("imports", "version"):
                config[key] = value
def name(key):
    return key if re.fullmatch(r"[A-Za-z0-9_-]+", key) else json.dumps(key)
def value(v):
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, list):
        return "[" + ", ".join(value(x) for x in v) + "]"
    return json.dumps(v) if isinstance(v, str) else str(v)
def emit(table, path):
    if path:
        print("[" + ".".join(name(p) for p in path) + "]")
    for key in sorted(table):
        if not isinstance(table[key], dict):
            print(f"  {name(key)} = {value(table[key])}")
    for key in sorted(table):
        if isinstance(table[key], dict):
            emit(table[key], path + [key])
emit(config, [])
PY

# The fake host. HOST_ROOT is the node's /; $LOG records restarts.
cat > "$work/fakehost" <<'FAKE'
#!/bin/bash
set -euo pipefail
root=$HOST_ROOT
k3sdir="$root/var/lib/rancher/k3s/agent/etc/containerd"
render_k3s() {
  local tmpl=""
  if [ "${K3S_VERSION:-3}" = 3 ] && [ -f "$k3sdir/config-v3.toml.tmpl" ]; then tmpl="$k3sdir/config-v3.toml.tmpl"
  elif [ -f "$k3sdir/config.toml.tmpl" ]; then tmpl="$k3sdir/config.toml.tmpl"; fi
  if [ -n "$tmpl" ]; then
    python3 - "$tmpl" "$k3sdir/base.toml" > "$k3sdir/config.toml" <<'PY'
import sys
template, base = open(sys.argv[1]).read(), open(sys.argv[2]).read()
print(template.replace('{{ template "base" . }}', base.rstrip("\n")))
PY
  else
    cp "$k3sdir/base.toml" "$k3sdir/config.toml"
  fi
}
case "$1 ${2:-}" in
  "systemctl restart")
    echo "restart $3" >> "$LOG"
    [ "$3" = containerd ] || render_k3s
    ;;
  "systemctl is-active") [ ! -f "$root/fail-start" ] ;;
  "systemctl cat") [ "${FAKE_AGENT:-}" = 1 ] ;;
  "systemctl show")
    # containerd became active $ACTIVE_AGO seconds ago (default: now). The
    # human-readable timestamp carries a zone abbreviation GNU date rejects.
    case "$*" in
      *ActiveEnterTimestampMonotonic*) python3 -c 'import sys, time; print(int((time.monotonic() - float(sys.argv[1])) * 1e6))' "${ACTIVE_AGO:-0}" ;;
      *ActiveEnterTimestamp*) echo "Mon 2026-10-19 12:00:00 NZDT" ;;
      *) exit 2 ;;
    esac ;;
  "containerd config") python3 "$(dirname "$0")/fakedump.py" "$root" ;;
  "k3s crictl") grep -q 'runtimes.runsc' "$k3sdir/config.toml" && echo '{"config":{"containerd":{"runtimes":{"runsc":{}}}}}' || echo '{}' ;;
  "sh -c") exit 1 ;;
  *)
    if [ "$1" = /usr/local/bin/runsc ]; then shift; exec "$root/usr/local/bin/runsc" "$@"; fi
    echo "fakehost: unexpected $*" >&2; exit 2 ;;
esac
FAKE
chmod +x "$work/fakehost"

new_node() {
  node="$work/node-$1"; rm -rf "$node"; mkdir -p "$node"
  export HOST_ROOT="$node" LOG="$node/log" HOST_EXEC="$work/fakehost"
  : > "$LOG"
}
k3s_node() {
  new_node "$1"
  mkdir -p "$HOST_ROOT/var/lib/rancher/k3s/agent/etc/containerd"
  printf 'version = 3\n\n[plugins."io.containerd.cri.v1.images"]\n  snapshotter = "overlayfs"\n' \
    > "$HOST_ROOT/var/lib/rancher/k3s/agent/etc/containerd/base.toml"
  cp "$HOST_ROOT/var/lib/rancher/k3s/agent/etc/containerd/base.toml" "$HOST_ROOT/var/lib/rancher/k3s/agent/etc/containerd/config.toml"
}
run() { env MODE="$MODE" GVISOR_RELEASE=20261005.0 GVISOR_BASE_URL="file://$work/release" \
  GVISOR_SHA512_X86_64="${PIN:-$SHA}" GVISOR_SHA512_AARCH64="${PIN:-$SHA}" ALLOW_FILE_URL=true \
  RESTART_TIMEOUT=$TIMEOUT bash "$INSTALL" > "$HOST_ROOT/out" 2>&1; }
TIMEOUT=4
restarts() { grep -c '^restart' "$LOG" || true; }
tmpl() { echo "$HOST_ROOT/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.tmpl"; }

echo "== k3s mode"
MODE=k3s
k3s_node fresh
if MODE=k3s bash "$CHECK" >/dev/null 2>&1; then fail "ready before the install"; else pass "not ready before the install"; fi
run && pass "a fresh node installs" || { cat "$HOST_ROOT/out"; fail "a fresh node installs"; }
[ "$(restarts)" = 1 ] && grep -qx 'restart k3s' "$LOG" && pass "one restart of k3s" || fail "restarts: $(cat "$LOG")"
[ -x "$HOST_ROOT/usr/local/bin/runsc" ] && [ -x "$HOST_ROOT/usr/local/bin/containerd-shim-runsc-v1" ] \
  && [ -x "$HOST_ROOT/usr/local/bin/gvisor-bin/runsc-metric-server" ] && [ ! -e "$HOST_ROOT/usr/local/bin/runsc-metric-server" ] \
  && pass "runsc, the shim and the gvisor-bin/ sidecars, nothing else" || fail "binaries"
grep -q '^file:gvisor-bin/runsc-metric-server=' "$HOST_ROOT/var/lib/elitea-sandbox-runtime/gvisor.state" && pass "the state hashes every file" || fail "state"
head -1 "$(tmpl)" | grep -qF '{{ template "base" . }}' && grep -qF "[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]" "$(tmpl)" \
  && pass "config-v3.toml.tmpl from k3s's base, v3 table" || { cat "$(tmpl)"; fail "template content"; }
MODE=k3s bash "$CHECK" && pass "ready after the install" || fail "not ready after the install"
cp "$(tmpl)" "$work/first.tmpl"
rm -rf "$work/release/20261005.0/x86_64" "$work/release/20261005.0/aarch64"
run && [ "$(restarts)" = 1 ] && cmp -s "$(tmpl)" "$work/first.tmpl" && grep -q 'binaries are current' "$HOST_ROOT/out" \
  && pass "a second run downloads nothing and restarts nothing" || { cat "$HOST_ROOT/out"; fail "idempotent rerun"; }
for arch in x86_64 aarch64; do
  mkdir -p "$work/release/20261005.0/$arch"
  cp "$work/gvisor.tar.bz2" "$work/release/20261005.0/$arch/"
  (cd "$work/release/20261005.0/$arch" && sha512sum gvisor.tar.bz2 > gvisor.tar.bz2.sha512)
done

# A sidecar changed on the node: the install is not current, so it is redone.
echo tampered > "$HOST_ROOT/usr/local/bin/gvisor-bin/runsc-metric-server"
run && grep -q 'downloading' "$HOST_ROOT/out" && grep -qx x "$HOST_ROOT/usr/local/bin/gvisor-bin/runsc-metric-server" && [ "$(restarts)" = 1 ] \
  && pass "a changed sidecar is reinstalled, with no restart" || { cat "$HOST_ROOT/out"; fail "sidecar repair"; }

k3s_node agent
FAKE_AGENT=1 run && grep -qx 'restart k3s-agent' "$LOG" && pass "an agent node restarts k3s-agent" || fail "agent restart: $(cat "$LOG")"

k3s_node foreign
printf '{{ template "base" . }}\n\n[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.custom]\n  runtime_type = "io.containerd.runc.v2"\n\n\n' > "$(tmpl)"
run && grep -q 'runtimes.custom' "$(tmpl)" && grep -q 'runtimes.runsc' "$(tmpl)" && [ "$(restarts)" = 1 ] \
  && pass "an operator template is kept and extended" || fail "foreign template"
run && [ "$(restarts)" = 1 ] && pass "and is stable on the next run" || { cat "$(tmpl)"; fail "foreign template rerun"; }

k3s_node v2
printf 'version = 2\n[plugins."io.containerd.grpc.v1.cri"]\n' > "$HOST_ROOT/var/lib/rancher/k3s/agent/etc/containerd/config.toml"
cp "$HOST_ROOT/var/lib/rancher/k3s/agent/etc/containerd/config.toml" "$HOST_ROOT/var/lib/rancher/k3s/agent/etc/containerd/base.toml"
K3S_VERSION=2 run && grep -qF '[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]' \
  "$HOST_ROOT/var/lib/rancher/k3s/agent/etc/containerd/config.toml.tmpl" && pass "containerd 1.7: config.toml.tmpl, v2 table" || fail "v2 template"

k3s_node rollback
printf '{{ template "base" . }}\n# operator\n' > "$(tmpl)"
cp "$(tmpl)" "$work/operator.tmpl"
touch "$HOST_ROOT/fail-start"
started=$(date +%s)
if run; then fail "a failed restart reads as success"; else pass "a failed restart fails the install"; fi
# One RESTART_TIMEOUT for the restart, one for the rollback's restart, plus
# slack; a wait that restarts its clock per probe takes about T^2/2 + T.
elapsed=$(( $(date +%s) - started ))
[ "$elapsed" -le $((2 * TIMEOUT + 3)) ] && pass "the rollback wait is bounded by the timeout (${elapsed}s)" \
  || fail "rollback took ${elapsed}s, over 2x${TIMEOUT}s"
cmp -s "$(tmpl)" "$work/operator.tmpl" && pass "and restores the previous template" || fail "rollback content"
[ "$(restarts)" = 2 ] && pass "one restart, one rollback restart" || fail "rollback restarts: $(cat "$LOG")"
compgen -G "$HOST_ROOT/var/lib/elitea-sandbox-runtime/backup/config-v3.toml.tmpl.*" >/dev/null && pass "a backup is kept" || fail "backup"

k3s_node badsum
PIN=$(printf '0%.0s' $(seq 128)) run && fail "a wrong pin installs" || pass "a wrong pinned SHA-512 is refused"
[ ! -e "$HOST_ROOT/usr/local/bin/runsc" ] && [ "$(restarts)" = 0 ] && pass "and nothing is installed or restarted" || fail "badsum effects"
k3s_node badpublished
echo "$(printf 'f%.0s' $(seq 128))  gvisor.tar.bz2" > "$work/release/20261005.0/x86_64/gvisor.tar.bz2.sha512"
cp "$work/release/20261005.0/x86_64/gvisor.tar.bz2.sha512" "$work/release/20261005.0/aarch64/"
run && fail "a published checksum mismatch installs" || pass "a published checksum that differs from the pin is refused"
for arch in x86_64 aarch64; do (cd "$work/release/20261005.0/$arch" && sha512sum gvisor.tar.bz2 > gvisor.tar.bz2.sha512); done
k3s_node latest
env MODE=k3s GVISOR_RELEASE=latest GVISOR_SHA512_X86_64="$SHA" GVISOR_SHA512_AARCH64="$SHA" bash "$INSTALL" >/dev/null 2>&1 \
  && fail "latest accepted" || pass "release latest is refused"

echo "== containerd mode"
MODE=containerd
CFG() { echo "$HOST_ROOT/etc/containerd/config.toml"; }
DROPIN() { echo "$HOST_ROOT/etc/containerd/config.d/50-elitea-runsc.toml"; }
dump() { "$work/fakehost" containerd config dump; }
# Version 2 (containerd 1.7) with a runc runtime, or version 3 (containerd 2.x).
cd_node() {
  new_node "$1"; mkdir -p "$HOST_ROOT/etc/containerd"
  printf 'version = 2\n\n[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]\n  SystemdCgroup = true\n' > "$(CFG)"
}
cd3_node() {
  new_node "$1"; mkdir -p "$HOST_ROOT/etc/containerd"
  printf "version = 3\n\n[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc.options]\n  SystemdCgroup = true\n" > "$(CFG)"
}

echo "-- config version 2: the runtime goes into config.toml"
cd_node plain
run && pass "a plain containerd 1.7 node installs" || { cat "$HOST_ROOT/out"; fail "containerd install"; }
grep -q '^imports' "$(CFG)" && fail "imports added on version 2" || pass "no imports added"
[ ! -e "$(DROPIN)" ] && pass "no drop-in" || fail "a drop-in on version 2"
grep -qxF '[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]' "$(CFG)" && grep -qxF 'SystemdCgroup = true' <(sed 's/^ *//' "$(CFG)") \
  && pass "config.toml keeps its content and gains the v2 runsc table" || { cat "$(CFG)"; fail "v2 config content"; }
[ "$(restarts)" = 1 ] && grep -qx 'restart containerd' "$LOG" && pass "one containerd restart" || fail "restarts: $(cat "$LOG")"
cp "$(CFG)" "$work/v2-first.toml"
run && [ "$(restarts)" = 1 ] && cmp -s "$(CFG)" "$work/v2-first.toml" && pass "a second run changes and restarts nothing" || fail "containerd rerun"
MODE=containerd bash "$CHECK" && pass "ready (no crictl: config dump + restart time)" || fail "containerd readiness"
ACTIVE_AGO=600 MODE=containerd bash "$CHECK" && fail "ready with a containerd started before the change" \
  || pass "not ready while containerd predates the change"

# EKS AL2023-style: imports already cover config.d, and the cri section is
# customised. A cri drop-in would replace that whole table on containerd 1.7.
cd_node customised
cat > "$(CFG)" <<'TOML'
version = 2
imports = ["/etc/containerd/config.d/*.toml"]

[plugins."io.containerd.grpc.v1.cri"]
  sandbox_image = "602401143452.dkr.ecr.us-west-2.amazonaws.com/eks/pause:3.10"
  enable_cdi = true

[plugins."io.containerd.grpc.v1.cri".containerd]
  default_runtime_name = "runc"
  discard_unpacked_layers = true

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
  runtime_type = "io.containerd.runc.v2"

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
  SystemdCgroup = true

[plugins."io.containerd.grpc.v1.cri".registry]
  config_path = "/etc/containerd/certs.d:/etc/docker/certs.d"
TOML
dump > "$work/customised-before.toml"
run && pass "a customised 1.7 cri section installs" || { cat "$HOST_ROOT/out"; fail "customised v2 install"; }
dump > "$work/customised-after.toml"
grep -vxF -f "$work/customised-after.toml" "$work/customised-before.toml" | grep -q . \
  && fail "settings lost: $(grep -vxF -f "$work/customised-after.toml" "$work/customised-before.toml")" \
  || pass "every effective setting survives (SystemdCgroup, sandbox_image, ...)"
grep -q 'io.containerd.runsc.v1' "$work/customised-after.toml" && [ ! -e "$(DROPIN)" ] && [ "$(restarts)" = 1 ] \
  && pass "runsc registered without a drop-in, one restart" || fail "customised v2 effects"

# An earlier installer's drop-in on a 1.7 node is removed, not kept beside the block.
cd_node stale
sed -i '1a imports = ["/etc/containerd/config.d/*.toml"]' "$(CFG)"
mkdir -p "$(dirname "$(DROPIN)")"
printf '[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]\n  runtime_type = "io.containerd.runsc.v1"\n' > "$(DROPIN)"
run && [ ! -e "$(DROPIN)" ] && grep -qF 'runtimes.runsc]' "$(CFG)" && pass "a stale version-2 drop-in is replaced by the block" \
  || { cat "$HOST_ROOT/out"; fail "stale drop-in"; }

cd_node existing
printf '\n[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]\n  runtime_type = "io.containerd.runsc.v1"\n' >> "$(CFG)"
run && [ "$(restarts)" = 0 ] && [ ! -e "$(DROPIN)" ] \
  && pass "a node that already registers runsc is left alone" || fail "existing runsc"

echo "-- config version 3: a drop-in, merged per key"
cd3_node plain3
run && pass "a containerd 2.x node installs" || { cat "$HOST_ROOT/out"; fail "containerd v3 install"; }
sed -n 2p "$(CFG)" | grep -qx 'imports = \["/etc/containerd/config.d/\*.toml"\]' \
  && pass "imports added after version" || { cat "$(CFG)"; fail "imports"; }
grep -qF "[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]" "$(DROPIN)" \
  && pass "the drop-in has the v3 table" || fail "drop-in"
dump | grep -qx '  SystemdCgroup = true' && pass "the runc options survive the per-key merge" || fail "v3 merge"
[ "$(restarts)" = 1 ] && run && [ "$(restarts)" = 1 ] && pass "one restart, none on the rerun" || fail "v3 restarts: $(cat "$LOG")"

cd3_node covered
sed -i '1a imports = ["/etc/containerd/config.d/*.toml"]' "$(CFG)"
cp "$(CFG)" "$work/covered.toml"
run && cmp -s "$(CFG)" "$work/covered.toml" && pass "covering imports leave config.toml as it is" || fail "covered"

cd3_node uncovered
sed -i '1a imports = ["/etc/other/*.toml"]' "$(CFG)"
run && fail "an uncovering imports list was rewritten" || pass "an imports list that does not cover the drop-in is refused"
[ ! -e "$(DROPIN)" ] && [ "$(restarts)" = 0 ] && pass "and nothing changes" || fail "uncovered effects"

# A containerd whose import replaced the plugin table: the line check refuses.
cd3_node dropping
touch "$HOST_ROOT/replace-plugin"
cp "$(CFG)" "$work/dropping.toml"
run && fail "an import that drops settings was applied" || pass "an import that drops settings is refused"
cmp -s "$(CFG)" "$work/dropping.toml" && [ ! -e "$(DROPIN)" ] \
  && [ "$(restarts)" = 0 ] && pass "rolled back before any restart" || fail "dropping effects"

[ "$fails" -eq 0 ] || { echo "$fails failure(s)" >&2; exit 1; }
echo "sandbox runtime installer: ok"
