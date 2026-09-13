"""Verify an autonomous Elitea MCP tool with an independently authenticated client.

Supply ELITEA_MCP_BEARER_TOKEN in the environment. Do not put credentials in argv.
This script never reads browser state or prints authentication material.
"""
import argparse
import json
import os
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
    args = parser.parse_args()
    url = urllib.parse.urlsplit(args.url)
    if url.scheme != "https" and not (url.scheme == "http" and url.hostname in ("localhost", "127.0.0.1")):
        parser.error("Use HTTPS, or HTTP on localhost for rehearsal")
    if url.username or url.password or url.query or url.fragment:
        parser.error("The MCP URL must not contain credentials, query parameters, or fragments")
    token = os.environ.get("ELITEA_MCP_BEARER_TOKEN")
    if not token:
        parser.error("Set ELITEA_MCP_BEARER_TOKEN through the approved credential mechanism")
    arguments = json.loads(args.arguments)
    if not isinstance(arguments, dict):
        parser.error("Arguments must be a JSON object")
    opener = urllib.request.build_opener(NoRedirect())
    session_id = None

    def send(method, params, request_id):
        nonlocal session_id
        payload = {"jsonrpc": "2.0", "method": method, "params": params}
        if request_id is not None:
            payload["id"] = request_id
        headers = {"Authorization": "Bearer " + token, "Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
        if session_id:
            headers["Mcp-Session-Id"] = session_id
        request = urllib.request.Request(args.url, json.dumps(payload).encode(), headers)
        try:
            with opener.open(request, timeout=120) as response:
                session_id = response.headers.get("Mcp-Session-Id", session_id)
                raw = response.read(4 * 1024 * 1024 + 1)
                assert len(raw) <= 4 * 1024 * 1024, "Response exceeds the acceptance bound"
                if request_id is None:
                    assert response.status in (200, 202, 204)
                    return None
                body = json.loads(raw)
        except urllib.error.HTTPError as error:
            raise RuntimeError(f"MCP HTTP failure: {error.code}") from None
        assert body.get("id") == request_id, "JSON-RPC response identity mismatch"
        assert "error" not in body, "JSON-RPC failure"
        return body["result"]

    initialized = send("initialize", {"protocolVersion": "2024-11-05", "capabilities": {}, "clientInfo": {"name": "elitea-rust-acceptance", "version": "1"}}, 1)
    assert initialized.get("protocolVersion"), "No negotiated protocol version"
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
    print(json.dumps({"passed": True, "protocol": initialized["protocolVersion"], "tool": args.tool, "text_blocks": len(texts), "marker": args.expect}))


if __name__ == "__main__":
    main()
