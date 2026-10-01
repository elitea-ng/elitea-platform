"""Claim-resolved MCP authority reaches the pinned SDK without local lookup."""

from __future__ import annotations

from types import SimpleNamespace

import pytest
from langchain_core.tools import StructuredTool

from elitea_worker.agents import sdk_adapter
from elitea_worker.execution.errors import DependencyUnavailable


@pytest.mark.parametrize("kind", ["mcp_config", "mcp_elitea_internal_applications", "mcp_prebuilt_fixture"])
def test_pinned_sdk_uses_materialized_http_authority_without_local_configuration(monkeypatch, kind):
    sdk_tools = sdk_adapter.importlib.import_module("elitea_sdk.runtime.toolkits.tools")
    sdk_config = sdk_adapter.importlib.import_module("elitea_sdk.runtime.toolkits.mcp_config")
    sdk_mcp = sdk_adapter.importlib.import_module("elitea_sdk.runtime.toolkits.mcp")
    calls = []

    def local_lookup(*_args, **_kwargs):
        raise AssertionError("Claim materialization must not reread local MCP configuration.")

    def http_toolkit(**kwargs):
        calls.append(kwargs)
        tools = [StructuredTool.from_function(
            lambda: "fixture", name=name, description="Return a test fixture.",
        ) for name in ("read_fixture", "write_fixture")]
        return SimpleNamespace(get_tools=lambda: tools)

    monkeypatch.setattr(sdk_config, "get_mcp_server_config", local_lookup)
    monkeypatch.setattr(sdk_config, "get_all_mcp_server_configs", local_lookup)
    monkeypatch.setattr(sdk_tools, "get_mcp_server_config", local_lookup)
    monkeypatch.setattr(sdk_mcp.McpToolkit, "get_toolkit", http_toolkit)
    monkeypatch.setattr(sdk_tools, "is_toolkit_blocked", lambda _kind: False)
    source = {
        "id": 52,
        "type": kind,
        "name": "Applications",
        "toolkit_name": "Applications",
        "settings": {
            "server_name": "fixture_server" if kind == "mcp_config" else kind,
            "url": "https://main.example.test/app/7/mcp/elitea_core/applications",
            "headers": {"Authorization": "Bearer TEST_ONLY_CLAIM_ACTOR"},
            "timeout": 300,
            "ssl_verify": True,
            "selected_tools": ["read_fixture"],
            "excluded_tools": ["write_fixture"],
        },
        "meta": {"mcp": True, "internal_builder": True},
    }
    adapted = sdk_adapter._sdk_agent_tools([source])

    served = sdk_tools.get_tools(adapted)

    assert [tool.name for tool in served] == ["read_fixture"]
    assert len(calls) == 1
    call = calls[0]
    assert call["url"] == source["settings"]["url"]
    assert call["headers"] == source["settings"]["headers"]
    assert call["timeout"] == 300
    assert call["ssl_verify"] is True
    assert call["selected_tools"] == ["read_fixture"]
    assert call["toolkit_name"] == "Applications"
    assert call["toolkit_type"] == ("mcp_fixture_server" if kind == "mcp_config" else kind)
    assert adapted[0]["id"] == 52
    assert adapted[0]["type"] == kind
    assert adapted[0]["meta"] == source["meta"]
    assert "server_config" not in source["settings"]


@pytest.mark.parametrize("settings", [{}, {"url": ""}, {"url": None}, {"url": 7}, None])
def test_unresolved_prebuilt_mcp_cannot_fall_back_to_local_configuration(settings):
    with pytest.raises(DependencyUnavailable, match="materialized MCP endpoint"):
        sdk_adapter._sdk_agent_tools([{"type": "mcp_elitea_internal_applications", "settings": settings}])
