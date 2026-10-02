"""Check the fixture's actual SSE boundary and terminal reason."""
import json
import threading
import unittest
import urllib.request
import urllib.error
from http.server import ThreadingHTTPServer

from server import Handler, _script_for


class ToolCallFixtureTest(unittest.TestCase):
    def setUp(self):
        self.prompt = '[[mock:call_tool elitea_agent_4_v_5 {"task":"review"}]] hand it over'
        self.user = {"role": "user", "content": self.prompt}
        self.call = _script_for([self.user]).tool_calls[0]
        self.notice = {"role": "tool", "tool_call_id": "elitea-skipped-internal-tools",
                       "content": "internal tool 'swarm' is not available on this worker"}

    def test_unrelated_swarm_notice_does_not_complete_the_requested_call(self):
        for notice in [self.notice, {"role": "tool", "content": self.notice["content"]}]:
            with self.subTest(notice=notice):
                script = _script_for([self.user, notice])
                self.assertEqual(script.mode, "call_tool")
                self.assertEqual(script.tool_calls, [self.call])
                self.assertEqual(script.reply, "")

    def test_matching_completion_preserves_success_denial_and_error_results(self):
        for result in ["ROUTE_TAG_REJECTED review", '{"type":"sensitive_tool_blocked"}', '{"error":"refused"}']:
            with self.subTest(result=result):
                completed = {"role": "tool", "tool_call_id": self.call["id"], "content": result}
                script = _script_for([self.user, self.notice, completed])
                self.assertEqual(script.mode, "call_tool_resumed")
                self.assertIsNone(script.tool_calls)
                self.assertIn(result, script.reply)
                self.assertNotIn("swarm", script.reply)
                self.assertTrue(script.reply.endswith("MOCKCALLTOOLEND"))

    def test_legacy_function_results_match_the_requested_name(self):
        completed = {"role": "function", "name": "elitea_agent_4_v_5", "content": "legacy completion"}
        self.assertEqual(_script_for([self.user, completed]).mode, "call_tool_resumed")
        unrelated = {**completed, "name": "unrelated_function"}
        self.assertEqual(_script_for([self.user, unrelated]).mode, "call_tool")
        wrong_id = {**completed, "tool_call_id": "unrelated-call"}
        self.assertEqual(_script_for([self.user, wrong_id]).mode, "call_tool")

    def test_replayed_call_with_reminted_id_completes_the_requested_call(self):
        # The SDK's HITL resume replays the paused assistant message under a
        # fresh call id; the pair still answers the scripted call.
        replayed = {"role": "assistant", "content": "", "tool_calls": [
            {**self.call, "id": "call_reminted"}]}
        completed = {"role": "tool", "tool_call_id": "call_reminted", "content": "approved result"}
        script = _script_for([self.user, self.notice, replayed, completed])
        self.assertEqual(script.mode, "call_tool_resumed")
        self.assertIn("approved result", script.reply)
        self.assertNotIn("swarm", script.reply)
        unscripted = {"role": "assistant", "content": "", "tool_calls": [{
            **self.call, "id": "call_reminted",
            "function": {"name": "unrelated_function", "arguments": "{}"}}]}
        self.assertEqual(_script_for([self.user, unscripted, completed]).mode, "call_tool")

    def test_old_turn_result_and_repeated_operation_identities_remain_distinct(self):
        completed = {"role": "tool", "tool_call_id": self.call["id"], "content": "old completion"}
        self.assertEqual(_script_for([self.user, completed, self.user]).mode, "call_tool")
        prompt = "[[mock:call_tool same_operation]] [[mock:call_tool same_operation]]"
        user = {"role": "user", "content": prompt}
        calls = _script_for([user]).tool_calls
        self.assertNotEqual(calls[0]["id"], calls[1]["id"])
        results = [{"role": "tool", "tool_call_id": call["id"], "content": str(index)}
                   for index, call in enumerate(calls)]
        script = _script_for([user, self.notice, *results])
        self.assertEqual(script.mode, "call_tool_resumed")
        self.assertIn("tool result 1 said 0 tool result 2 said 1", script.reply)
        self.assertNotIn("swarm", script.reply)


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

    def test_request_growth_prefix_is_bounded_and_output_limited(self):
        text, reason = self.stream([{"role": "user", "content": "[[mock:request_growth]]"}])
        self.assertEqual(len(text.encode()), 512 * 1024)
        self.assertTrue(text.startswith("BYTE_GROWTH_PARTIAL\n"))
        self.assertEqual(reason, "length")

    def test_cached_usage_is_complete_and_request_scoped(self):
        for streaming in [False, True]:
            for prompt in ["[[mock:cached_usage]]", "healthy"]:
                with self.subTest(streaming=streaming, prompt=prompt):
                    request = urllib.request.Request(
                        f"http://127.0.0.1:{self.server.server_port}/v1/chat/completions",
                        data=json.dumps({"model": "fixture", "stream": streaming,
                                         "messages": [{"role": "user", "content": prompt}]}).encode(),
                        headers={"Content-Type": "application/json"},
                    )
                    with urllib.request.urlopen(request, timeout=5) as response:
                        raw = response.read().decode()
                    if streaming:
                        events = [json.loads(line[6:]) for line in raw.splitlines()
                                  if line.startswith("data: ") and line != "data: [DONE]"]
                        usages = [event["usage"] for event in events if event.get("usage")]
                        self.assertEqual(len(usages), 1)
                        usage = usages[0]
                    else:
                        usage = json.loads(raw)["usage"]
                    if prompt == "healthy":
                        self.assertEqual(usage["prompt_tokens"], 1)
                        self.assertNotIn("prompt_tokens_details", usage)
                    else:
                        self.assertEqual(usage, {
                            "prompt_tokens": 10000, "completion_tokens": 23, "total_tokens": 10023,
                            "prompt_tokens_details": {"cached_tokens": 8000},
                            "completion_tokens_details": {"reasoning_tokens": 7},
                        })

    def test_large_tool_input_is_generated_from_short_prompt(self):
        request = urllib.request.Request(
            f"http://127.0.0.1:{self.server.server_port}/v1/chat/completions",
            data=json.dumps({"model": "fixture", "stream": True,
                             "messages": [{"role": "user", "content": "[[mock:large_tool_input]]"}]}).encode(),
            headers={"Content-Type": "application/json"},
        )
        with urllib.request.urlopen(request, timeout=5) as response:
            lines = response.read().decode().splitlines()
        events = [json.loads(line[6:]) for line in lines if line.startswith("data: ") and line != "data: [DONE]"]
        calls = [call for event in events for call in event["choices"][0]["delta"].get("tool_calls", [])]
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0]["function"]["name"], "lookup_record")
        self.assertEqual(len(json.loads(calls[0]["function"]["arguments"])["probe"]), 48000)
        self.assertEqual(events[-1]["choices"][0]["finish_reason"], "tool_calls")

    def test_provider_errors_are_http_failures_and_do_not_leak_into_next_request(self):
        for status, category in ((400, "invalid_request_error"),
                                 (401, "authentication_error"),
                                 (402, "insufficient_quota"),
                                 (429, "rate_limit_error"), (503, "server_error")):
            with self.subTest(status=status):
                with self.assertRaises(urllib.error.HTTPError) as caught:
                    self.stream([{"role": "user", "content": f"[[mock:http_{status}]]"}])
                with caught.exception as response:
                    self.assertEqual(response.code, status)
                    body = json.load(response)
                self.assertEqual(body["error"]["type"], category)
                self.assertEqual(body["error"]["message"], "SYNTHETIC_PROVIDER_BODY_MUST_NOT_REACH_UI")
        text, reason = self.stream([
            {"role": "user", "content": "[[mock:http_401]]"},
            {"role": "user", "content": "healthy request"},
        ])
        self.assertEqual(text, "MOCK: healthy request ")
        self.assertEqual(reason, "stop")

    def test_responses_error_fixture_requires_explicit_stream_marker(self):
        for prompt in ("[[mock:stream_error]]", "[[mock:wrong_model]]", "healthy"):
            request = urllib.request.Request(
                f"http://127.0.0.1:{self.server.server_port}/v1/responses",
                data=json.dumps({"model": "fixture", "stream": True,
                                 "input": [{"role": "user", "content": prompt}]}).encode(),
                headers={"Content-Type": "application/json"},
            )
            if prompt == "healthy":
                with self.assertRaises(urllib.error.HTTPError) as caught:
                    urllib.request.urlopen(request, timeout=5)
                caught.exception.close()
                self.assertEqual(caught.exception.code, 400)
                continue
            with urllib.request.urlopen(request, timeout=5) as response:
                events = [json.loads(line[6:]) for line in response.read().decode().splitlines() if line.startswith("data: ")]
            self.assertEqual(events[0]["type"], "response.created")
            expected_model = "unexpected-fixture-model" if prompt == "[[mock:wrong_model]]" else "vllm/fixture"
            self.assertEqual(events[0]["response"]["model"], expected_model)
            event = events[-1]
            self.assertEqual(event["type"], "response.failed")
            self.assertEqual(event["response"]["error"]["message"], "SYNTHETIC_PROVIDER_BODY_MUST_NOT_REACH_UI")

    def test_stream_error_follows_partial_output_without_success_terminal(self):
        request = urllib.request.Request(
            f"http://127.0.0.1:{self.server.server_port}/v1/chat/completions",
            data=json.dumps({"model": "fixture", "stream": True,
                             "messages": [{"role": "user", "content": "[[mock:stream_error]]"}]}).encode(),
            headers={"Content-Type": "application/json"},
        )
        with urllib.request.urlopen(request, timeout=5) as response:
            raw = response.read().decode()
        events = [json.loads(line[6:]) for line in raw.splitlines() if line.startswith("data: ")]
        self.assertEqual(events[-2]["choices"][0]["delta"]["content"], "VALID_PARTIAL_OUTPUT")
        self.assertEqual(events[-1]["error"]["message"], "SYNTHETIC_PROVIDER_BODY_MUST_NOT_REACH_UI")
        self.assertNotIn("[DONE]", raw)
        self.assertTrue(all(event["choices"][0]["finish_reason"] is None for event in events[:-1]))
        self.assertEqual(self.stream([{"role": "user", "content": "healthy"}]), ("MOCK: healthy ", "stop"))

    def test_incomplete_stream_preserves_partial_text_without_terminal_reason(self):
        text, reason = self.stream([{"role": "user", "content": "[[mock:incomplete_stream]]"}])
        self.assertEqual(text, "VALID_PARTIAL_OUTPUT")
        self.assertIsNone(reason)

    def test_exhaustion_replay_keeps_exact_prefix_and_is_transcript_scoped(self):
        original = [{"role": "user", "content": "[[mock:continuation_exhaust]]"}]
        accepted = ""
        for number in range(1, 6):
            messages = original
            if accepted:
                prompt = "The previous answer reached its output allowance. Exact anchor: " + json.dumps(accepted[-256:])
                messages = original + [{"role": "assistant", "content": accepted}, {"role": "user", "content": prompt}]
            fragment, reason = self.stream(messages)
            self.assertEqual(reason, "length")
            self.assertEqual(self.stream(messages), (fragment, reason))
            anchor = accepted[-256:]
            self.assertTrue(fragment.startswith(anchor))
            accepted += fragment[len(anchor):]
            self.assertEqual(accepted.count(f"EXHAUST_SEGMENT_{number}\n"), 1)
        self.assertLess(len(accepted), 120000)
        self.assertTrue(accepted.endswith("RECORD 5.0400: accepted incomplete fixture text.\n"))
        self.assertEqual(self.stream([{"role": "user", "content": "healthy"}]), ("MOCK: healthy ", "stop"))

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
