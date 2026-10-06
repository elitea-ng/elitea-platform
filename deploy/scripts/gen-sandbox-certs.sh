#!/usr/bin/env bash
# Issue sandbox server certificates under the existing runtime CA.
# Preserve every existing certificate and key. Rotation is an operator action.
set -euo pipefail
runtime_dir="${1:?Pass the existing runtime CA directory}"
output_dir="${2:?Pass a private output directory}"
if [[ "$#" -gt 3 || ( "$#" -eq 3 && "$3" != "--preparation" && "$3" != "--native" ) ]]; then
  printf 'Usage: %s <runtime-ca-directory> <private-output-directory> [--preparation|--native]\n' "$0" >&2
  exit 1
fi
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
if [[ "${3:-}" == "--preparation" || "${3:-}" == "--native" ]]; then
  # Content client leaves carry one DNS identity and clientAuth only.
  issue_preparation_identity() {
    local name="$1" dns="$2" usage="$3" purpose="$4" expected_usage="$5"
    local cert_public key_public san eku scratch
    if [[ -e "$output_dir/$name.crt" || -e "$output_dir/$name.key" ]]; then
      [[ -f "$output_dir/$name.crt" && -f "$output_dir/$name.key" ]] || return 1
      openssl verify -purpose "$purpose" -verify_hostname "$dns" \
        -CAfile "$runtime_dir/runtime-ca.crt" "$output_dir/$name.crt" >/dev/null
      openssl x509 -in "$output_dir/$name.crt" -checkend 86400 -noout >/dev/null
      san="$(openssl x509 -in "$output_dir/$name.crt" -noout -ext subjectAltName | tail -n +2 | tr -d '[:space:]')"
      eku="$(openssl x509 -in "$output_dir/$name.crt" -noout -ext extendedKeyUsage | tail -n +2 | tr -d '[:space:]')"
      if [[ "$san" != "DNS:$dns" || "$eku" != "$expected_usage" ]]; then
        printf 'Existing preparation certificate has an incompatible identity or purpose.\n' >&2
        return 1
      fi
      cert_public="$(openssl x509 -in "$output_dir/$name.crt" -pubkey -noout)"
      key_public="$(openssl pkey -in "$output_dir/$name.key" -pubout)"
      [[ "$cert_public" == "$key_public" ]] || return 1
      return
    fi
    scratch="$(mktemp -d "$output_dir/.issue-XXXXXX")"
    trap 'rm -rf "$scratch"' EXIT
    openssl req -new -newkey rsa:2048 -nodes -keyout "$scratch/key" \
      -out "$scratch/request" -subj "/CN=$dns" >/dev/null 2>&1
    printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=%s\nsubjectAltName=DNS:%s\n' "$usage" "$dns" > "$scratch/extensions"
    openssl x509 -req -in "$scratch/request" -CA "$runtime_dir/runtime-ca.crt" \
      -CAkey "$runtime_dir/runtime-ca.key" -set_serial "0x$(openssl rand -hex 16)" \
      -days 30 -sha256 -extfile "$scratch/extensions" -out "$scratch/certificate" >/dev/null 2>&1
    openssl verify -purpose "$purpose" -verify_hostname "$dns" \
      -CAfile "$runtime_dir/runtime-ca.crt" "$scratch/certificate" >/dev/null
    # Hard links fail if another issuer creates an output file concurrently.
    ln "$scratch/key" "$output_dir/$name.key"
    ln "$scratch/certificate" "$output_dir/$name.crt"
    rm -rf "$scratch"
    trap - EXIT
  }
  issue_preparation_identity elitea-sandbox-preparation elitea-sandbox-preparation \
    serverAuth sslserver TLSWebServerAuthentication
  issue_preparation_identity elitea-sandbox-preparation-content-client elitea-sandbox-preparation \
    clientAuth sslclient TLSWebClientAuthentication
  issue_preparation_identity elitea-sandbox-deno-content-client elitea-sandbox-deno \
    clientAuth sslclient TLSWebClientAuthentication
  if [[ "${3:-}" == "--native" ]]; then
    for profile in deno-native javascript-preparation typescript-preparation rust-preparation; do
      issue_preparation_identity "elitea-sandbox-$profile" "elitea-sandbox-$profile" \
        serverAuth sslserver TLSWebServerAuthentication
    done
    for profile in rust deno-native javascript-preparation typescript-preparation rust-preparation; do
      issue_preparation_identity "elitea-sandbox-$profile-content-client" "elitea-sandbox-$profile" \
        clientAuth sslclient TLSWebClientAuthentication
    done
    printf 'Native server and separate content client certificates are ready.\n'
  fi
  printf 'Preparation server and separate content client certificates are ready.\n'
fi
printf 'Sandbox server certificates are ready; existing runtime identities are unchanged.\n'
