"""Bounded codec for the current UI-facing NodeEvent JSON contract."""

from __future__ import annotations

import json
import re
from datetime import datetime
from typing import Any

from elitea.runtime.v1 import node_event_pb2


# Keep enough headroom for the containing ExecutionOutputFrameV1 identities,
# fence and digest beneath the fixed 64 KiB gRPC request ceiling. The browser
# JSON limit is intentionally larger than the old 48 KiB value because this is
# data-plane output, while the signed bus command remains independently
# bounded at 32 KiB.
MAX_CURRENT_NODE_EVENT_JSON_BYTES = 60 * 1024
_MAX_SAFE_STRING_BYTES = 256
_MAX_JSON_NESTING = 64
_MAX_EVENT_TYPE_BYTES = 128

_CURRENT_FIELDS = frozenset(
    {
        "type",
        "stream_id",
        "message_id",
        "question_id",
        "content",
        "thinking",
        "response_metadata",
        "references",
        "sio_event",
        "created_at",
        "parent_message_id",
        "agent_name",
        "execution_generation",
    }
)
_EVENT_TYPE = re.compile(r"^[A-Za-z][A-Za-z0-9_]{0,127}$")
_RFC3339 = re.compile(
    r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$"
)


class InvalidCurrentNodeEvent(ValueError):
    """The event cannot cross the bounded NodeEvent v1 data-plane contract."""


def decode_current_node_event_json(raw: bytes) -> node_event_pb2.NodeEventV1:
    """Map one current NodeEvent JSON object to protobuf without float coercion."""

    data = _load_json(raw)
    if not isinstance(data, dict) or set(data).difference(_CURRENT_FIELDS):
        raise InvalidCurrentNodeEvent("The current node event is malformed.")

    event_type = data.get("type")
    if not isinstance(event_type, str) or not _valid_event_type(event_type):
        raise InvalidCurrentNodeEvent("The current node event is malformed.")

    optional_values = {
        "stream_id": _optional_safe_string(data.get("stream_id")),
        "message_id": _optional_safe_string(data.get("message_id")),
        "question_id": _optional_safe_string(data.get("question_id")),
        "thinking": _optional_string(
            data.get("thinking"), MAX_CURRENT_NODE_EVENT_JSON_BYTES
        ),
        "sio_event": _optional_safe_string(data.get("sio_event")),
        "created_at": _optional_timestamp(data.get("created_at")),
        "parent_message_id": _optional_safe_string(data.get("parent_message_id")),
        "agent_name": _optional_safe_string(data.get("agent_name")),
        "execution_generation": _optional_safe_string(
            data.get("execution_generation")
        ),
    }

    response_metadata = data.get("response_metadata", {})
    if response_metadata is not None and not isinstance(response_metadata, dict):
        raise InvalidCurrentNodeEvent("The current node event is malformed.")
    references = data.get("references", [])
    if references is not None and not isinstance(references, list):
        raise InvalidCurrentNodeEvent("The current node event is malformed.")

    fields: dict[str, Any] = {
        "type": event_type,
        "content": _dump_json(data.get("content")),
        "response_metadata": _dump_json(response_metadata),
        "references": _dump_json(references),
    }
    fields.update({name: value for name, value in optional_values.items() if value is not None})
    event = node_event_pb2.NodeEventV1(**fields)
    # The complete protobuf frame has additional identity/fence overhead; keep
    # this payload strictly below the selected 64 KiB output-frame ceiling.
    if event.ByteSize() >= 64 * 1024:
        raise InvalidCurrentNodeEvent("The current node event is malformed.")
    return event


def encode_current_node_event_json(event: node_event_pb2.NodeEventV1) -> bytes:
    """Return all thirteen established NodeEvent fields for SSE projection."""

    if not isinstance(event, node_event_pb2.NodeEventV1) or not _valid_event_type(
        event.type
    ):
        raise InvalidCurrentNodeEvent("The current node event is malformed.")

    optional_values = {
        "stream_id": _protobuf_optional_string(event, "stream_id"),
        "message_id": _protobuf_optional_string(event, "message_id"),
        "question_id": _protobuf_optional_string(event, "question_id"),
        "thinking": _protobuf_optional_string(
            event, "thinking", max_bytes=MAX_CURRENT_NODE_EVENT_JSON_BYTES, safe=False
        ),
        "sio_event": _protobuf_optional_string(event, "sio_event"),
        "created_at": _protobuf_optional_timestamp(event),
        "parent_message_id": _protobuf_optional_string(event, "parent_message_id"),
        "agent_name": _protobuf_optional_string(event, "agent_name"),
        "execution_generation": _protobuf_optional_string(
            event, "execution_generation"
        ),
    }
    content = _load_fragment(event.content or b"null")
    response_metadata = _load_fragment(event.response_metadata or b"{}")
    if response_metadata is not None and not isinstance(response_metadata, dict):
        raise InvalidCurrentNodeEvent("The current node event is malformed.")
    references = _load_fragment(event.references or b"[]")
    if references is not None and not isinstance(references, list):
        raise InvalidCurrentNodeEvent("The current node event is malformed.")

    data = {
        "type": event.type,
        "stream_id": optional_values["stream_id"],
        "message_id": optional_values["message_id"],
        "question_id": optional_values["question_id"],
        "content": content,
        "thinking": optional_values["thinking"],
        "response_metadata": response_metadata,
        "references": references,
        "sio_event": optional_values["sio_event"],
        "created_at": optional_values["created_at"],
        "parent_message_id": optional_values["parent_message_id"],
        "agent_name": optional_values["agent_name"],
        "execution_generation": optional_values["execution_generation"],
    }
    return _dump_json(data)


