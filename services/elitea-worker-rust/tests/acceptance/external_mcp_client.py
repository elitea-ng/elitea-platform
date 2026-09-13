"""Verify an autonomous Elitea MCP tool with an independently authenticated client.

Supply ELITEA_MCP_BEARER_TOKEN in the environment. Do not put credentials in argv.
This script never reads browser state or prints authentication material.
"""
import argparse
import json
import os
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("url", help="Exact toolkit, agent, or pipeline MCP URL")
    parser.add_argument("--tool", required=True)
    parser.add_argument("--arguments", required=True, help="JSON arguments for a synthetic test")
    parser.add_argument("--expect", required=True, help="Synthetic marker required in the result")
    parser.add_argument("--resume", action="store_true", help="Disconnect after the priming event and resume with GET")
    parser.add_argument("--restart-main", action="store_true", help="Restart the named local rehearsal Main after disconnect")
    args = parser.parse_args()
    url = urllib.parse.urlsplit(args.url)
    if url.scheme != "https" and not (url.scheme == "http" and url.hostname in ("localhost", "127.0.0.1")):
        parser.error("Use HTTPS, or HTTP on localhost for rehearsal")
    if url.username or url.password or url.query or url.fragment:
        parser.error("The MCP URL must not contain credentials, query parameters, or fragments")
    if args.restart_main and (not args.resume or url.hostname not in ("localhost", "127.0.0.1")):
        parser.error("Main restart requires --resume against localhost rehearsal")
    token = os.environ.get("ELITEA_MCP_BEARER_TOKEN")
    if not token:
        parser.error("Set ELITEA_MCP_BEARER_TOKEN through the approved credential mechanism")
    arguments = json.loads(args.arguments)
    if not isinstance(arguments, dict):
        parser.error("Arguments must be a JSON object")
    opener = urllib.request.build_opener(NoRedirect())
    session_id = None
    protocol = None

    def read_sse(response, stop_after_prime=False):
        assert response.headers.get_content_type() == "text/event-stream", "Expected SSE transport"
        cursor = None
        body = None
        data = []
        total = 0
        for line in response:
            total += len(line)
            assert total <= 4 * 1024 * 1024, "SSE response exceeds acceptance bound"
            text = line.decode("utf-8").rstrip("\r\n")
            if text.startswith("id:"):
                cursor = text[3:].lstrip(" ")
            elif text.startswith("data:"):
                data.append(text[5:].lstrip(" "))
            elif text == "":
                payload = "\n".join(data)
                data = []
                if payload:
                    assert body is None, "Duplicate JSON-RPC response"
                    body = json.loads(payload)
                if stop_after_prime and cursor:
                    assert body is None, "No separate priming event"
                    return cursor, None
        return cursor, body

    def resume_call(payload, headers):
        request = urllib.request.Request(args.url, json.dumps(payload).encode(), headers)
        with opener.open(request, timeout=120) as response:
            initial_cursor, _ = read_sse(response, stop_after_prime=True)
        assert initial_cursor, "No recovery cursor was supplied"
        if args.restart_main:
            restarted = subprocess.run(["docker", "restart", "-t", "0", "elitea-rust-rehearsal-elitea-main-1"], capture_output=True, timeout=45)
            assert restarted.returncode == 0, "Rehearsal Main restart failed"
        resume_headers = dict(headers, **{"Last-Event-ID": initial_cursor})
        deadline = time.monotonic() + 180
        body = None
        final_cursor = None
        while time.monotonic() < deadline:
            try:
                request = urllib.request.Request(args.url, headers=resume_headers, method="GET")
                with opener.open(request, timeout=60) as response:
                    final_cursor, body = read_sse(response)
                if body is not None:
                    break
            except urllib.error.HTTPError as error:
                if error.code not in (502, 503, 504):
                    raise RuntimeError(f"MCP resume HTTP failure: {error.code}") from None
            except (urllib.error.URLError, TimeoutError, ConnectionError):
                pass
            time.sleep(1)
        assert body is not None, "Resumed invocation did not complete"
        # Re-reading the initial cursor returns the same result without a new POST.
        request = urllib.request.Request(args.url, headers=resume_headers, method="GET")
        with opener.open(request, timeout=60) as response:
            _, replay = read_sse(response)
        assert replay == body, "Replay changed the JSON-RPC result"
        resume_headers["Last-Event-ID"] = final_cursor
        request = urllib.request.Request(args.url, headers=resume_headers, method="GET")
        with opener.open(request, timeout=60) as response:
            assert response.status == 204, "Completed cursor replayed a duplicate response"
        return body


    def send(method, params, request_id):
        nonlocal session_id
        payload = {"jsonrpc": "2.0", "method": method, "params": params}
        if request_id is not None:
            payload["id"] = request_id
        headers = {"Authorization": "Bearer " + token, "Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
        if session_id:
            headers["Mcp-Session-Id"] = session_id
        if protocol:
            headers["MCP-Protocol-Version"] = protocol
        request = urllib.request.Request(args.url, json.dumps(payload).encode(), headers)
        try:
            if method == "tools/call" and args.resume:
                body = resume_call(payload, headers)
            else:
                with opener.open(request, timeout=120) as response:
                    session_id = response.headers.get("Mcp-Session-Id", session_id)
                    if request_id is None:
                        assert response.status in (200, 202, 204)
                        return None
                    if response.headers.get_content_type() == "text/event-stream":
                        _, body = read_sse(response)
                    else:
                        raw = response.read(4 * 1024 * 1024 + 1)
                        assert len(raw) <= 4 * 1024 * 1024, "Response exceeds the acceptance bound"
                        body = json.loads(raw)
        except urllib.error.HTTPError as error:
            raise RuntimeError(f"MCP HTTP failure: {error.code}") from None
        assert body.get("id") == request_id, "JSON-RPC response identity mismatch"
        assert "error" not in body, "JSON-RPC failure"
        return body["result"]

    initialized = send("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "elitea-rust-acceptance", "version": "1"}}, 1)
    assert initialized.get("protocolVersion"), "No negotiated protocol version"
    protocol = initialized["protocolVersion"]
    send("notifications/initialized", {}, None)
    listed = send("tools/list", {}, 2)["tools"]
    names = [item["name"] for item in listed]
    assert len(names) == len(set(names)), "Duplicate exported tool names"
    selected = [item for item in listed if item["name"] == args.tool]
    if len(selected) != 1:
        print(json.dumps({"available_tools": names}))
        raise AssertionError("Exact tool is unavailable")
    assert selected[0]["inputSchema"].get("type") == "object", "Argument schema is missing"
    result = send("tools/call", {"name": args.tool, "arguments": arguments}, 3)
    assert not result.get("isError", False), "Tool failed or requires a non-autonomous continuation"
    texts = [item["text"] for item in result.get("content", []) if item.get("type") == "text"]
    assert any(args.expect in item for item in texts), "Expected synthetic marker is absent"
    print(json.dumps({"passed": True, "protocol": initialized["protocolVersion"], "tool": args.tool, "text_blocks": len(texts), "marker": args.expect, "resumed": args.resume, "main_restarted": args.restart_main}))


if __name__ == "__main__":
    main()
