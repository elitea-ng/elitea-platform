# Sourced by the postgres image entrypoint on first start. Creates the loader (write) and reader (read-only) roles.
# Passwords come from the compose environment (etlctl.py init writes them to the local, uncommitted env file) and
# reach SQL only as psql variables (:'name' quoting), never by string building.
# No `set -u`: the script is sourced into the image entrypoint, whose helpers expect unset positional args.
: "${ETL_PG_LOADER_PASSWORD:?}" "${ETL_PG_READER_PASSWORD:?}"
docker_process_sql -v loader_password="$ETL_PG_LOADER_PASSWORD" -v reader_password="$ETL_PG_READER_PASSWORD" <<'SQL'
CREATE ROLE etl_loader LOGIN PASSWORD :'loader_password';
CREATE ROLE etl_reader LOGIN PASSWORD :'reader_password';
SQL
