"""Check the crash-test delays hold a response back after the journal entry."""
import json
import os
import threading
import time
import unittest
import urllib.request
from http.server import ThreadingHTTPServer
from unittest import mock

import server
from server import Handler, _delay_seconds_from_env

DELAY = 0.4


class DelayParsingTest(unittest.TestCase):
    def parse(self, value):
        env = {} if value is None else {"MOCK_DELAY": value}
        with mock.patch.dict(os.environ, env, clear=False):
            if value is None:
                os.environ.pop("MOCK_DELAY", None)
            return _delay_seconds_from_env("MOCK_DELAY")

    def test_unset_empty_and_zero_mean_no_delay(self):
        for value in [None, "", "  ", "0"]:
            with self.subTest(value=value):
                self.assertEqual(self.parse(value), 0.0)

    def test_milliseconds_become_seconds(self):
        self.assertEqual(self.parse("1500"), 1.5)
        self.assertEqual(self.parse(str(server.MAX_DELAY_MS)), server.MAX_DELAY_MS / 1000.0)

    def test_bad_values_are_refused(self):
        for value in ["-1", "1.5", "abc", "1e3", str(server.MAX_DELAY_MS + 1)]:
            with self.subTest(value=value):
                with self.assertRaises(ValueError):
                    self.parse(value)

    def test_the_knobs_default_to_no_delay(self):
        self.assertEqual(server.TOOL_DELAY_SECONDS, 0.0)
        self.assertEqual(server.SLOW_FIRST_CHUNK_DELAY_SECONDS, 0.0)


class ServerTestCase(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
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
        self.request("DELETE", "/__journal")
        self.request("DELETE", "/tool/__journal")

    def request(self, method, path, body=None):
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(self.base + path, data=data, method=method,
                                     headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=10) as response:
            return response.status, response.read()

    def tool_journal(self):
        return json.loads(self.request("GET", "/tool/__journal")[1])["data"]

    def model_journal(self):
        return json.loads(self.request("GET", "/__journal")[1])["data"]


class ToolDelayTest(ServerTestCase):
    def call_in_background(self, method, path, body=None):
        result = {}

        def run():
            started = time.monotonic()
            result["status"] = self.request(method, path, body)[0]
            result["elapsed"] = time.monotonic() - started

        thread = threading.Thread(target=run)
        thread.start()
        return thread, result

    def test_effect_is_journaled_while_the_call_is_still_in_flight(self):
        for method, path, body in [("POST", "/tool/items", {"name": "x"}), ("GET", "/tool/status", None)]:
            with self.subTest(path=path):
                self.request("DELETE", "/tool/__journal")
                with mock.patch.object(server, "TOOL_DELAY_SECONDS", DELAY):
                    thread, result = self.call_in_background(method, path, body)
                    time.sleep(DELAY / 4)
                    self.assertEqual(result, {}, "the response must still be held back")
                    self.assertEqual([entry["path"] for entry in self.tool_journal()], [path])
                    thread.join()
                self.assertGreaterEqual(result["elapsed"], DELAY * 0.9)
                self.assertIn(result["status"], (200, 201))

    def test_no_delay_keeps_the_old_behaviour(self):
        started = time.monotonic()
        self.assertEqual(self.request("GET", "/tool/status")[0], 200)
        self.assertEqual(self.request("POST", "/tool/items", {})[0], 201)
        self.assertLess(time.monotonic() - started, DELAY)
        self.assertEqual(len(self.tool_journal()), 2)

    def test_reading_the_spec_is_never_delayed(self):
        with mock.patch.object(server, "TOOL_DELAY_SECONDS", DELAY):
            started = time.monotonic()
            self.request("GET", "/tool/openapi.json")
            self.assertLess(time.monotonic() - started, DELAY)


class SlowFirstChunkDelayTest(ServerTestCase):
    def chat(self, prompt, stream):
        payload = {"model": "m", "stream": stream,
                   "messages": [{"role": "user", "content": prompt}]}
        req = urllib.request.Request(self.base + "/v1/chat/completions",
                                     data=json.dumps(payload).encode(), method="POST",
                                     headers={"Content-Type": "application/json"})
        started = time.monotonic()
        with urllib.request.urlopen(req, timeout=30) as response:
            first_byte = time.monotonic() - started
            response.read()
        return first_byte

    def first_byte_in_background(self, prompt, stream):
        result = {}
        thread = threading.Thread(target=lambda: result.update(first=self.chat(prompt, stream)))
        thread.start()
        return thread, result

    def test_slow_mode_is_held_before_any_byte_but_after_the_journal_entry(self):
        for stream in (True, False):
            with self.subTest(stream=stream):
                self.request("DELETE", "/__journal")
                with mock.patch.object(server, "SLOW_FIRST_CHUNK_DELAY_SECONDS", DELAY), \
                        mock.patch.object(server, "SLOW_CHUNKS", 2), \
                        mock.patch.object(server, "SLOW_CHUNK_DELAY_SECONDS", 0.0):
                    thread, result = self.first_byte_in_background("[[mock:slow]] hi", stream)
                    time.sleep(DELAY / 4)
                    self.assertEqual(result, {}, "no response byte may arrive yet")
                    self.assertEqual([entry["mode"] for entry in self.model_journal()], ["slow"])
                    thread.join()
                self.assertGreaterEqual(result["first"], DELAY * 0.9)

    def test_other_modes_and_zero_delay_are_unaffected(self):
        with mock.patch.object(server, "SLOW_FIRST_CHUNK_DELAY_SECONDS", DELAY):
            for stream in (True, False):
                with self.subTest(stream=stream):
                    self.assertLess(self.chat("plain echo", stream), DELAY)
        with mock.patch.object(server, "SLOW_CHUNKS", 2), \
                mock.patch.object(server, "SLOW_CHUNK_DELAY_SECONDS", 0.0):
            self.assertLess(self.chat("[[mock:slow]] hi", True), DELAY)


if __name__ == "__main__":
    unittest.main()
