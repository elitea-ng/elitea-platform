#!/usr/bin/env python3
"""Change the wrapping of centry.secrets_key rows. Issue #418.

WHAT A ROW HOLDS. centry.secrets_key holds one project key per vault id.
centry.secrets_data holds that vault's JSON, Fernet-encrypted with the project
key. When a service has SECRETS_MASTER_KEY it stores the project key
Fernet-encrypted with that master key; when it has no master key it stores the
project key verbatim. The two forms live in the same column and look alike.

WHAT THIS SCRIPT CHANGES. Only the wrapping of the project key. The project key
itself does not change, so centry.secrets_data is never rewritten and no secret
value is re-encrypted. That is what makes the conversion safe to repeat and
cheap to verify.

WHEN YOU NEED IT. A deployment that ran elitea-main without SECRETS_MASTER_KEY
has unwrapped rows. So does one that ran pylon-indexer without it, before #339
deleted that service. Supplying the key to the stack makes
those rows unreadable until they are wrapped. Rotating an exposed key has the
same shape, with --from-key set to the old value.

  # look first; this writes nothing
  ./rewrap-centry-vault.py --database-url "$URL" --to-key "$NEW"

  # unwrapped rows -> wrapped with a new key
  ./rewrap-centry-vault.py --database-url "$URL" --to-key "$NEW" --apply

  # rotation: old key -> new key
  ./rewrap-centry-vault.py --database-url "$URL" \
      --from-key "$OLD" --to-key "$NEW" --apply

  # wrapped rows -> unwrapped
  ./rewrap-centry-vault.py --database-url "$URL" --from-key "$OLD" --apply

RUN IT ON A COPY FIRST. Take a dump, load it into another database, convert
that, and read a secret back through the application. This script has no undo.

The script needs `cryptography` and `psycopg2`. The elitea-worker-python image
carries both, and it is in every stack that has this table:

  podman run --rm --network <net> \
    -v "$PWD/deploy/scripts:/scripts:ro" ghcr.io/elitea-ng/elitea-worker-python:dev \
    python /scripts/rewrap-centry-vault.py --help

It never prints a key or a secret value.
"""

from __future__ import annotations

import argparse
import base64
import sys

try:
    import psycopg2
except ImportError:  # pragma: no cover - depends on the host
    sys.exit("rewrap-centry-vault: psycopg2 is not installed.")

try:
    from cryptography.fernet import Fernet, InvalidToken
except ImportError:  # pragma: no cover - depends on the host
    sys.exit("rewrap-centry-vault: cryptography is not installed.")


def check_key(label: str, value: str) -> bytes:
    """Apply Fernet's own rule: URL-safe base64 of exactly 32 bytes."""
    try:
        raw = base64.urlsafe_b64decode(value)
    except Exception:
        sys.exit(f"rewrap-centry-vault: {label} is not URL-safe base64.")
    if len(raw) != 32:
        sys.exit(
            f"rewrap-centry-vault: {label} decodes to {len(raw)} bytes; "
            "Fernet needs 32."
        )
    return value.encode()


def unwrap(row: bytes, master: bytes | None) -> bytes:
    """Return the project key held by one secrets_key row."""
    if master is None:
        return row
    return Fernet(master).decrypt(row)


def wrap(project_key: bytes, master: bytes | None) -> bytes:
    """Return the row bytes that store project_key under master."""
    if master is None:
        return project_key
    return Fernet(master).encrypt(project_key)


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Change the wrapping of centry.secrets_key rows.",
    )
    parser.add_argument("--database-url", required=True)
    parser.add_argument(
        "--from-key",
        default=None,
        help="the master key the rows are wrapped with now; omit if they are "
        "unwrapped",
    )
    parser.add_argument(
        "--to-key",
        default=None,
        help="the master key to wrap the rows with; omit to leave them "
        "unwrapped",
    )
    parser.add_argument(
        "--apply",
        action="store_true",
        help="write the converted rows; without it the script only reports",
    )
    args = parser.parse_args()

    from_key = check_key("--from-key", args.from_key) if args.from_key else None
    to_key = check_key("--to-key", args.to_key) if args.to_key else None

    if from_key == to_key:
        sys.exit("rewrap-centry-vault: --from-key and --to-key are the same.")

    conn = psycopg2.connect(args.database_url)
    conn.autocommit = False
    cur = conn.cursor()

    cur.execute("SELECT id, data FROM centry.secrets_key ORDER BY id")
    key_rows = cur.fetchall()
    cur.execute("SELECT id, data FROM centry.secrets_data")
    data_rows = dict(cur.fetchall())

    if not key_rows:
        print("rewrap-centry-vault: centry.secrets_key holds no rows.")
        return 0

    converted: list[tuple[str, bytes]] = []
    failed: list[str] = []

    for vault_id, row in key_rows:
        row = bytes(row)
        try:
            project_key = unwrap(row, from_key)
        except InvalidToken:
            failed.append(f"{vault_id}: will not open with --from-key")
            continue
        except Exception as exc:
            failed.append(f"{vault_id}: {type(exc).__name__}")
            continue

        # The premise check. A wrong key can still decode, and a row of the
        # wrong FORM can still look like bytes. Only the data row proves the
        # project key is the real one, so a vault that has a data row must
        # decrypt it before its key row is rewritten.
        blob = data_rows.get(vault_id)
        if blob is not None:
            try:
                Fernet(project_key).decrypt(bytes(blob))
            except Exception:
                failed.append(f"{vault_id}: the project key does not open its data row")
                continue
        else:
            print(f"  {vault_id}: no data row; the key row is converted unchecked")

        converted.append((vault_id, wrap(project_key, to_key)))

    src = "wrapped" if from_key else "unwrapped"
    dst = "wrapped" if to_key else "unwrapped"
    print(f"rewrap-centry-vault: {len(key_rows)} rows, {src} -> {dst}")
    print(f"  convertible: {len(converted)}")
    for line in failed:
        print(f"  SKIPPED {line}")

    if failed:
        print(
            "rewrap-centry-vault: some rows did not open. Nothing was written. "
            "Check --from-key, or convert the readable rows from a database "
            "that holds only those."
        )
        conn.rollback()
        return 1

    if not args.apply:
        print("rewrap-centry-vault: dry run. Add --apply to write.")
        conn.rollback()
        return 0

    for vault_id, new_row in converted:
        cur.execute(
            "UPDATE centry.secrets_key SET data = %s WHERE id = %s",
            (new_row, vault_id),
        )
    conn.commit()

    # Read back what was written, with the key the services will use. A commit
    # is not evidence: the point of the conversion is that the rows OPEN
    # afterwards.
    cur.execute("SELECT id, data FROM centry.secrets_key ORDER BY id")
    for vault_id, row in cur.fetchall():
        project_key = unwrap(bytes(row), to_key)
        blob = data_rows.get(vault_id)
        if blob is not None:
            Fernet(project_key).decrypt(bytes(blob))
    print(f"rewrap-centry-vault: {len(converted)} rows written and read back.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
