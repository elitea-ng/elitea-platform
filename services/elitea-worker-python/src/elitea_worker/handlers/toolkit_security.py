"""``toolkit_security`` enforcement for ``toolkit.call_tool.v1``.

The producer (elitea-main) sends the platform guardrails policy in the run's
runtime context (``toolkit.call_tool.runtime_context``), together with the only
authority that may run a sensitive tool: ``sensitive_action_approval``. The
Rust worker refuses a blocked tool and a sensitive one on this path
(``services/elitea-worker-rust/src/toolkits/direct_execution.rs``,
``execute_toolsets``); this module is the same refusal for the Python worker,
so that a caller of ``toolkit.call_tool.v1`` other than the routes that check
first cannot run what the policy forbids.

The matching rules are the SDK's (``elitea_sdk/runtime/toolkits/security.py``)
as elitea-main's ``internal/domain/guardrails`` restates them: canonical keys
(lowercase, alphanumerics only), the tool-name alias reductions, a ``*``
toolkit key honoured for sensitive tools only. They are pure functions over the
document the run carries — the SDK's module-level configuration is never read
or written here, because it is process-wide and this policy is per run.
"""

from __future__ import annotations

import re
from typing import Any, Mapping

from elitea_worker.execution.errors import InvalidInput, UnsupportedCapability


APPROVAL_SOURCES = frozenset({"configuration_test", "user_confirmation"})
_WILDCARD = "*"
_NON_ALNUM = re.compile(r"[^a-z0-9]")


def canonical_key(value: str | None) -> str:
    return _NON_ALNUM.sub("", str(value or "").strip().lower())


def tool_name_aliases(tool_name: str | None) -> list[str]:
    current = str(tool_name or "").strip().lower()
    aliases: list[str] = []
    while current and current not in aliases:
        aliases.append(current)
        reduced = current
        if "___" in reduced:
            reduced = reduced.split("___", 1)[1].strip()
        if ":" in reduced:
            reduced = reduced.split(":", 1)[1].strip()
        if not reduced or reduced == current:
            break
        current = reduced
    return aliases


def _mapping(value: Any) -> dict[str, set[str]]:
    if value is None:
        return {}
    if not isinstance(value, Mapping):
        raise InvalidInput("The toolkit security policy is malformed.")
    mapping: dict[str, set[str]] = {}
    for toolkit, tools in value.items():
        if not isinstance(toolkit, str) or not isinstance(tools, list) or not all(isinstance(t, str) for t in tools):
            raise InvalidInput("The toolkit security policy is malformed.")
        key = _WILDCARD if toolkit.strip() == _WILDCARD else canonical_key(toolkit)
        if key:
            mapping[key] = {canonical_key(tool) for tool in tools if canonical_key(tool)}
    return mapping


def enforce_toolkit_security(
    *,
    toolkit_security: Mapping[str, Any] | None,
    approval: Mapping[str, Any] | None,
    toolkit_type: str,
    toolkit_name: str | None,
    tool_name: str,
) -> None:
    """Refuse a blocked tool, and a sensitive one without an approval.

    An ABSENT policy is refused rather than read as "nothing is blocked": the
    producer always sends one, so its absence is a producer this worker does
    not understand, and running unguarded is the failure this module exists to
    prevent.
    """

    if not isinstance(toolkit_security, Mapping):
        raise UnsupportedCapability("This tool run carries no toolkit security policy, so it was not run.")
    blocked_toolkits = toolkit_security.get("blocked_toolkits") or []
    if not isinstance(blocked_toolkits, list) or not all(isinstance(t, str) for t in blocked_toolkits):
        raise InvalidInput("The toolkit security policy is malformed.")
    blocked_tools = _mapping(toolkit_security.get("blocked_tools"))
    sensitive_tools = _mapping(toolkit_security.get("sensitive_tools"))

    toolkit_key = canonical_key(toolkit_type)
    candidates = {canonical_key(alias) for alias in tool_name_aliases(tool_name)} - {""}
    if toolkit_key and toolkit_key in {canonical_key(t) for t in blocked_toolkits}:
        raise UnsupportedCapability("The requested toolkit is blocked by policy.")
    if candidates & blocked_tools.get(toolkit_key, set()):
        raise UnsupportedCapability("The requested toolkit operation is blocked by policy.")

    identities = [key for key in (toolkit_key, canonical_key(toolkit_name)) if key]
    sensitive = any(candidates & sensitive_tools.get(key, set()) for key in identities) or bool(
        candidates & sensitive_tools.get(_WILDCARD, set())
    )
    if not sensitive:
        return
    if approval is None:
        raise UnsupportedCapability(
            "The requested toolkit operation is sensitive and was not approved for this run."
        )
    if not isinstance(approval, Mapping) or approval.get("source") not in APPROVAL_SOURCES:
        raise InvalidInput("The sensitive action approval is malformed.")
    if approval.get("source") == "user_confirmation" and not (
        isinstance(approval.get("approved_at"), str) and approval.get("approved_at")
    ):
        raise InvalidInput("The sensitive action approval is malformed.")
