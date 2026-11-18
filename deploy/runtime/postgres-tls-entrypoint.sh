#!/bin/sh
# Start the stack's PostgreSQL with TLS on, for the sandbox supervisor.
#
# The supervisor connects to its receipt ledger with certificate AND hostname
# verification (VerifyFull) and has no plaintext mode. The stack's database is
# plain, so docker-compose.sandbox-postgres-tls.yml runs this instead of the
# image's entrypoint. Other clients are unchanged: the server still accepts
# plaintext connections from the stack's own services.
#
# PostgreSQL refuses a private key that is not owned by the server user with
# mode 0600 (or root-owned 0640). A bind mount arrives with the host's owner,
# so the pair is copied to a private directory owned by `postgres` first.
set -eu
# The arguments are the base file's `command`, which must be the postgres
# server (`postgres -c ...`). If a change to the base file turns it into
# anything else, appending `-c ssl=...` below would hand PostgreSQL flags to a
# program that does not know them, or silently run without TLS: stop instead.
[ "${1:-}" = postgres ] || {
  echo "ERROR: the container command must start with 'postgres', got '${1:-}'" >&2
  exit 1
}
src=/tls-src
dst=/var/lib/postgresql-tls
[ -r "$src/postgres.crt" ] && [ -r "$src/postgres.key" ] || {
  echo "ERROR: $src/postgres.crt and postgres.key are required" >&2
  exit 1
}
mkdir -p "$dst"
cp "$src/postgres.crt" "$dst/server.crt"
cp "$src/postgres.key" "$dst/server.key"
chown -R postgres:postgres "$dst"
chmod 700 "$dst"
chmod 600 "$dst/server.key"
chmod 644 "$dst/server.crt"
# docker-entrypoint.sh starts initdb/postgres as the postgres user and passes
# the arguments through; the base file's `command` (`postgres -c ...`) arrives
# as "$@", so TLS is appended to it.
exec docker-entrypoint.sh "$@" \
  -c ssl=on \
  -c ssl_cert_file="$dst/server.crt" \
  -c ssl_key_file="$dst/server.key"
