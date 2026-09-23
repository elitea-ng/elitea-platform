"""Deterministic fake qTest backend for the standalone stack (issue #939, group 4).

Why this exists
---------------
Nine `toolkits-credentials` onetest cases (ELITEA-2243..2251) exercise the qTest
toolkit's WRITE surface — record a manual execution status, attach a file from
an artifact bucket, and the error paths around both. qTest has no `e2e/live`
lane in this repo and mock-llm has no qTest-shaped backend, so those cases had
nowhere to run. This serves the four routes the SDK's qTest wrapper actually
calls (`elitea_sdk/tools/qtest/api_wrapper.py`), with the same shapes its
parsers read, and journals every request so a test can prove SERVER-SIDE what
the toolkit did rather than believing the model's prose.

Standard library only, same posture as `deploy/mock-llm/server.py`: no pip
install, no network at build or run time, byte-predictable answers.

What it serves
--------------
  GET  /healthz                                              compose healthcheck
  GET  /api/v3/projects                                      the connection check
  POST /api/v3/projects/{pid}/search                         DQL search (test-runs, test-cases)
  GET  /api/v3/projects/{pid}/test-runs/execution-statuses   the id space submit_test_log needs
  POST /api/v3/projects/{pid}/test-runs/{runId}/test-logs    record a manual execution
  POST /api/v3/projects/{pid}/{objectType}/{objectId}/blob-handles   attach a binary
  GET  /api/v3/projects/{pid}/test-cases/{id}/versions[/{vid}]       version validation
  GET  /api/v3/projects/{pid}/test-runs/{runId}              read back a run (the cases' "verify via qTest API")
  GET  /api/v3/projects/{pid}/test-runs/{runId}/attachments  read back its attachments

  GET    /__journal   every request, newest last: {method, path, status, auth_ok, body_bytes}
  DELETE /__journal   empty it
  GET    /__state     the mutable backend: runs (with their latest log), logs, attachments
  POST   /__reset     back to the seeded state, journal included

Authorisation
-------------
Every /api/v3 route requires `Authorization: Bearer <QTEST_MOCK_TOKEN>` and
answers 401 otherwise. That is not decoration: half the point of the cases is
that the toolkit resolved a CREDENTIAL, and a backend that answered without one
could not tell a working credential from an absent one.

Seed
----
Project 1 ("E2E-QTEST") holds two runs:
  TR-1  (id 4001, test case TC-1 / id 3001) — no execution log yet
  TR-2  (id 4002, test case TC-2 / id 3002) — no execution log yet
Any other id is genuinely absent, which is what the invalid-testRunId case
needs: `search` answers `{"total": 0, "items": []}` and the toolkit raises.
"""
from __future__ import annotations

import hashlib
import json
import os
import re
import threading
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse

PORT = int(os.environ.get("QTEST_MOCK_PORT", "8096"))
TOKEN = os.environ.get("QTEST_MOCK_TOKEN", "e2e-qtest-token")

# The nine statuses a manual run can carry. `__resolve_test_run_status` matches
# by NAME (case-insensitively) and submits only the id, so the names are the
# contract and the ids only have to be stable.
EXECUTION_STATUSES = [
    {"id": 601, "name": "Passed", "is_default": False, "color": "#0e8a16", "active": True},
    {"id": 602, "name": "Failed", "is_default": False, "color": "#d73a4a", "active": True},
    {"id": 603, "name": "Incomplete", "is_default": False, "color": "#fbca04", "active": True},
    {"id": 604, "name": "Blocked", "is_default": False, "color": "#b60205", "active": True},
    {"id": 605, "name": "Unexecuted", "is_default": True, "color": "#cccccc", "active": True},
    {"id": 606, "name": "Skipped", "is_default": False, "color": "#999999", "active": True},
    {"id": 607, "name": "Not Applicable", "is_default": False, "color": "#888888", "active": True},
    {"id": 608, "name": "In Progress", "is_default": False, "color": "#1d76db", "active": True},
    {"id": 609, "name": "Retest", "is_default": False, "color": "#5319e7", "active": True},
]

_LOCK = threading.Lock()


def _seed() -> dict:
    return {
        "runs": {
            "TR-1": {
                "id": 4001,
                "pid": "TR-1",
                "name": "Autotest run one",
                "testCaseId": 3001,
                "test_case_version": "1.0",
                "latest_test_log": None,
                "properties": [
                    {"field_id": 1, "field_name": "Status", "field_value_name": "Unexecuted"},
                ],
            },
            "TR-2": {
                "id": 4002,
                "pid": "TR-2",
                "name": "Autotest run two",
                "testCaseId": 3002,
                "test_case_version": "1.0",
                "latest_test_log": None,
                "properties": [
                    {"field_id": 1, "field_name": "Status", "field_value_name": "Unexecuted"},
                ],
            },
        },
        "cases": {
            "TC-1": {"id": 3001, "pid": "TC-1", "name": "Autotest case one", "properties": []},
            "TC-2": {"id": 3002, "pid": "TC-2", "name": "Autotest case two", "properties": []},
        },
        # Attachments keyed by "{object_type}/{object_id}" — qTest has no
        # attachment slot on the bare run, only on its test case or its latest
        # test log, and the toolkit picks between them by `attachment_type`.
        "attachments": {},
        "logs": [],
        "next_log_id": 7001,
        "next_blob_id": 9001,
    }


