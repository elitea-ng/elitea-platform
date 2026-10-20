#!/bin/sh
# Copies the test-CA server material with the ownership and mode the server insists on, and the init scripts into
# the container's own filesystem (Docker Desktop bind mounts report every file as executable, so the image entrypoint
# would execute the .sh files instead of sourcing them). Then runs the image entrypoint.
set -eu
install -d -m 0755 /run/etl-tls
install -o postgres -g postgres -m 0600 /etl/tls/warehouse-pg.key /run/etl-tls/server.key
install -o postgres -g postgres -m 0644 /etl/tls/warehouse-pg.crt /run/etl-tls/server.crt
install -o postgres -g postgres -m 0644 /etl/tls/ca.crt /run/etl-tls/ca.crt
install -d -m 0755 /docker-entrypoint-initdb.d
for f in /etl/warehouse/pg/init/*; do install -m 0644 "$f" /docker-entrypoint-initdb.d/; done
exec docker-entrypoint.sh "$@"
