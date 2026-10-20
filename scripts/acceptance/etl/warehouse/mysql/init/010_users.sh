# Sourced by the mysql image entrypoint on first start. Creates the loader (write) and reader (read-only) users,
# both REQUIRE SSL (the server also sets require_secure_transport). MySQL has no client-side variables for
# IDENTIFIED BY, so each password is checked against a strict alphabet before it is used.
# No `set -u`: the script is sourced into the image entrypoint, whose helpers expect unset positional args.
for etl_password in "${ETL_MYSQL_LOADER_PASSWORD:?}" "${ETL_MYSQL_READER_PASSWORD:?}"; do
  case "$etl_password" in
    *[!A-Za-z0-9_-]*) echo 'ETL MySQL passwords must use [A-Za-z0-9_-] only' >&2; exit 1 ;;
  esac
  if [ "${#etl_password}" -lt 24 ]; then echo 'ETL MySQL passwords must be at least 24 characters' >&2; exit 1; fi
done
unset etl_password
docker_process_sql --database=mysql <<SQL
CREATE USER 'etl_loader'@'%' IDENTIFIED BY '${ETL_MYSQL_LOADER_PASSWORD}' REQUIRE SSL;
CREATE USER 'etl_reader'@'%' IDENTIFIED BY '${ETL_MYSQL_READER_PASSWORD}' REQUIRE SSL;
GRANT SELECT, INSERT, UPDATE, DELETE ON warehouse.* TO 'etl_loader'@'%';
GRANT SELECT ON warehouse.* TO 'etl_reader'@'%';
SQL
