# shellcheck shell=bash
# stack-secrets.sh — the signing secrets of a local or CI compose stack.
#
# Source it, then call `stack_secrets_load <stack name>`. The compose files
# and the native tables carry NO literal secret (#2958): each one reads the
# value from the environment, and elitea-main, the LLM gateway and the
# sub-application hosts refuse to start with an empty, short or placeholder
# value. This file supplies the values.
#
# The first call for a stack name generates every value with openssl and
# writes them to deploy/.stack-secrets/<stack name>.env (mode 0600, ignored
# by git). Later calls read that file again, so the stack keeps the same keys
# across `up`, `restart` and `down` without -v: a vault wrapped with
# SECRETS_MASTER_KEY, a personal access token signed with
# APPLICATION_SECRET_KEY and the identity HMAC stay valid. A value that is
# already set in the environment wins over the file, so an engineer or a CI
# job can supply its own.
#
# Delete the file to rotate every value. Do it together with `down -v`: a
# new SECRETS_MASTER_KEY cannot open the project vaults the old one wrapped.

# The variables, one per line: NAME and how its value is generated.
#   b64     32 random bytes, standard base64 (44 characters)
#   fernet  32 random bytes, base64url with padding: a Fernet key
_stack_secret_specs() {
  cat <<'SPECS'
APPLICATION_SECRET_KEY b64
GATEWAY_IDENTITY_SECRET b64
ELITEA_DEEPWIKI_IDENTITY_SECRET b64
ELITEA_INVENTORY_IDENTITY_SECRET b64
SECRETS_MASTER_KEY fernet
SPECS
}

_stack_secret_value() {
  case "$1" in
    b64) openssl rand -base64 32 ;;
    fernet) openssl rand -base64 32 | tr '+/' '-_' ;;
    *) echo "stack-secrets: unknown kind $1" >&2; return 1 ;;
  esac
}

# stack_secrets_load NAME — export every stack secret for the stack NAME.
stack_secrets_load() {
  local name="${1:?stack_secrets_load needs the stack name}"
  local root="${STACK_SECRETS_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/.stack-secrets}"
  local file="${root}/${name}.env"
  command -v openssl >/dev/null 2>&1 || { echo "stack-secrets: openssl is required to generate the stack's secrets" >&2; return 1; }
  mkdir -p "$root"
  chmod 700 "$root" 2>/dev/null || true
  if [ ! -f "$file" ]; then
    local tmp="${file}.tmp.$$"
    (
      umask 077
      : > "$tmp"
      while read -r var kind; do
        [ -n "$var" ] || continue
        printf '%s=%s\n' "$var" "$(_stack_secret_value "$kind")" >> "$tmp"
      done < <(_stack_secret_specs)
    )
    mv "$tmp" "$file"
  fi
  # A file written before a variable was added gains it, without touching the
  # values it already holds.
  local var kind
  while read -r var kind; do
    [ -n "$var" ] || continue
    if ! grep -q "^${var}=" "$file"; then
      (umask 077; printf '%s=%s\n' "$var" "$(_stack_secret_value "$kind")" >> "$file")
    fi
  done < <(_stack_secret_specs)
  local line value
  while IFS= read -r line; do
    var="${line%%=*}"
    value="${line#*=}"
    [ -n "$var" ] || continue
    # The environment wins: export the file's value only when unset or empty.
    if [ -z "${!var:-}" ]; then
      export "${var}=${value}"
    else
      export "${var?}"
    fi
  done < "$file"
}

_stack_hex() { od -An -v -tx1 | tr -d ' \n'; }
# Hex text on stdin to bytes on stdout, without xxd (not on every runner).
_stack_unhex() { local h; h="$(tr -d ' \n')"; printf '%b' "$(printf '%s' "$h" | sed 's/../\\x&/g')"; }

# stack_fernet_encrypt KEY PLAINTEXT — print the Fernet token of PLAINTEXT
# under the Fernet key KEY (base64url, 32 bytes), with openssl only. The
# token is the one cryptography.fernet and elitea-main's vault read:
# 0x80 | timestamp | IV | AES-128-CBC(PKCS7) | HMAC-SHA256, base64url.
stack_fernet_encrypt() {
  local key_hex sign_hex enc_hex iv_hex ts_hex body_hex mac_hex
  key_hex="$(printf '%s' "$1" | tr -- '-_' '+/' | openssl base64 -d -A | _stack_hex)"
  [ "${#key_hex}" -eq 64 ] || { echo "stack-secrets: SECRETS_MASTER_KEY is not a 32-byte Fernet key" >&2; return 1; }
  sign_hex="${key_hex:0:32}"
  enc_hex="${key_hex:32:32}"
  iv_hex="$(openssl rand -hex 16)"
  ts_hex="$(printf '%016x' "$(date +%s)")"
  body_hex="80${ts_hex}${iv_hex}$(printf '%s' "$2" | openssl enc -aes-128-cbc -K "$enc_hex" -iv "$iv_hex" | _stack_hex)"
  mac_hex="$(printf '%s' "$body_hex" | _stack_unhex | openssl dgst -sha256 -mac HMAC -macopt "hexkey:${sign_hex}" -binary | _stack_hex)"
  printf '%s%s' "$body_hex" "$mac_hex" | _stack_unhex | openssl base64 -A | tr '+/' '-_'
}

# stack_wrap_seeded_vault_keys PSQL... — wrap every unwrapped project key in
# centry.secrets_key with SECRETS_MASTER_KEY.
#
# The E2E and kind seeds write fixed, non-secret vault keys in the clear,
# because psql cannot produce Fernet. A stack with SECRETS_MASTER_KEY reads
# every key row as a wrapped one (#2958 refuses the unwrapped opt-out next to
# a real sign-in configuration), so the seed calls this after its SQL. It
# wraps the key only; centry.secrets_data stays as it is, the same contract
# as deploy/scripts/rewrap-centry-vault.py. An unwrapped key is the 44-byte
# base64 text of 32 bytes; a wrapped one is a Fernet token, which is always
# longer. So a row that is already wrapped is left alone, and a second run
# changes nothing.
#
# PSQL is the command that runs psql against the stack's database; this
# function appends `-tAc <statement>`.
stack_wrap_seeded_vault_keys() {
  [ -n "${SECRETS_MASTER_KEY:-}" ] || return 0
  local rows hex plain token count=0
  rows="$("$@" -tAc "SELECT DISTINCT encode(data, 'hex') FROM centry.secrets_key WHERE length(data) = 44")"
  for hex in $rows; do
    plain="$(printf '%s' "$hex" | _stack_unhex)"
    token="$(stack_fernet_encrypt "$SECRETS_MASTER_KEY" "$plain")"
    "$@" -tAc "UPDATE centry.secrets_key SET data = convert_to('${token}', 'UTF8') WHERE data = decode('${hex}', 'hex')" >/dev/null
    count=$((count + 1))
  done
  echo "  ✓ wrapped ${count} seeded vault key value(s) with the stack's SECRETS_MASTER_KEY"
}
