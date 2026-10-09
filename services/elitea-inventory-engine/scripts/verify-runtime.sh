#!/bin/sh
# Resolve every library the engine binary needs against the runtime image
# mounted at /runtime-root, without starting the binary: no system OpenSSL
# linked or shipped, CA roots present, and the whole closure found inside
# the runtime root. Run by services/elitea-inventory-engine/Containerfile in
# the builder stage (a script file, not a Dockerfile heredoc: podman's
# builder on the CI runners has no heredoc support).
set -eu
artifact="$1"
readelf --sections --wide "$artifact" | grep -q '[.]dep-v0 '
if readelf --dynamic "$artifact" | grep -Eq 'Shared library: \[(libssl|libcrypto)[.]'; then
    echo 'System OpenSSL is not part of this runtime.' >&2
    exit 1
fi
test -z "$(find /runtime-root/var/lib/dpkg/status.d -name 'libssl*')"
test -z "$(find /runtime-root -name 'libssl.so*' -o -name 'libcrypto.so*')"
test -s /runtime-root/etc/ssl/certs/ca-certificates.crt
interpreter="$(readelf --program-headers --wide "$artifact" | sed -n 's/.*Requesting program interpreter: \(.*\)]/\1/p')"
test -n "$interpreter"
triplet="$(gcc -dumpmachine)"
"/runtime-root$interpreter" --inhibit-cache \
    --library-path "/runtime-root/lib/$triplet:/runtime-root/usr/lib/$triplet:/runtime-root/lib:/runtime-root/usr/lib" \
    --list "$artifact" > /tmp/runtime-libraries
cat /tmp/runtime-libraries
awk '$2 == "=>" && $3 !~ /^\/runtime-root\// { exit 1 }' /tmp/runtime-libraries
