"""Credential-free async client for the fixed Code bridge. No HTTP capability."""
import asyncio
import json
import math
import re
import struct

MAX_REQUEST_HEADER = 262144
MAX_REPLY_HEADER = 2097152
MAX_CHUNK = 65536
MAX_VALUES = 10000
MAX_DEPTH = 32
MAX_OBJECT_BYTES = 8388608
_UNSET = object()
_OPERATIONS = frozenset((
    "application_list", "application_get", "application_version_get", "user_get",
    "toolkit_list", "toolkit_call", "secret_read", "bucket_exists", "bucket_create",
    "artifact_list", "artifact_head", "artifact_read", "artifact_read_chunk",
    "artifact_write_begin", "artifact_write_chunk", "artifact_write_commit",
    "artifact_append", "artifact_delete",
))
_STATUSES = frozenset(("ok", "not_found", "sharing_denied", "authorization_denied",
    "authentication_denied", "approval_required", "sensitive_rejected",
    "dependency_unavailable", "unsupported_operation", "invalid_resource", "invalid_frame", "resource_exhausted",
    "revision_conflict", "unknown_effect", "stopped", "lease_lost"))


def _header(header, reply):
    keys = {"revision", "sequence", "status", "result", "receipt"} if reply else {"revision", "sequence", "operation", "resource", "arguments"}
    if not isinstance(header, dict) or set(header) != keys or type(header["revision"]) is not int or header["revision"] != 1:
        raise PlatformError("invalid_frame")
    if type(header["sequence"]) is not int or not 1 <= header["sequence"] <= 4096:
        raise PlatformError("invalid_frame")
    if not reply:
        if not isinstance(header["operation"], str) or header["operation"] not in _OPERATIONS or not isinstance(header["resource"], dict) or not isinstance(header["arguments"], dict):
            raise PlatformError("invalid_frame")
        return
    if not isinstance(header["status"], str) or header["status"] not in _STATUSES:
        raise PlatformError("invalid_reply")
    receipt = header["receipt"]
    if receipt is not None:
        if not isinstance(receipt, dict) or set(receipt) != {"effect_id", "call_sha256", "state"}:
            raise PlatformError("invalid_reply")
        if any(not isinstance(receipt[key], str) or not re.fullmatch(r"[a-f0-9]{64}", receipt[key]) for key in ("effect_id", "call_sha256")):
            raise PlatformError("invalid_reply")
        if receipt["state"] not in ("committed", "uncertain"):
            raise PlatformError("invalid_reply")
    if header["status"] == "ok" and (receipt is None or receipt["state"] != "committed"):
        raise PlatformError("invalid_reply")
    if header["status"] != "ok" and header["result"] is not None:
        raise PlatformError("invalid_reply")


class PlatformError(Exception):
    def __init__(self, code):
        self.code = code
        super().__init__("Code platform operation failed: " + code)


def _shape(value, depth=0, remaining=None):
    if remaining is None:
        remaining = [MAX_VALUES]
    if depth > MAX_DEPTH or remaining[0] == 0:
        raise PlatformError("resource_exhausted")
    remaining[0] -= 1
    if isinstance(value, float) and not math.isfinite(value):
        raise PlatformError("invalid_frame")
    if isinstance(value, dict):
        if any(not isinstance(key, str) for key in value):
            raise PlatformError("invalid_frame")
        for child in value.values():
            _shape(child, depth + 1, remaining)
    elif isinstance(value, (list, tuple)):
        for child in value:
            _shape(child, depth + 1, remaining)
    elif value is not None and not isinstance(value, (str, int, float, bool)):
        raise PlatformError("invalid_frame")


def _unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise PlatformError("invalid_frame")
        result[key] = value
    return result