def _load_json(raw: bytes) -> Any:
    if (
        not isinstance(raw, bytes)
        or not raw
        or len(raw) > MAX_CURRENT_NODE_EVENT_JSON_BYTES
        or not _valid_json_nesting(raw)
    ):
        raise InvalidCurrentNodeEvent("The current node event is malformed.")
    try:
        text = raw.decode("utf-8", errors="strict")
        return json.loads(
            text,
            object_pairs_hook=_unique_object,
            parse_constant=_reject_json_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError, TypeError, ValueError) as exc:
        if isinstance(exc, InvalidCurrentNodeEvent):
            raise
        raise InvalidCurrentNodeEvent("The current node event is malformed.") from exc


def _load_fragment(raw: bytes) -> Any:
    return _load_json(raw)


def _dump_json(value: Any) -> bytes:
    try:
        raw = json.dumps(
            value,
            ensure_ascii=False,
            allow_nan=False,
            separators=(",", ":"),
        ).encode("utf-8")
    except (TypeError, ValueError, UnicodeEncodeError, RecursionError) as exc:
        raise InvalidCurrentNodeEvent("The current node event is malformed.") from exc
    # Go's encoding/json is the authoritative serializer for the durable SSE
    # projection.  Match its default string escaping byte-for-byte so the
    # worker's terminal artifact digest is calculated over the exact bytes
    # elitea-main persists.  These replacements are safe over the complete
    # JSON document because none of the characters is JSON syntax.
    raw = (
        raw.replace(b"&", b"\\u0026")
        .replace(b"<", b"\\u003c")
        .replace(b">", b"\\u003e")
        .replace("\u2028".encode(), b"\\u2028")
        .replace("\u2029".encode(), b"\\u2029")
    )
    if len(raw) > MAX_CURRENT_NODE_EVENT_JSON_BYTES or not _valid_json_nesting(raw):
        raise InvalidCurrentNodeEvent("The current node event is malformed.")
    return raw


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise InvalidCurrentNodeEvent("The current node event is malformed.")
        result[key] = value
    return result


def _reject_json_constant(_: str) -> None:
    raise InvalidCurrentNodeEvent("The current node event is malformed.")


def _valid_json_nesting(raw: bytes) -> bool:
    depth = 0
    in_string = False
    escaped = False
    for character in raw:
        if in_string:
            if escaped:
                escaped = False
            elif character == ord("\\"):
                escaped = True
            elif character == ord('"'):
                in_string = False
            continue
        if character == ord('"'):
            in_string = True
        elif character in (ord("{"), ord("[")):
            depth += 1
            if depth > _MAX_JSON_NESTING:
                return False
        elif character in (ord("}"), ord("]")):
            depth -= 1
            if depth < 0:
                return False
    return depth == 0 and not in_string and not escaped


def _valid_event_type(value: str) -> bool:
    return (
        bool(value)
        and len(value.encode("utf-8")) <= _MAX_EVENT_TYPE_BYTES
        and _EVENT_TYPE.fullmatch(value) is not None
    )


def _optional_safe_string(value: Any) -> str | None:
    result = _optional_string(value, _MAX_SAFE_STRING_BYTES)
    if result is not None and any(character in result for character in ("\r", "\n", "\x00")):
        raise InvalidCurrentNodeEvent("The current node event is malformed.")
    return result


def _optional_string(value: Any, max_bytes: int) -> str | None:
    if value is None:
        return None
    if not isinstance(value, str) or len(value.encode("utf-8")) > max_bytes:
        raise InvalidCurrentNodeEvent("The current node event is malformed.")
    return value


def _optional_timestamp(value: Any) -> str | None:
    result = _optional_safe_string(value)
    if result is None:
        return None
    if not _valid_timestamp(result):
        raise InvalidCurrentNodeEvent("The current node event is malformed.")
    return result


def _valid_timestamp(value: str) -> bool:
    if len(value.encode("utf-8")) > 64 or _RFC3339.fullmatch(value) is None:
        return False
    try:
        parsed = datetime.fromisoformat(value[:-1] + "+00:00" if value.endswith("Z") else value)
    except ValueError:
        return False
    return parsed.utcoffset() is not None


def _protobuf_optional_string(
    event: node_event_pb2.NodeEventV1,
    field: str,
    *,
    max_bytes: int = _MAX_SAFE_STRING_BYTES,
    safe: bool = True,
) -> str | None:
    if not event.HasField(field):
        return None
    value = _optional_string(getattr(event, field), max_bytes)
    if safe and value is not None and any(
        character in value for character in ("\r", "\n", "\x00")
    ):
        raise InvalidCurrentNodeEvent("The current node event is malformed.")
    return value


def _protobuf_optional_timestamp(event: node_event_pb2.NodeEventV1) -> str | None:
    value = _protobuf_optional_string(event, "created_at")
    if value is not None and not _valid_timestamp(value):
        raise InvalidCurrentNodeEvent("The current node event is malformed.")
    return value
