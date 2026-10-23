#!/usr/bin/env python3
"""Generate or verify frozen current index-schedule observations.

This harness intentionally does not import Elitea or legacy-runtime source. It
defines the small public validation contract independently, then evaluates the
checked-in payloads with the installed Pydantic and croniter. Point PYTHONPATH
at the current scheduling plugin's installed requirements when croniter is not
in the active environment.

Examples:
  python verify_current_schedule_contract.py
  python verify_current_schedule_contract.py --generate

Exit 77 means the optional Python dependencies are unavailable.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib
import importlib.metadata
import json
import sys
import warnings
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Optional
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

try:
    from croniter import croniter
    from pydantic import BaseModel, Field, validator
except ModuleNotFoundError as error:
    print(f"SKIP: optional current-contract dependency is unavailable: {error}", file=sys.stderr)
    raise SystemExit(77) from error

warnings.filterwarnings("ignore", category=DeprecationWarning)

DAILY_FLOOR = timedelta(hours=24)
GAP_PROBE_FIRINGS = 32
FIXTURE = Path(__file__).with_name("current_python_schedule_contract.json")


def validate_cron_expression(value: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise ValueError("cron must be a non-empty string")
    value = value.strip()
    croniter(value)
    return value


def validate_daily_floor(expression: str) -> str:
    iterator = croniter(expression, datetime(2000, 1, 1, tzinfo=timezone.utc))
    previous = iterator.get_next(datetime)
    for _ in range(GAP_PROBE_FIRINGS):
        next_run = iterator.get_next(datetime)
        if next_run - previous < DAILY_FLOOR:
            raise ValueError("Frequency cannot be more than once per day")
        previous = next_run
    return expression


class Credentials(BaseModel):
    private: Optional[bool] = False
    elitea_title: str


class UpdateIndexingSchedule(BaseModel):
    cron: str
    enabled: bool = False
    user_id: Optional[int] = -1
    credentials: Optional[Credentials] = None
    timezone: str

    @validator("timezone")
    def validate_timezone(cls, value):
        try:
            ZoneInfo(value)
        except ZoneInfoNotFoundError as error:
            raise ValueError("timezone must be a valid IANA timezone name") from error
        return value

    @validator("cron")
    def validate_cron(cls, value: str) -> str:
        return validate_daily_floor(validate_cron_expression(value))


class ToolkitIndexingSchedule(BaseModel):
    cron: str
    enabled: bool
    credentials: Optional[Credentials] = None
    created_by: int = Field(gt=0)
    timezone: str
    last_run: str

    @validator("timezone")
    def validate_timezone(cls, value):
        try:
            ZoneInfo(value)
        except ZoneInfoNotFoundError as error:
            raise ValueError("timezone must be a valid IANA timezone name") from error
        return value

    @validator("last_run", pre=True)
    def normalize_last_run(cls, value):
        parsed = value if isinstance(value, datetime) else datetime.fromisoformat(value)
        if parsed.tzinfo is None or parsed.tzinfo.utcoffset(parsed) is None:
            parsed = parsed.replace(tzinfo=timezone.utc)
        else:
            parsed = parsed.astimezone(timezone.utc)
        return parsed.isoformat()

    @validator("cron")
    def validate_cron(cls, value: str) -> str:
        return validate_cron_expression(value)


def model_validate(model_type, value):
    if hasattr(model_type, "model_validate"):
        return model_type.model_validate(value)
    return model_type.parse_obj(value)


def model_dump(model):
    if hasattr(model, "model_dump"):
        return model.model_dump()
    return model.dict()


def observe(payload) -> bool:
    try:
        payload = dict(payload)
        update = model_validate(UpdateIndexingSchedule, payload)
        model_validate(
            ToolkitIndexingSchedule,
            {
                "cron": update.cron,
                "enabled": update.enabled,
                "credentials": model_dump(update.credentials) if update.credentials else None,
                "created_by": 11,
                "timezone": update.timezone,
                "last_run": datetime(2026, 7, 27, 9, 34, 56, tzinfo=timezone.utc),
            },
        )
        return True
    except Exception:
        return False


def croniter_source_sha256() -> str:
    module = importlib.import_module("croniter.croniter")
    source = Path(module.__file__).read_bytes()
    return hashlib.sha256(source).hexdigest()


def load_fixture() -> dict:
    with FIXTURE.open("r", encoding="utf-8") as fixture_file:
        return json.load(fixture_file)


def observations(fixture: dict) -> list[dict]:
    return [
        {
            "name": case["name"],
            "current_accepted": observe(case["payload"]),
        }
        for case in fixture["cases"]
    ]


def generate(fixture: dict) -> None:
    result = {
        "croniter_version": importlib.metadata.version("croniter"),
        "croniter_source_sha256": croniter_source_sha256(),
        "pydantic_version": importlib.metadata.version("pydantic"),
        "observations": observations(fixture),
    }
    print(json.dumps(result, indent=2, sort_keys=True))


def verify(fixture: dict) -> None:
    for package, field in (
        ("croniter", "observed_croniter_version"),
        ("pydantic", "observed_pydantic_version"),
    ):
        expected_version = fixture["provenance"][field]
        actual_version = importlib.metadata.version(package)
        if actual_version != expected_version:
            raise SystemExit(
                f"{package} version drift: got {actual_version}, "
                f"expected {expected_version}"
            )

    expected_sha = fixture["provenance"]["croniter_source_sha256"]
    actual_sha = croniter_source_sha256()
    if actual_sha != expected_sha:
        raise SystemExit(
            f"croniter source drift: got {actual_sha}, expected {expected_sha}"
        )

    mismatches = []
    for case, observed in zip(fixture["cases"], observations(fixture), strict=True):
        if observed["current_accepted"] != case["current_accepted"]:
            mismatches.append(
                {
                    "name": case["name"],
                    "expected": case["current_accepted"],
                    "observed": observed["current_accepted"],
                }
            )
    if mismatches:
        raise SystemExit("current fixture drift:\n" + json.dumps(mismatches, indent=2))
    print(
        "verified "
        f"{len(fixture['cases'])} cases with croniter "
        f"{importlib.metadata.version('croniter')} ({actual_sha})"
    )


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--generate",
        action="store_true",
        help="print deterministic observations instead of checking expected values",
    )
    arguments = parser.parse_args()
    fixture = load_fixture()
    if arguments.generate:
        generate(fixture)
    else:
        verify(fixture)


if __name__ == "__main__":
    main()
