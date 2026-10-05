"""Guards on the migration set. No database needed."""

from __future__ import annotations

import hashlib
import re
from pathlib import Path

import pytest

from elitea_deepwiki.storage import migrate


def test_migrations_are_discovered_in_order():
    migrations = migrate.discover()
    assert migrations, "no migrations found"
    versions = [item.version for item in migrations]
    assert versions == sorted(versions)
    assert versions[0] == "0001"


def test_every_migration_filename_is_numbered(tmp_path: Path):
    """A misnamed file is an error, not something to skip.

    Skipping would mean a typo in a migration filename reads as "there are no
    migrations here" — the failure shape that keeps recurring in this
    codebase, where absence is mistaken for correctness.
    """
    (tmp_path / "0001_fine.sql").write_text("SELECT 1;")
    (tmp_path / "oops.sql").write_text("SELECT 1;")
    with pytest.raises(migrate.MigrationError, match="does not match"):
        migrate.discover(tmp_path)


def test_duplicate_versions_are_rejected(tmp_path: Path):
    (tmp_path / "0001_one.sql").write_text("SELECT 1;")
    (tmp_path / "0001_two.sql").write_text("SELECT 2;")
    with pytest.raises(migrate.MigrationError, match="duplicate migration version"):
        migrate.discover(tmp_path)


def test_checksums_are_stable_for_a_given_text():
    migration = migrate.discover()[0]
    assert migration.checksum == hashlib.sha256(
        migration.path.read_text(encoding="utf-8").encode("utf-8")
    ).hexdigest()


def test_migration_sql_declares_the_text_search_configuration():
    """The one schema object the retrieval path cannot work without.

    `deepwiki_porter` is created by migration 0001 and named by the backend.
    A migration that stopped creating it would leave every FTS query failing
    at runtime rather than at deploy time.
    """
    from elitea_deepwiki.storage.postgres import TS_CONFIG

    sql = migrate.discover()[0].sql
    assert f"CREATE TEXT SEARCH CONFIGURATION {TS_CONFIG}" in sql


def test_the_unicode61_fold_is_identical_on_both_sides():
    """The write side and the read side must tokenise the same way.

    Migration 0001's generated column and `PostgresBackend.search_fts` both
    fold non-alphanumerics to spaces to reproduce FTS5's `unicode61`. If only
    one of them did, terms would be indexed that no query could name (or the
    reverse) and every FTS result would silently narrow.
    """
    sql = migrate.discover()[0].sql
    backend_source = (
        Path(migrate.__file__).resolve().parent / "postgres.py"
    ).read_text(encoding="utf-8")

    fold = r"'\[\^\[:alnum:\]\]\+', ' ', 'g'"
    assert re.search(fold, sql), "migration 0001 no longer folds non-alphanumerics"
    assert re.search(fold, backend_source), "search_fts no longer folds them"

    # Both query-side uses — the match and the lexeme extraction — need it.
    assert len(re.findall(fold, backend_source)) >= 2


def test_the_migrations_live_inside_the_package():
    """The published image must carry them, and it did not.

    ``MIGRATIONS_DIR`` used to be ``parents[3] / "migrations"``, which resolves
    to the service directory from a source checkout and to
    ``<site-packages>/../migrations`` from an installed one — a path that
    exists nowhere. Only files inside the package go into the wheel, so the
    image had no migrations at all and could not prepare its own database.

    Every test above passed the whole time, because every test runs from a
    checkout where the wrong path happens to resolve. This one does not: it
    asserts the directory is under the package root, which is false for the
    old computation in EITHER layout and true for the new one in both.
    """
    import elitea_deepwiki

    package_root = Path(elitea_deepwiki.__file__).resolve().parent
    assert migrate.MIGRATIONS_DIR.resolve().is_relative_to(package_root), (
        f"{migrate.MIGRATIONS_DIR} is outside the package at {package_root}, "
        "so it will not be installed and the image cannot migrate"
    )
    assert list(migrate.MIGRATIONS_DIR.glob("*.sql")), "the packaged directory is empty"


def test_the_wheel_configuration_ships_the_package_directory():
    """A companion to the assertion above, on the other side of the build.

    The path can be correct while packaging excludes the files anyway. This
    reads pyproject's wheel target and checks that the package directory —
    which now contains the migrations — is what ships.
    """
    import tomllib

    pyproject = Path(__file__).resolve().parents[2] / "pyproject.toml"
    config = tomllib.loads(pyproject.read_text())
    packages = config["tool"]["hatch"]["build"]["targets"]["wheel"]["packages"]
    assert "src/elitea_deepwiki" in packages, (
        f"the wheel ships {packages}, which does not include the package the "
        "migrations now live in"
    )


def test_the_test_reset_drops_what_the_migrations_create():
    """The PostgreSQL fixture's clean slate must cover every migration.

    A schema the reset leaves behind can hold objects that depend on ones it
    drops: 0003's staging `fts` column depends on `deepwiki_porter`, and a
    reset that dropped the configuration without the schema failed on every
    run after the first.
    """
    from .conftest import RESET_STATEMENTS

    sql = "\n".join(m.sql for m in migrate.discover())
    reset = [statement.lower() for statement in RESET_STATEMENTS]

    def position(fragment: str) -> int:
        for index, statement in enumerate(reset):
            if fragment in statement:
                return index
        raise AssertionError(f"the test reset does not drop {fragment!r}")

    schemas = re.findall(r"CREATE SCHEMA IF NOT EXISTS (\w+)", sql)
    assert schemas, "no migration creates a schema any more; update this test"
    configuration = position("drop text search configuration if exists deepwiki_porter")
    for schema in schemas:
        assert position(f"drop schema if exists {schema} cascade") < configuration

    # Every unqualified table is dropped by name.
    tables = re.findall(r"CREATE (?:UNLOGGED )?TABLE IF NOT EXISTS (\w+)\s", sql)
    dropped = next(s for s in reset if s.startswith("drop table"))
    for table in tables:
        assert re.search(rf"\b{table}\b", dropped), f"the test reset keeps {table}"
