from __future__ import annotations

import pytest


@pytest.mark.skip(
    reason=(
        "the production index.ingest.v1 and #5681 cross-process seams exist, but "
        "this deterministic baseline harness has no reusable deployment fixture "
        "that binds its Confluence and LiteLLM endpoints through claimed execution, "
        "protected content and workload identity; do not replace that missing "
        "target comparison with a mocked adapter"
    )
)
def test_standalone_worker_matches_current_confluence_goldens() -> None:
    """Enable only after the real production composition accepts fixture endpoints."""


@pytest.mark.skip(
    reason=(
        "current SDK index_data is synchronous and exposes no cancellation "
        "input; baseline Stop is legacy task cancellation plus index-meta "
        "reconciliation, so a direct SDK harness cannot claim Stop parity "
        "without exercising the real orchestration boundary"
    )
)
def test_current_sdk_stop_reconciles_terminal_history_and_events() -> None:
    """Enable in the cross-process legacy-versus-worker orchestration profile."""
