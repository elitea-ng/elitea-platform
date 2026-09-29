#!/usr/bin/env python3
"""Real Linux language-adapter checks; only fresh test-owned containers are removed."""
import argparse
import json
from pathlib import Path
import secrets
import subprocess
import tempfile

parser = argparse.ArgumentParser()
parser.add_argument("--image", required=True, help="already built immutable local image ID")
args = parser.parse_args()
if not args.image.startswith("sha256:"):
    parser.error("use a local immutable sha256 image ID")

cases = [
    ("python", "python", "import asyncio\nawait asyncio.sleep(0)\nprint('diagnostic')\n{'answer': elitea_state['count'] + 1}", 15, "completed"),
    ("javascript", "javascript", "console.log('diagnostic'); export default async state => ({answer: state.count + 1});", 15, "completed"),
    ("typescript", "typescript", "interface R { answer: number }; export default async (state: {count: number}): Promise<R> => ({answer: state.count + 1});", 15, "completed"),
    ("python_error", "python", "raise ValueError('expected-code-error')", 15, "failed"),
    ("network_denied", "javascript", "await fetch('https://example.com'); export default null;", 15, "failed"),
    ("process_denied", "javascript", "await new Deno.Command('/bin/sh').output(); export default null;", 15, "failed"),
    ("timeout", "javascript", "while (true) {} export default null;", 1, "timeout"),
]

for name, language, source, timeout, expected in cases:
    container = "elitea-code-runner-test-" + secrets.token_hex(6)
    with tempfile.TemporaryDirectory(prefix="elitea-code-runner-") as directory:
        request = Path(directory) / "job.json"
        request.write_text(json.dumps({"argv": ["/usr/local/bin/elitea-code-execute", "/workspace/.elitea-code.json"], "timeout_seconds": timeout}))
        code_request = Path(directory) / "code.json"
        code_request.write_text(json.dumps({"revision": 1, "language": language, "source": source,
            "input": {"count": 41}, "image_digest": args.image, "policy_revision": "probe-v1", "timeout_seconds": timeout}))
        code_request.chmod(0o644)
        request.chmod(0o644)
        created = False
        try:
            subprocess.run([
                "docker", "create", "--pull=never", "--name", container,
                "--user", "10001:10001", "--memory", "512m", "--memory-swap", "512m",
                "--cpus", "1", "--pids-limit", "64", "--network", "none",
                "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
                "--tmpfs", "/workspace:rw,nosuid,nodev,size=256m,uid=10001,gid=10001,mode=0700",
                "--mount", f"type=bind,source={request},target=/workspace/.elitea-job.json,readonly",
                "--mount", f"type=bind,source={code_request},target=/workspace/.elitea-code.json,readonly",
                "--log-driver", "local", "--log-opt", "max-size=256m", "--log-opt", "max-file=1", "--log-opt", "compress=false",
                args.image,
            ], check=True, stdout=subprocess.DEVNULL, timeout=20)
            created = True
            subprocess.run(["docker", "start", container], check=True, stdout=subprocess.DEVNULL, timeout=10)
            # No attached exec stream or live log reader is needed for execution.
            code = subprocess.check_output(["docker", "wait", container], text=True, timeout=20).strip()
            assert code == "0", (name, code)
            first = subprocess.check_output(["docker", "logs", container], timeout=10)
            receipt = json.loads(first)
            assert receipt["revision"] == 1
            assert receipt["status"] == expected, (name, receipt)
            assert len(receipt["stdout"].encode()) + len(receipt["stderr"].encode()) <= 524288
            if expected == "completed":
                assert json.loads(receipt["stdout"]) == {"revision": 1, "result": {"answer": 42}}, receipt
            if name == "python_error":
                assert "expected-code-error" in receipt["stderr"], receipt
            if name in ("network_denied", "process_denied"):
                assert "NotCapable" in receipt["stderr"], receipt
            # A fresh client reads the same terminal envelope after execution.
            assert subprocess.check_output(["docker", "logs", container], timeout=10) == first
            state = json.loads(subprocess.check_output([
                "docker", "inspect", container, "--format", "{{json .State}}"
            ], timeout=10))
            assert not state["Running"] and state["Pid"] == 0
            print(f"{name}: passed")
        finally:
            if created:
                subprocess.run(["docker", "rm", "-f", container], check=True, stdout=subprocess.DEVNULL, timeout=20)
