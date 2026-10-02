#!/usr/bin/env bash
# Issue sandbox server certificates under the existing runtime CA.
# Preserve every existing certificate and key. Rotation is an operator action.
set -euo pipefail
runtime_dir="${1:?Pass the existing runtime CA directory}"
output_dir="${2:?Pass a private output directory}"
[[ -f "$runtime_dir/runtime-ca.crt" && -f "$runtime_dir/runtime-ca.key" ]]
umask 077
mkdir -p "$output_dir"
for name in elitea-sandbox-deno elitea-sandbox-rust postgres; do
  if [[ -e "$output_dir/$name.crt" || -e "$output_dir/$name.key" ]]; then
    [[ -f "$output_dir/$name.crt" && -f "$output_dir/$name.key" ]]
    openssl verify -purpose sslserver -verify_hostname "$name" -CAfile "$runtime_dir/runtime-ca.crt" "$output_dir/$name.crt" >/dev/null
    openssl x509 -in "$output_dir/$name.crt" -checkend 86400 -noout >/dev/null
    cert_public="$(openssl x509 -in "$output_dir/$name.crt" -pubkey -noout)"
    key_public="$(openssl pkey -in "$output_dir/$name.key" -pubout)"
    [[ "$cert_public" == "$key_public" ]]
    continue
  fi
  scratch="$(mktemp -d "$output_dir/.issue-XXXXXX")"
  trap 'rm -rf "$scratch"' EXIT
  openssl req -new -newkey rsa:2048 -nodes -keyout "$scratch/key" \
    -out "$scratch/request" -subj "/CN=$name" >/dev/null 2>&1
  printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:%s\n' "$name" > "$scratch/extensions"
  openssl x509 -req -in "$scratch/request" -CA "$runtime_dir/runtime-ca.crt" \
    -CAkey "$runtime_dir/runtime-ca.key" -set_serial "0x$(openssl rand -hex 16)" \
    -days 30 -sha256 -extfile "$scratch/extensions" -out "$scratch/certificate" >/dev/null 2>&1
  openssl verify -CAfile "$runtime_dir/runtime-ca.crt" "$scratch/certificate" >/dev/null
  mv "$scratch/key" "$output_dir/$name.key"
  mv "$scratch/certificate" "$output_dir/$name.crt"
  rm -rf "$scratch"
  trap - EXIT
done
printf 'Sandbox server certificates are ready; existing runtime identities are unchanged.\n'
