#!/usr/bin/env python3
"""Convert public Main command keys into fixed Code runner receipt trust."""
import argparse
import base64
import binascii
import hashlib
import json
import os
from pathlib import Path
import re
import stat

SCHEMA = "elitea.runtime-ed25519-keyring.v1"
KEYRING_LIMIT = 64 * 1024
TRUST_LIMIT = 8192
TRUST_PATH = "/opt/elitea-code-trust/main-receipt-keys.json"


class Rejected(ValueError):
    """Expose fixed failure codes without input values."""


def require(condition, code):
    if not condition:
        raise Rejected(code)


def encoded(value):
    return json.dumps(value, ensure_ascii=True, separators=(",", ":"), allow_nan=False).encode("ascii")


def strict_json(raw, limit):
    require(isinstance(raw, bytes) and 0 < len(raw) <= limit, "json.bound")

    def pairs(items):
        result = {}
        for key, value in items:
            require(key not in result, "json.duplicate")
            result[key] = value
        return result

    def constant(_value):
        raise Rejected("json.number")

    try:
        text = raw.decode("utf-8")
        depth, quoted, escaped = 0, False, False
        for char in text:
            if quoted:
                if escaped:
                    escaped = False
                elif char == "\\":
                    escaped = True
                elif char == '"':
                    quoted = False
            elif char == '"':
                quoted = True
            elif char in "{[":
                depth += 1
                require(depth <= 8, "json.depth")
            elif char in "}]":
                depth -= 1
        return json.loads(text, object_pairs_hook=pairs, parse_constant=constant)
    except (UnicodeError, json.JSONDecodeError, RecursionError, ValueError) as error:
        if isinstance(error, Rejected):
            raise
        raise Rejected("json.invalid") from error


def fields(value, names):
    require(isinstance(value, dict) and set(value) == set(names), "json.fields")


def key_id(value):
    require(isinstance(value, str) and 1 <= len(value) <= 256
            and all(0x21 <= ord(char) <= 0x7e for char in value), "key.id")
    return value


def validate_entries(entries, maximum, public_field):
    require(isinstance(entries, list) and 1 <= len(entries) <= maximum, "key.count")
    result, ids, material = [], set(), set()
    for entry in entries:
        fields(entry, ("key_id", public_field))
        identity = key_id(entry["key_id"])
        require(identity not in ids, "key.duplicate_id")
        value = entry[public_field]
        if public_field == "public_key_base64":
            require(isinstance(value, str) and len(value) == 44, "key.base64")
            try:
                public = base64.b64decode(value, validate=True)
            except (binascii.Error, ValueError) as error:
                raise Rejected("key.base64") from error
            require(len(public) == 32 and base64.b64encode(public).decode("ascii") == value, "key.base64")
        else:
            require(isinstance(value, str) and re.fullmatch(r"[0-9a-f]{64}", value) is not None, "key.hex")
            public = bytes.fromhex(value)
        require(public != bytes(32), "key.zero")
        require(public not in material, "key.duplicate_material")
        ids.add(identity)
        material.add(public)
        result.append({"key_id": identity, "public_key_hex": public.hex()})
    return result


def trust_bytes(entries, required_key_id=None):
    require(1 <= len(entries) <= 8, "key.count")
    if required_key_id is not None:
        require(key_id(required_key_id) in {entry["key_id"] for entry in entries}, "key.required")
    result = encoded({"revision": 1, "keys": sorted(entries, key=lambda entry: entry["key_id"])})
    require(len(result) <= TRUST_LIMIT, "json.bound")
    return result


def convert(raw, selected_ids=None, required_key_id=None):
    document = strict_json(raw, KEYRING_LIMIT)
    fields(document, ("schema_version", "keys"))
    require(document["schema_version"] == SCHEMA, "keyring.schema")
    entries = validate_entries(document["keys"], 64, "public_key_base64")
    if selected_ids is not None:
        require(1 <= len(selected_ids) <= 8, "key.count")
        wanted = [key_id(identity) for identity in selected_ids]
        require(len(set(wanted)) == len(wanted), "key.duplicate_id")
        require(set(wanted) <= {entry["key_id"] for entry in entries}, "key.selection")
        entries = [entry for entry in entries if entry["key_id"] in wanted]
    return trust_bytes(entries, required_key_id)


