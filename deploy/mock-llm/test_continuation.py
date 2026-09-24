"""Check the fixture's actual SSE boundary and terminal reason."""
import json
import threading
import unittest
import urllib.request
from http.server import ThreadingHTTPServer

from server import Handler


class ContinuationFixtureTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join()

    def stream(self, messages):
        request = urllib.request.Request(
            f"http://127.0.0.1:{self.server.server_port}/v1/chat/completions",
            data=json.dumps({"model": "fixture", "messages": messages, "stream": True}).encode(),
            headers={"Content-Type": "application/json"},
        )
        with urllib.request.urlopen(request, timeout=5) as response:
            lines = response.read().decode().splitlines()
        events = [json.loads(line[6:]) for line in lines if line.startswith("data: ") and line != "data: [DONE]"]
        text = "".join(event["choices"][0]["delta"].get("content", "") for event in events)
        return text, events[-1]["choices"][0]["finish_reason"]

    def test_bad_boundary_then_exact_repair_and_independent_requests(self):
        original = [{"role": "user", "content": "[[mock:continuation_repair]]"}]
        prefix, reason = self.stream(original)
        self.assertEqual(reason, "length")
        self.assertTrue(prefix.endswith("RECORD 012: accepted fixture output."))
        anchor = prefix[-256:]
        prompt = "The previous answer reached its output allowance. Exact anchor: " + json.dumps(anchor)
        bad, reason = self.stream(original + [{"role": "user", "content": prompt}])
        self.assertEqual(bad, "REJECTED_BOUNDARY_MUST_NOT_APPEAR")
        self.assertEqual(reason, "stop")
        repaired, reason = self.stream(original + [{"role": "user", "content": prompt + " The previous continuation was rejected"}])
        self.assertEqual(repaired, anchor + "\nREPAIR_COMPLETE")
        self.assertEqual(reason, "stop")
        ordinary, reason = self.stream([{"role": "user", "content": "unrelated request"}])
        self.assertEqual(ordinary, "MOCK: unrelated request ")
        self.assertEqual(reason, "stop")


if __name__ == "__main__":
    unittest.main()
