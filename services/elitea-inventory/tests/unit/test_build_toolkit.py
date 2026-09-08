"""`build_toolkit` must hand the SDK the shape its toolkits actually read.

The SDK subscripts the toolkit dict directly — `str(tool['toolkit_name'])` in
github's `get_toolkit`, `collection_name=tool['toolkit_name']` in ado_repos',
and `tool['settings']['active_branch']` in github's — so a missing key is a
KeyError raised from inside the toolkit, before any credential is looked at
and long before anything reaches a repository. MEASURED against the SDK this
package pins, on the shape `build_toolkit` used to send:

    github    -> KeyError: 'active_branch'
    ado_repos -> KeyError: 'toolkit_name'

That is what these pin. The first test asserts the dict, which is the contract
`build_toolkit` owns; the second drives the real SDK, which is what proves the
contract was read correctly rather than guessed.
"""

from __future__ import annotations

import pytest

from elitea_inventory.sources import Source, build_toolkit

GITHUB_SETTINGS = {
    "repository": "acme/service",
    "github_configuration": {"access_token": "not-a-real-token"},
}
ADO_SETTINGS = {
    "project": "proj",
    "repository_id": "repo",
    "ado_configuration": {
        "organization_url": "https://dev.azure.com/acme",
        "token": "not-a-real-token",
    },
}


def _capture(monkeypatch):
    """Intercept the SDK call and return the dict it was handed."""
    seen: dict = {}

    def fake_instantiate(toolkit_data):
        seen.update(toolkit_data)
        raise _Stop

    import elitea_sdk.tools

    monkeypatch.setattr(elitea_sdk.tools, "instantiate_toolkit", fake_instantiate)
    return seen


class _Stop(Exception):
    """Ends the call once the dict has been captured."""


def test_the_toolkit_dict_carries_the_name_and_branches_the_sdk_reads(monkeypatch):
    seen = _capture(monkeypatch)
    source = Source(
        toolkit_id=42, type="github", name="acme-repo", settings=GITHUB_SETTINGS, branch="release/2"
    )

    with pytest.raises(_Stop):
        build_toolkit(source)

    # The key the SDK reads the name from. `name` alone is not it.
    assert seen["toolkit_name"] == "acme-repo"
    # github requires both, with no default of its own.
    assert seen["settings"]["active_branch"] == "release/2"
    assert seen["settings"]["base_branch"] == "release/2"
    # Credentials are passed through untouched, never rebuilt.
    assert seen["settings"]["github_configuration"] == GITHUB_SETTINGS["github_configuration"]


def test_a_stored_base_branch_survives_the_requested_branch(monkeypatch):
    """`active_branch` is what we read; `base_branch` is the repository's own."""
    seen = _capture(monkeypatch)
    source = Source(
        toolkit_id=42,
        type="github",
        name="acme-repo",
        settings={**GITHUB_SETTINGS, "base_branch": "main"},
        branch="feature/x",
    )

    with pytest.raises(_Stop):
        build_toolkit(source)

    assert seen["settings"]["active_branch"] == "feature/x"
    assert seen["settings"]["base_branch"] == "main"


def test_no_branch_anywhere_falls_back_rather_than_raising(monkeypatch):
    seen = _capture(monkeypatch)
    source = Source(toolkit_id=42, type="github", name="acme-repo", settings=GITHUB_SETTINGS)

    with pytest.raises(_Stop):
        build_toolkit(source)

    assert seen["settings"]["active_branch"] == "main"


def test_a_missing_credential_block_is_named_not_left_to_the_sdk():
    """The SDK would raise KeyError from inside the toolkit; say it here instead."""
    source = Source(toolkit_id=42, type="github", name="acme-repo", settings={"repository": "a/b"})

    with pytest.raises(ValueError, match="github_configuration"):
        build_toolkit(source)


@pytest.mark.parametrize(
    ("source_type", "settings"), [("github", GITHUB_SETTINGS), ("ado_repos", ADO_SETTINGS)]
)
def test_the_real_sdk_gets_past_the_dict_shape(source_type, settings):
    """Drive the actual SDK: whatever happens next, it is not a KeyError.

    github constructs offline, so it returns a wrapper. ado_repos calls Azure
    at construction and the fake token is refused — a refusal FROM AZURE is the
    whole dependency and credential path proved, which is the point; only a
    KeyError would mean the dict shape is still wrong.
    """
    pytest.importorskip("elitea_sdk.tools")
    source = Source(toolkit_id=7, type=source_type, name="src", settings=settings, branch="main")

    try:
        wrapper = build_toolkit(source)
    except KeyError as exc:  # pragma: no cover - the defect this file exists for
        pytest.fail(f"the toolkit dict is still missing {exc}")
    except Exception as exc:  # noqa: BLE001 - any non-KeyError is past the shape
        assert "KeyError" not in repr(exc)
    else:
        assert wrapper is not None