def validate(raw, required_key_id=None):
    document = strict_json(raw, TRUST_LIMIT)
    fields(document, ("revision", "keys"))
    require(type(document["revision"]) is int and document["revision"] == 1, "trust.revision")
    return trust_bytes(validate_entries(document["keys"], 8, "public_key_hex"), required_key_id)


def validate_runner_image(value):
    # Require a repository reference with an exact digest. Do not accept a tag or image ID.
    require(isinstance(value, str) and len(value) <= 1024 and re.fullmatch(
        r"[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]+)?"
        r"(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*@sha256:[0-9a-f]{64}", value
    ) is not None, "image.digest")


def path_value(value, parent_only=False):
    require(isinstance(value, (str, Path)), "file.path")
    text = str(value)
    require(0 < len(os.fsencode(text)) <= 4096 and not any(char in text for char in "\0\r\n"), "file.path")
    path = Path(text)
    require(path.is_absolute() and os.path.normpath(text) == text, "file.path")
    checked = path.parent if parent_only else path
    require(checked.resolve(strict=True) == checked, "file.path")
    return path


def file_identity(info):
    return (info.st_dev, info.st_ino, info.st_mode, info.st_nlink, info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def public_file(value, bound):
    path = path_value(value)
    before = path.lstat()
    require(stat.S_ISREG(before.st_mode) and before.st_nlink == 1
            and before.st_mode & 0o400 and not before.st_mode & 0o7133
            and 0 < before.st_size <= bound, "file.regular")
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
    try:
        require(file_identity(os.fstat(descriptor)) == file_identity(before), "file.changed")
        with os.fdopen(descriptor, "rb", closefd=False) as stream:
            raw = stream.read(bound + 1)
        require(len(raw) == before.st_size and file_identity(os.fstat(descriptor)) == file_identity(before), "file.changed")
        require(file_identity(path.lstat()) == file_identity(before), "file.changed")
        return raw
    finally:
        os.close(descriptor)


def publish(value, raw):
    path = path_value(value, parent_only=True)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o444)
    with os.fdopen(descriptor, "wb") as stream:
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())
        os.fchmod(stream.fileno(), 0o444)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="mode", required=True)
    conversion = commands.add_parser("convert")
    conversion.add_argument("--key-id", action="append", help="Select each admitted rotation key explicitly.")
    conversion.add_argument("--output", required=True)
    validation = commands.add_parser("validate")
    validation.add_argument("--output")
    validation.add_argument("--runner-image", help="Require a repository image pinned by sha256 digest.")
    for command in (conversion, validation):
        command.add_argument("--input", required=True)
        command.add_argument("--require-key-id", help="Require Main's active signing key ID in the output.")
    args = parser.parse_args()
    try:
        if args.mode == "convert":
            raw = convert(public_file(args.input, KEYRING_LIMIT), args.key_id, args.require_key_id)
        else:
            if args.runner_image is not None:
                validate_runner_image(args.runner_image)
            raw = validate(public_file(args.input, TRUST_LIMIT), args.require_key_id)
        if args.output is not None:
            publish(args.output, raw)
        print(encoded({"status": "written" if args.output else "validated", "sha256": hashlib.sha256(raw).hexdigest(),
                       "bytes": len(raw), "trust_path": TRUST_PATH}).decode("ascii"))
    except (Rejected, OSError, ValueError) as error:
        print(encoded({"status": "rejected", "code": str(error) if isinstance(error, Rejected) else "operation.failed"}).decode("ascii"))
        raise SystemExit(1)


if __name__ == "__main__":
    main()
