"""Check the request journal and its /__journal routes."""
import json
import threading
import unittest
import urllib.error
import urllib.request
from unittest import mock

import server
from server import Handler, _dispatch
from http.server import ThreadingHTTPServer


def call(name, text):
    return {"jsonrpc": "2.0", "id": 1, "method": "tools/call",
            "params": {"name": name, "arguments": {"text": text}}}


class JournalFunctionTest(unittest.TestCase):
    def setUp(self):
        server._clear_journal()

    def test_every_method_is_recorded_including_notifications(self):
        _dispatch({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}})
        _dispatch({"jsonrpc": "2.0", "method": "notifications/initialized"})
        _dispatch({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
        entries = server._journal_entries()
        self.assertEqual([e["method"] for e in entries],
                         ["initialize", "notifications/initialized", "tools/list"])
        for entry in entries:
            self.assertEqual(set(entry), {"method", "tool", "marker", "at"})
            self.assertIsNone(entry["tool"])
            self.assertIsNone(entry["marker"])
            self.assertIsInstance(entry["at"], float)

    def test_tools_call_records_the_tool_and_a_bounded_marker(self):
        _dispatch(call("echo", "x" * 200))
        _dispatch(call("reverse", "short"))
        first, second = server._journal_entries()
        self.assertEqual((first["tool"], first["marker"]), ("echo", "x" * 64))
        self.assertEqual((second["tool"], second["marker"]), ("reverse", "short"))

    def test_tools_call_without_a_text_string_has_a_null_marker(self):
        for params in [{"name": "echo"}, {"name": "echo", "arguments": {"text": 5}},
                       {"name": "echo", "arguments": "nope"}]:
            with self.subTest(params=params):
                server._clear_journal()
                _dispatch({"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
                (entry,) = server._journal_entries()
                self.assertEqual(entry["tool"], "echo")
                self.assertIsNone(entry["marker"])

    def test_journal_is_bounded_and_drops_the_oldest(self):
        with mock.patch.object(server, "MAX_JOURNAL_ENTRIES", 3):
            for index in range(5):
                _dispatch(call("echo", f"m{index}"))
        self.assertEqual([e["marker"] for e in server._journal_entries()], ["m2", "m3", "m4"])

    def test_clear_empties_the_journal(self):
        _dispatch(call("echo", "a"))
        server._clear_journal()
        self.assertEqual(server._journal_entries(), [])

    def test_replies_are_unchanged_by_journaling(self):
        reply = _dispatch(call("echo", "hello"))
        self.assertEqual(reply["result"]["content"][0]["text"], "hello")
        self.assertIsNone(_dispatch({"jsonrpc": "2.0", "method": "notifications/initialized"}))


class JournalRouteTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # Plain HTTP: the handler does not care that production wraps the socket in TLS.
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever)
        cls.thread.start()
        cls.base = f"http://127.0.0.1:{cls.server.server_address[1]}"

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join()

    def setUp(self):
        server._clear_journal()

    def request(self, method, path, body=None):
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(self.base + path, data=data, method=method,
                                     headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=10) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.read()

    def test_post_then_get_then_delete_the_journal(self):
        self.assertEqual(self.request("POST", "/mcp", call("echo", "over http"))[0], 200)
        status, raw = self.request("GET", "/__journal")
        self.assertEqual(status, 200)
        body = json.loads(raw)
        self.assertEqual(body["object"], "list")
        self.assertEqual(body["count"], 1)
        self.assertEqual((body["data"][0]["tool"], body["data"][0]["marker"]), ("echo", "over http"))

        status, raw = self.request("DELETE", "/__journal")
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(raw), {"object": "list", "data": [], "count": 0})
        self.assertEqual(json.loads(self.request("GET", "/__journal")[1])["count"], 0)

    def test_reading_the_journal_does_not_journal_itself(self):
        self.request("GET", "/__journal")
        self.assertEqual(server._journal_entries(), [])

    def test_mcp_endpoint_behaviour_is_unchanged(self):
        self.assertEqual(self.request("GET", "/mcp")[0], 405)
        self.assertEqual(self.request("DELETE", "/mcp")[0], 405)
        self.assertEqual(self.request("GET", "/nope")[0], 404)
        self.assertEqual(self.request("GET", "/healthz")[0], 200)


if __name__ == "__main__":
    unittest.main()
