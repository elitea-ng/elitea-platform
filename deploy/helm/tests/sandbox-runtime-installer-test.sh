#!/usr/bin/env bash
# The elitea-sandbox-runtime installer (files/install.sh) against a fake host:
# a scratch root and a fake `host` command that plays systemd, k3s (which
# re-renders config.toml from its template on restart), containerd
# (`config dump`, merging the drop-in) and crictl. Proves: one restart per
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
  "systemctl show") date -u ;;
  "containerd config")
    # A merge that keeps the base and adds each imported file, or (when
    # $root/replace-plugin exists) one where an import drops base settings.
    if [ -f "$root/replace-plugin" ] && compgen -G "$root/etc/containerd/config.d/*.toml" >/dev/null; then grep -v 'SystemdCgroup' "$root/etc/containerd/config.toml"; else cat "$root/etc/containerd/config.toml"; fi
    for f in "$root"/etc/containerd/config.d/*.toml; do if [ -f "$f" ]; then cat "$f"; fi; done
    ;;
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
  RESTART_TIMEOUT=4 bash "$INSTALL" > "$HOST_ROOT/out" 2>&1; }
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
if run; then fail "a failed restart reads as success"; else pass "a failed restart fails the install"; fi
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
cd_node() {
  new_node "$1"; mkdir -p "$HOST_ROOT/etc/containerd"
  printf 'version = 2\n\n[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]\n  SystemdCgroup = true\n' \
    > "$HOST_ROOT/etc/containerd/config.toml"
}
cd_node plain
run && pass "a plain containerd node installs" || { cat "$HOST_ROOT/out"; fail "containerd install"; }
sed -n 2p "$HOST_ROOT/etc/containerd/config.toml" | grep -qx 'imports = \["/etc/containerd/config.d/\*.toml"\]' \
  && pass "imports added after version" || { cat "$HOST_ROOT/etc/containerd/config.toml"; fail "imports"; }
grep -qF '[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]' "$HOST_ROOT/etc/containerd/config.d/50-elitea-runsc.toml" \
  && pass "the drop-in has the v2 table" || fail "drop-in"
[ "$(restarts)" = 1 ] && grep -qx 'restart containerd' "$LOG" && pass "one containerd restart" || fail "restarts: $(cat "$LOG")"
run && [ "$(restarts)" = 1 ] && pass "a second run restarts nothing" || fail "containerd rerun"
MODE=containerd bash "$CHECK" && pass "ready (no crictl: config dump + restart time)" || fail "containerd readiness"

cd_node covered
sed -i '1a imports = ["/etc/containerd/config.d/*.toml"]' "$HOST_ROOT/etc/containerd/config.toml"
cp "$HOST_ROOT/etc/containerd/config.toml" "$work/covered.toml"
run && cmp -s "$HOST_ROOT/etc/containerd/config.toml" "$work/covered.toml" && pass "covering imports leave config.toml as it is" || fail "covered"

cd_node uncovered
sed -i '1a imports = ["/etc/other/*.toml"]' "$HOST_ROOT/etc/containerd/config.toml"
run && fail "an uncovering imports list was rewritten" || pass "an imports list that does not cover the drop-in is refused"
[ ! -e "$HOST_ROOT/etc/containerd/config.d/50-elitea-runsc.toml" ] && [ "$(restarts)" = 0 ] && pass "and nothing changes" || fail "uncovered effects"

cd_node dropping
touch "$HOST_ROOT/replace-plugin"
cp "$HOST_ROOT/etc/containerd/config.toml" "$work/dropping.toml"
run && fail "an import that drops settings was applied" || pass "an import that drops settings is refused"
cmp -s "$HOST_ROOT/etc/containerd/config.toml" "$work/dropping.toml" && [ ! -e "$HOST_ROOT/etc/containerd/config.d/50-elitea-runsc.toml" ] \
  && [ "$(restarts)" = 0 ] && pass "rolled back before any restart" || fail "dropping effects"

cd_node existing
printf '\n[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]\n  runtime_type = "io.containerd.runsc.v1"\n' >> "$HOST_ROOT/etc/containerd/config.toml"
run && [ "$(restarts)" = 0 ] && [ ! -e "$HOST_ROOT/etc/containerd/config.d/50-elitea-runsc.toml" ] \
  && pass "a node that already registers runsc is left alone" || fail "existing runsc"

[ "$fails" -eq 0 ] || { echo "$fails failure(s)" >&2; exit 1; }
echo "sandbox runtime installer: ok"