def _bounded_json(value, bound):
    data = bytearray()
    remaining = [MAX_VALUES]

    def append(piece):
        if len(piece) > bound - len(data):
            raise PlatformError("resource_exhausted")
        data.extend(piece)

    def string(value):
        if len(value) > bound - len(data):
            raise PlatformError("resource_exhausted")
        append(b'"')
        for character in value:
            code = ord(character)
            if character in ('"', "\\"):
                append(b"\\" + character.encode("ascii"))
            elif code < 32:
                append(("\\u%04x" % code).encode("ascii"))
            elif 0xd800 <= code <= 0xdfff:
                raise PlatformError("invalid_frame")
            else:
                append(character.encode("utf-8"))
        append(b'"')

    def write(item, depth):
        if depth > MAX_DEPTH or remaining[0] == 0:
            raise PlatformError("resource_exhausted")
        remaining[0] -= 1
        kind = type(item)
        if item is None:
            append(b"null")
        elif kind is bool:
            append(b"true" if item else b"false")
        elif kind is str:
            string(item)
        elif kind is int:
            if item.bit_length() > 4096:
                raise PlatformError("resource_exhausted")
            append(str(item).encode("ascii"))
        elif kind is float:
            if not math.isfinite(item):
                raise PlatformError("invalid_frame")
            append(json.dumps(item, allow_nan=False).encode("ascii"))
        elif kind in (list, tuple, dict):
            if len(item) > remaining[0]:
                raise PlatformError("resource_exhausted")
            is_object = kind is dict
            append(b"{" if is_object else b"[")
            first = True
            children = item.items() if is_object else ((None, child) for child in item)
            for key, child in children:
                if not first:
                    append(b",")
                first = False
                if is_object:
                    if type(key) is not str:
                        raise PlatformError("invalid_frame")
                    string(key)
                    append(b":")
                write(child, depth + 1)
            append(b"}" if is_object else b"]")
        else:
            raise PlatformError("invalid_frame")

    write(value, 0)
    return data


def encode_frame(header, payload=b"", *, reply=False):
    payload_bytes = payload.nbytes if type(payload) is memoryview else len(payload) if type(payload) in (bytes, bytearray) else MAX_CHUNK + 1
    if type(payload) not in (bytes, bytearray, memoryview) or payload_bytes > MAX_CHUNK:
        raise PlatformError("resource_exhausted")
    bound = MAX_REPLY_HEADER if reply else MAX_REQUEST_HEADER
    data = _bounded_json(header, bound)
    _header(header, reply)
    return struct.pack(">II", len(data), payload_bytes) + data + bytes(payload)


def _bounded_parse(text):
    index = 0
    values = 0
    number = re.compile(r"-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?")

    def white():
        nonlocal index
        while index < len(text) and text[index] in " \t\r\n":
            index += 1

    def string():
        nonlocal index
        if index >= len(text) or text[index] != '"':
            raise PlatformError("invalid_frame")
        try:
            result, index = json.decoder.scanstring(text, index + 1, True)
        except (ValueError, UnicodeError) as error:
            raise PlatformError("invalid_frame") from error
        return result

    def value(depth):
        nonlocal index, values
        values += 1
        if depth > MAX_DEPTH or values > MAX_VALUES:
            raise PlatformError("resource_exhausted")
        white()
        if index >= len(text):
            raise PlatformError("invalid_frame")
        current = text[index]
        if current == '"':
            return string()
        if current in "{[":
            is_object = current == "{"
            end = "}" if is_object else "]"
            result = {} if is_object else []
            index += 1
            white()
            if index < len(text) and text[index] == end:
                index += 1
                return result
            while True:
                if is_object:
                    key = string()
                    if key in result:
                        raise PlatformError("invalid_frame")
                    white()
                    if index >= len(text) or text[index] != ":":
                        raise PlatformError("invalid_frame")
                    index += 1
                    result[key] = value(depth + 1)
                else:
                    result.append(value(depth + 1))
                white()
                if index >= len(text):
                    raise PlatformError("invalid_frame")
                next_token = text[index]
                index += 1
                if next_token == end:
                    return result
                if next_token != ",":
                    raise PlatformError("invalid_frame")
                white()
        for literal, parsed in (("null", None), ("true", True), ("false", False)):
            if text.startswith(literal, index):
                index += len(literal)
                return parsed
        match = number.match(text, index)
        if match is None or len(match[0]) > 1280:
            raise PlatformError("invalid_frame")
        index = match.end()
        parsed = float(match[0]) if any(c in match[0] for c in ".eE") else int(match[0])
        if type(parsed) is float and not math.isfinite(parsed):
            raise PlatformError("invalid_frame")
        return parsed

    parsed = value(0)
    white()
    if index != len(text):
        raise PlatformError("invalid_frame")
    return parsed