STATE = _seed()
JOURNAL: list[dict] = []


def _now() -> str:
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat()


def _run_by_qtest_id(qtest_id: int) -> dict | None:
    for run in STATE["runs"].values():
        if run["id"] == qtest_id:
            return run
    return None


_DQL_ID = re.compile(r"""['"]?\bid['"]?\s*=\s*['"]?([A-Za-z0-9-]+)['"]?""", re.IGNORECASE)


def _dql_target(query: str) -> str:
    """The single id a `Id = 'TR-1'` query names, or '' when it names none.

    The wrapper builds exactly two query shapes — `Id = '<pid>'` and
    `'id' = '<numeric>'` — so a full DQL parser would be pretending to a
    generality nothing here exercises.
    """
    match = _DQL_ID.search(query or "")
    return match.group(1) if match else ""


def _search(object_type: str, query: str) -> list[dict]:
    target = _dql_target(query)
    if not target:
        return []
    bucket = STATE["runs"] if object_type == "test-runs" else STATE["cases"]
    if target in bucket:
        return [bucket[target]]
    if target.isdigit():
        numeric = int(target)
        return [item for item in bucket.values() if item["id"] == numeric]
    return []


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    # ── plumbing ────────────────────────────────────────────────────────────
    def log_message(self, fmt: str, *args) -> None:  # noqa: A002 - stdlib name
        print(f"qtest-mock {self.address_string()} {fmt % args}", flush=True)

    def _body(self) -> bytes:
        length = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(length) if length else b""

    def _send(self, status: int, payload, *, raw: bytes | None = None) -> None:
        data = raw if raw is not None else json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _authorised(self) -> bool:
        return self.headers.get("Authorization", "") == f"Bearer {TOKEN}"

    def _record(self, path: str, status: int, auth_ok: bool, body: bytes) -> None:
        with _LOCK:
            JOURNAL.append({
                "at": _now(),
                "method": self.command,
                "path": path,
                "status": status,
                "auth_ok": auth_ok,
                "body_bytes": len(body),
                "file_name": self.headers.get("File-Name"),
                "content_type": self.headers.get("Content-Type"),
            })

    # ── routing ─────────────────────────────────────────────────────────────
    def do_GET(self) -> None:  # noqa: N802 - stdlib name
        path = urlparse(self.path).path
        if path == "/healthz":
            self._send(200, {"status": "ok"})
            return
        if path == "/__journal":
            with _LOCK:
                self._send(200, list(JOURNAL))
            return
        if path == "/__state":
            with _LOCK:
                self._send(200, STATE)
            return
        if not path.startswith("/api/v3/"):
            self._send(404, {"message": f"no route for {path}"})
            return
        auth_ok = self._authorised()
        status, payload = self._route_get(path) if auth_ok else (401, {"message": "unauthorised"})
        self._record(path, status, auth_ok, b"")
        self._send(status, payload)

    def _route_get(self, path: str) -> tuple[int, object]:
        if path == "/api/v3/projects":
            return 200, [{"id": 1, "name": "E2E-QTEST", "status": "Active"}]

        statuses = re.fullmatch(r"/api/v3/projects/(\d+)/test-runs/execution-statuses", path)
        if statuses:
            return 200, EXECUTION_STATUSES

        run = re.fullmatch(r"/api/v3/projects/(\d+)/test-runs/(\d+)", path)
        if run:
            found = _run_by_qtest_id(int(run.group(2)))
            if not found:
                return 404, {"message": f"test run {run.group(2)} not found"}
            return 200, found

        run_attachments = re.fullmatch(r"/api/v3/projects/(\d+)/test-runs/(\d+)/attachments", path)
        if run_attachments:
            found = _run_by_qtest_id(int(run_attachments.group(2)))
            if not found:
                return 404, {"message": f"test run {run_attachments.group(2)} not found"}
            return 200, self._attachments_of(found)

        version = re.fullmatch(r"/api/v3/projects/(\d+)/test-cases/(\d+)/versions/(\d+)", path)
        if version:
            # Only version id 1 exists, which is what makes the version-validation
            # branch of update_test_run_status answerable both ways.
            if version.group(3) != "1":
                return 404, {"message": "version not found"}
            return 200, {"test_case_version_id": 1, "version": "1.0"}

        versions = re.fullmatch(r"/api/v3/projects/(\d+)/test-cases/(\d+)/versions", path)
        if versions:
            return 200, [{"test_case_version_id": 1, "version": "1.0", "name": "Autotest case"}]

        return 404, {"message": f"no route for {path}"}

    @staticmethod
    def _attachments_of(run: dict) -> list[dict]:
        keys = [f"test-cases/{run.get('testCaseId')}"]
        latest = run.get("latest_test_log") or {}
        if latest.get("id"):
            keys.append(f"test-logs/{latest['id']}")
        out: list[dict] = []
        for key in keys:
            out.extend(STATE["attachments"].get(key, []))
        return out

    def do_POST(self) -> None:  # noqa: N802 - stdlib name
        path = urlparse(self.path).path
        body = self._body()
        if path == "/__reset":
            with _LOCK:
                STATE.clear()
                STATE.update(_seed())
                JOURNAL.clear()
            self._send(200, {"status": "reset"})
            return
        if not path.startswith("/api/v3/"):
            self._send(404, {"message": f"no route for {path}"})
            return
        auth_ok = self._authorised()
        status, payload = self._route_post(path, body) if auth_ok else (401, {"message": "unauthorised"})
        self._record(path, status, auth_ok, body)
        self._send(status, payload)

    def _route_post(self, path: str, body: bytes) -> tuple[int, object]:
        search = re.fullmatch(r"/api/v3/projects/(\d+)/search", path)
        if search:
            try:
                request = json.loads(body or b"{}")
            except ValueError:
                return 400, {"message": "body is not JSON"}
            items = _search(request.get("object_type", ""), request.get("query", ""))
            return 200, {
                "links": [],
                "page": 1,
                "page_size": len(items),
                "total": len(items),
                "items": items,
            }

        test_log = re.fullmatch(r"/api/v3/projects/(\d+)/test-runs/(\d+)/test-logs", path)
        if test_log:
            return self._submit_test_log(int(test_log.group(2)), body)

        blob = re.fullmatch(r"/api/v3/projects/(\d+)/([a-z-]+)/(\d+)/blob-handles", path)
        if blob:
            return self._store_blob(blob.group(2), int(blob.group(3)), body)

        return 404, {"message": f"no route for {path}"}

    def _submit_test_log(self, qtest_run_id: int, body: bytes) -> tuple[int, object]:
        run = _run_by_qtest_id(qtest_run_id)
        if not run:
            return 404, {"message": f"test run {qtest_run_id} not found"}
        try:
            request = json.loads(body or b"{}")
        except ValueError:
            return 400, {"message": "body is not JSON"}
        status_id = ((request.get("status") or {}).get("id"))
        known = {item["id"]: item for item in EXECUTION_STATUSES}
        if status_id not in known:
            # What the real endpoint answers for a bare status name — the reason
            # the wrapper resolves the id first.
            return 400, {"message": f"invalid status id {status_id!r}"}
        version_id = request.get("test_case_version_id")
        if version_id not in (None, 1):
            return 400, {"message": f"unknown test_case_version_id {version_id!r}"}
        with _LOCK:
            log_id = STATE["next_log_id"]
            STATE["next_log_id"] += 1
            log = {
                "id": log_id,
                "test_run_id": run["id"],
                "status": known[status_id],
                "note": request.get("note"),
                "exe_start_date": request.get("exe_start_date"),
                "exe_end_date": request.get("exe_end_date"),
                "test_case_version_id": version_id,
                "recorded_at": _now(),
            }
            STATE["logs"].append(log)
            run["latest_test_log"] = log
            for prop in run["properties"]:
                if prop.get("field_name") == "Status":
                    prop["field_value_name"] = known[status_id]["name"]
        return 201, log

    def _store_blob(self, object_type: str, object_id: int, body: bytes) -> tuple[int, object]:
        if object_type not in ("test-cases", "test-logs", "test-steps"):
            return 400, {"message": f"unsupported object type {object_type}"}
        if not body:
            return 400, {"message": "an attachment must carry bytes"}
        with _LOCK:
            blob_id = STATE["next_blob_id"]
            STATE["next_blob_id"] += 1
            record = {
                "id": blob_id,
                "name": self.headers.get("File-Name") or f"attachment-{blob_id}",
                "content_type": self.headers.get("Content-Type") or "application/octet-stream",
                "size": len(body),
                # The digest, not the bytes: a test proves the file that arrived
                # is the file the artifact held without this process keeping
                # megabytes of it, and a truncated upload cannot read as equal.
                "md5": hashlib.md5(body).hexdigest(),
                "object_type": object_type,
                "object_id": object_id,
                "uploaded_at": _now(),
            }
            STATE["attachments"].setdefault(f"{object_type}/{object_id}", []).append(record)
        return 201, record

    def do_DELETE(self) -> None:  # noqa: N802 - stdlib name
        if urlparse(self.path).path == "/__journal":
            with _LOCK:
                JOURNAL.clear()
            self._send(200, {"status": "cleared"})
            return
        self._send(404, {"message": "no route"})


def main() -> None:
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"qtest-mock listening on :{PORT} (bearer {TOKEN[:4]}…)", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
