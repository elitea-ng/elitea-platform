"""The durable half of the quarantine: it must outlive the process."""

from __future__ import annotations

import asyncio

import pytest

from elitea_worker.execution.errors import InvalidInput
from elitea_worker.execution.quarantine import FileQuarantineStore
from elitea_worker.transport.nats_jetstream import dead_letter_key


def test_a_recorded_entry_survives_a_new_store_over_the_same_file(tmp_path) -> None:
    """The whole point: a SECOND store instance sees the first one's decision.

    Two instances over one path is what a restart is. Asserting only that `add`
    returned True would pass even if nothing were written.
    """

    async def run() -> None:
        path = tmp_path / "quarantine.v1"
        first = FileQuarantineStore(path)
        assert await first.load() == frozenset()
        assert await first.add("1-0", reason_code="AUTHORIZATION_FAILED") is True

        # A new object with no shared memory — the file is the only channel.
        second = FileQuarantineStore(path)
        assert await second.load() == frozenset({"1-0"})

    asyncio.run(run())


def test_the_reason_is_written_for_a_reader_but_not_needed_to_load(tmp_path) -> None:
    async def run() -> None:
        path = tmp_path / "quarantine.v1"
        store = FileQuarantineStore(path)
        await store.add("2-0", reason_code="AUTHORIZATION_FAILED")

        written = path.read_text(encoding="utf-8").strip()
        assert written == "2-0\tAUTHORIZATION_FAILED"
        assert await FileQuarantineStore(path).load() == frozenset({"2-0"})

    asyncio.run(run())


def test_recording_the_same_entry_twice_is_idempotent(tmp_path) -> None:
    async def run() -> None:
        path = tmp_path / "quarantine.v1"
        store = FileQuarantineStore(path)
        assert await store.add("3-0", reason_code="X") is True
        assert await store.add("3-0", reason_code="X") is True
        assert len(path.read_text(encoding="utf-8").splitlines()) == 1

    asyncio.run(run())


def test_the_cap_refuses_rather_than_growing(tmp_path) -> None:
    """Past the cap `add` answers False, so the caller can say so."""

    async def run() -> None:
        store = FileQuarantineStore(tmp_path / "quarantine.v1", cap=2)
        assert await store.add("1-0", reason_code="X") is True
        assert await store.add("2-0", reason_code="X") is True
        assert await store.add("3-0", reason_code="X") is False
        # An already-recorded entry is still accepted at the cap.
        assert await store.add("1-0", reason_code="X") is True

    asyncio.run(run())


def test_a_missing_file_is_an_empty_record(tmp_path) -> None:
    async def run() -> None:
        store = FileQuarantineStore(tmp_path / "nothing-here" / "quarantine.v1")
        assert await store.load() == frozenset()

    asyncio.run(run())


def test_a_damaged_line_is_skipped_counted_and_the_rest_still_load(tmp_path) -> None:
    """One bad byte must not cost the whole record, nor pass unmentioned."""

    async def run() -> None:
        path = tmp_path / "quarantine.v1"
        path.write_text(
            "1-0\tAUTHORIZATION_FAILED\n"
            "\n"
            f"{'x' * 200}\tTOO_LONG\n"
            "2-0\tAUTHORIZATION_FAILED\n",
            encoding="utf-8",
        )
        store = FileQuarantineStore(path)
        assert await store.load() == frozenset({"1-0", "2-0"})
        assert store.malformed_lines == 1

    asyncio.run(run())


@pytest.mark.parametrize("entry_id", ["", "a\tb", "x" * 200, "a\nb", "1-0 2-0", "1-0\r"])
def test_a_malformed_entry_id_is_refused_on_write(tmp_path, entry_id: str) -> None:
    async def run() -> None:
        store = FileQuarantineStore(tmp_path / "quarantine.v1")
        with pytest.raises(InvalidInput):
            await store.add(entry_id, reason_code="X")

    asyncio.run(run())


def test_a_reason_cannot_forge_a_second_field(tmp_path) -> None:
    """The reason is untrusted for FORMAT purposes: it must not inject a field
    separator or a newline that would turn one record into two."""

    async def run() -> None:
        path = tmp_path / "quarantine.v1"
        store = FileQuarantineStore(path)
        await store.add("4-0", reason_code="A\tB\nC")

        lines = path.read_text(encoding="utf-8").splitlines()
        assert len(lines) == 1
        assert lines[0].split("\t")[0] == "4-0"
        assert await FileQuarantineStore(path).load() == frozenset({"4-0"})

    asyncio.run(run())


def test_a_dead_letter_key_is_a_valid_record_entry(tmp_path) -> None:
    """The record is keyed by the dead-letter key, <route>.<sha256>."""

    async def run() -> None:
        path = tmp_path / "quarantine.v2"
        key = dead_letter_key("ELITEA_RT_V1_AGENT", "outbox-1")
        assert await FileQuarantineStore(path).add(key, reason_code="INVALID_INPUT")
        assert await FileQuarantineStore(path).load() == frozenset({key})

    asyncio.run(run())
