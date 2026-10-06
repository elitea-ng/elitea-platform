"""This worker's durable record of command deliveries it must not run again.

WHAT DECIDES IT NOW. A delivery that fails non-retryably is poison: the serve
loop naks it with the contract's 24h delay and writes one record to the
dead-letter bucket (`transport/nats_jetstream.py`). The 24h delay is held by
the server, so neither a restart nor another replica re-runs it inside that
window, and the bucket is the shared, alertable record. There is no other
shared tier any more.

WHY THIS FILE TIER STAYS. The nak delay ends: on the 26h agent and index
streams the poison is redelivered once more before MaxAge removes it. A worker
that refused it before recognises its key here (`<route>.<sha256(delivery
id)>`, the dead-letter key) and parks it again WITHOUT running it a second
time. The spool is already exclusively the worker's (0700, its own uid,
checked by `validate_private_directory`) and already a persistent volume.
A local-only record is correct; it is just not shared.
"""

from __future__ import annotations

import asyncio
from pathlib import Path

from elitea_worker.execution.errors import (
    InvalidInput,
    ResourceExhausted,
)


_MAX_ENTRY_ID_BYTES = 128
_MAX_RECORDED_ENTRIES = 4096
_FIELD_SEPARATOR = "\t"


class FileQuarantineStore:
    """Append-only, bounded record of entries that must not run again.

    One line per entry: `<entry_id>\\t<reason_code>`. The reason is written for
    the person who opens the file, not for the loader, which needs only the id.
    """

    def __init__(self, path: Path, *, cap: int = 256) -> None:
        if not 1 <= cap <= _MAX_RECORDED_ENTRIES:
            raise ValueError("quarantine cap exceeds the runtime bound")
        self._path = path
        self._cap = cap
        self._lock = asyncio.Lock()
        self._recorded: set[str] = set()
        self._loaded = False

    @property
    def cap(self) -> int:
        return self._cap

    @property
    def path(self) -> Path:
        return self._path

    async def load(self) -> frozenset[str]:
        """Read the record written by previous processes.

        A missing file is an empty record, not an error: the first run of a new
        deployment has nothing to adopt.

        A malformed line is SKIPPED rather than fatal, and counted into
        `malformed_lines` so the caller can report it. Refusing to start would
        turn one bad byte into an outage; skipping silently would resurrect the
        spin for that entry with nothing to show for it.
        """
        async with self._lock:
            entries, malformed = await asyncio.to_thread(self._read)
            self._recorded = set(entries)
            self._loaded = True
            self.malformed_lines = malformed
            return frozenset(self._recorded)

    async def add(self, entry_id: str, *, reason_code: str) -> bool:
        """Record one entry. False means the cap refused it, not that it failed."""
        _validate_entry_id(entry_id)
        async with self._lock:
            if entry_id in self._recorded:
                return True
            if len(self._recorded) >= self._cap:
                return False
            await asyncio.to_thread(self._append, entry_id, reason_code)
            self._recorded.add(entry_id)
            return True

    def _read(self) -> tuple[set[str], int]:
        try:
            raw = self._path.read_text(encoding="utf-8")
        except FileNotFoundError:
            return set(), 0
        entries: set[str] = set()
        malformed = 0
        for line in raw.splitlines():
            candidate = line.split(_FIELD_SEPARATOR, 1)[0].strip()
            if not candidate:
                continue
            try:
                _validate_entry_id(candidate)
            except InvalidInput:
                malformed += 1
                continue
            entries.add(candidate)
            if len(entries) > _MAX_RECORDED_ENTRIES:
                raise ResourceExhausted(
                    "The quarantine record exceeds the runtime bound."
                )
        return entries, malformed

    def _append(self, entry_id: str, reason_code: str) -> None:
        # Append and flush to the filesystem before returning. The whole point
        # is to survive a process that is about to be killed, so an entry that
        # is only in the page cache of this process is not recorded at all.
        self._path.parent.mkdir(parents=True, exist_ok=True)
        safe_reason = reason_code.replace(_FIELD_SEPARATOR, " ").replace("\n", " ")
        with self._path.open("a", encoding="utf-8") as handle:
            handle.write(f"{entry_id}{_FIELD_SEPARATOR}{safe_reason}\n")
            handle.flush()

    #: Set by `load`; read by the caller to report a damaged record.
    malformed_lines: int = 0


def _validate_entry_id(entry_id: str) -> None:
    if (
        not entry_id
        or len(entry_id.encode("utf-8")) > _MAX_ENTRY_ID_BYTES
        or not entry_id.isascii()
        or not entry_id.isprintable()
        # No whitespace. A dead-letter key never contains any, and accepting it
        # let a shell that did not word-split a list of ids record all four as
        # ONE entry — silently, because every character was printable ASCII.
        or any(character.isspace() for character in entry_id)
        or _FIELD_SEPARATOR in entry_id
    ):
        raise InvalidInput("The quarantine entry ID is malformed.")


__all__ = ["FileQuarantineStore"]