def decode_frame(frame, *, reply=False):
    bound = MAX_REPLY_HEADER if reply else MAX_REQUEST_HEADER
    if not isinstance(frame, (bytes, bytearray)) or len(frame) < 8 or len(frame) > 8 + bound + MAX_CHUNK:
        raise PlatformError("invalid_frame")
    head_len, payload_len = struct.unpack(">II", frame[:8])
    if head_len > bound or payload_len > MAX_CHUNK or len(frame) != 8 + head_len + payload_len:
        raise PlatformError("invalid_frame")
    try:
        header = _bounded_parse(frame[8:8 + head_len].decode("utf-8"))
    except (UnicodeError, ValueError, RecursionError) as error:
        raise PlatformError("invalid_frame") from error
    _shape(header)
    _header(header, reply)
    if reply and header["status"] != "ok" and payload_len:
        raise PlatformError("invalid_reply")
    return header, bytes(frame[8 + head_len:])


def _id(value):
    if isinstance(value, bool):
        raise PlatformError("invalid_resource")
    value = str(value)
    if not re.fullmatch(r"[1-9][0-9]{0,9}", value) or int(value) > 2147483647:
        raise PlatformError("invalid_resource")
    return value


class SandboxClient:
    """The image supplies exchange. User code never supplies a token or project."""
    def __init__(self, exchange, max_calls=128):
        if not callable(exchange) or type(max_calls) is not int or not 1 <= max_calls <= 4096:
            raise PlatformError("invalid_binding")
        self._exchange = exchange
        self._max_calls = max_calls
        self._sequence = 0
        self._lock = asyncio.Lock()
        self._unknown = False

    async def call(self, operation, resource, arguments=None, payload=b""):
        if self._unknown:
            raise PlatformError("unknown_effect")
        if operation not in _OPERATIONS:
            raise PlatformError("invalid_resource")
        if self._lock.locked():
            raise PlatformError("in_flight_limit")
        async with self._lock:
            if self._sequence >= self._max_calls:
                raise PlatformError("call_limit")
            sequence = self._sequence + 1
            request = encode_frame({"revision": 1, "sequence": sequence,
                                    "operation": operation, "resource": resource,
                                    "arguments": {} if arguments is None else arguments}, payload)
            self._sequence = sequence
            # Observation errors do not authorize the shim to resend any call.
            try:
                response = await self._exchange(request)
                header, body = decode_frame(response, reply=True)
                if header["sequence"] != sequence:
                    raise PlatformError("invalid_reply")
            except BaseException as error:
                self._unknown = True
                if isinstance(error, asyncio.CancelledError):
                    raise
                raise PlatformError("observation_unknown") from error
            status = header["status"]
            if status != "ok":
                if status in ("unknown_effect", "stopped", "lease_lost"):
                    self._unknown = True
                raise PlatformError(status)
            return header["result"], body

    async def get_user_data(self):
        result, _ = await self.call("user_get", {"kind": "current_user"})
        return result

    async def get_list_of_apps(self, cursor=None, limit=100):
        # Return one bounded page. The caller selects each following cursor.
        result, _ = await self.call("application_list", {"kind": "application_catalog"}, {"cursor": cursor, "limit": limit})
        return result

    async def get_app_details(self, application_id):
        result, _ = await self.call("application_get", {"kind": "application", "id": _id(application_id)})
        return result

    async def get_app_version_details(self, application_id, application_version_id):
        result, _ = await self.call("application_version_get", {"kind": "application_version", "application_id": _id(application_id), "version_id": _id(application_version_id)})
        return result

    async def get_mcp_toolkits(self, cursor=None, limit=100):
        result, _ = await self.call("toolkit_list", {"kind": "toolkit_catalog"}, {"cursor": cursor, "limit": limit})
        return result

    async def mcp_tool_call(self, toolkit_id, revision, tool_name, arguments):
        if not isinstance(revision, str) or not re.fullmatch(r"[a-f0-9]{64}", revision):
            raise PlatformError("invalid_resource")
        result, _ = await self.call("toolkit_call", {"kind": "toolkit", "id": _id(toolkit_id), "revision": revision, "tool": tool_name}, arguments)
        return result

    async def unsecret(self, secret_name):
        result, _ = await self.call("secret_read", {"kind": "secret", "scope": "project", "name": secret_name})
        return result

    async def get_private_project_secret(self, secret_name, default=_UNSET):
        try:
            result, _ = await self.call("secret_read", {"kind": "secret", "scope": "personal", "name": secret_name})
            return result
        except PlatformError as error:
            if error.code == "not_found" and default is not _UNSET:
                return default
            raise

    async def bucket_exists(self, name):
        result, _ = await self.call("bucket_exists", {"kind": "bucket", "name": name})
        return result

    async def create_bucket(self, name):
        result, _ = await self.call("bucket_create", {"kind": "bucket", "name": name})
        return result

    def _invalid_observation(self):
        self._unknown = True
        raise PlatformError("unknown_effect")

    def artifact(self, bucket_name):
        return SandboxArtifact(self, bucket_name)


class SandboxArtifact:
    def __init__(self, client, bucket):
        self._client = client
        self._bucket = bucket

    def _resource(self, name):
        return {"kind": "artifact", "bucket": self._bucket, "name": name}

    async def list(self, prefix="", cursor=None, limit=100):
        result, _ = await self._client.call("artifact_list", {"kind": "bucket", "name": self._bucket}, {"prefix": prefix, "cursor": cursor, "limit": limit})
        return result

    async def head(self, name):
        result, _ = await self._client.call("artifact_head", self._resource(name))
        return result

    async def get_content_bytes(self, name):
        info, chunk = await self._client.call("artifact_read", self._resource(name))
        if type(info) is not dict or set(info) != {"bytes", "transfer", "version"} or type(info["transfer"]) is not str or not re.fullmatch(r"[a-f0-9]{64}", info["transfer"]) or type(info["version"]) is not str or not 1 <= len(info["version"]) <= 1024 or any(ord(c) < 32 or ord(c) == 127 for c in info["version"]):
            self._client._invalid_observation()
        total = info["bytes"]
        if type(total) is not int or not 0 <= total <= MAX_OBJECT_BYTES or len(chunk) > total:
            self._client._invalid_observation()
        data = bytearray(chunk)
        while len(data) < total:
            _, chunk = await self._client.call("artifact_read_chunk", {"kind": "artifact_transfer", "id": info["transfer"]}, {"offset": len(data)})
            if not chunk or len(chunk) > total - len(data):
                self._client._invalid_observation()
            data.extend(chunk)
        if len(data) != total:
            self._client._invalid_observation()
        return bytes(data)

    async def create(self, name, content):
        if type(content) is str:
            if len(content) > MAX_OBJECT_BYTES:
                raise PlatformError("resource_exhausted")
            # Count exact UTF-8 bytes before allocating the complete content.
            size = 0
            for character in content:
                size += len(character.encode("utf-8"))
                if size > MAX_OBJECT_BYTES:
                    raise PlatformError("resource_exhausted")
            data = content.encode("utf-8")
        elif type(content) in (bytes, bytearray, memoryview):
            content_bytes = content.nbytes if type(content) is memoryview else len(content)
            if content_bytes > MAX_OBJECT_BYTES:
                raise PlatformError("resource_exhausted")
            data = bytes(content)
        else:
            raise PlatformError("invalid_resource")
        info, _ = await self._client.call("artifact_write_begin", self._resource(name), {"bytes": len(data)})
        if type(info) is not dict or set(info) != {"transfer"} or type(info["transfer"]) is not str or not re.fullmatch(r"[a-f0-9]{64}", info["transfer"]):
            self._client._invalid_observation()
        transfer = {"kind": "artifact_transfer", "id": info["transfer"]}
        for offset in range(0, len(data), MAX_CHUNK):
            await self._client.call("artifact_write_chunk", transfer, {"offset": offset}, data[offset:offset + MAX_CHUNK])
        result, _ = await self._client.call("artifact_write_commit", transfer)
        return result

    async def append(self, name, text, expected_version):
        # Gate7 owns conditional append. Unsupported backends return a typed refusal.
        result, _ = await self._client.call("artifact_append", self._resource(name), {"text": text, "expected_version": expected_version})
        return result

    async def delete(self, name, expected_version):
        result, _ = await self._client.call("artifact_delete", self._resource(name), {"expected_version": expected_version})
        return result
