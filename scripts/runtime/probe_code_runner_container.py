#!/usr/bin/env python3
"""Real Linux PID-1 runner checks; only fresh test-owned containers are removed."""
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
    ("success", ["python", "-c", "import os; assert os.getuid()==10001; print('result')"], 5, "completed"),
    ("failure", ["sh", "-c", "printf diagnostic >&2; exit 7"], 5, "failed"),
    ("overflow", ["python", "-c", "print('x'*600000)"], 5, "output_limit"),
    ("timeout", ["sh", "-c", "sleep 30 & wait"], 1, "timeout"),
    ("inherited_pipe", ["sh", "-c", "sleep 30 & exit 0"], 1, "timeout"),
]

for name, argv, timeout, expected in cases:
    container = "elitea-code-runner-test-" + secrets.token_hex(6)
    with tempfile.TemporaryDirectory(prefix="elitea-code-runner-") as directory:
        request = Path(directory) / "job.json"
        request.write_text(json.dumps({"argv": argv, "timeout_seconds": timeout}))
        request.chmod(0o644)
        created = False
        try:
            subprocess.run([
                "docker", "create", "--pull=never", "--name", container,
                "--user", "10001:10001", "--memory", "64m", "--memory-swap", "64m",
                "--cpus", "0.25", "--pids-limit", "64", "--network", "none",
                "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
                "--tmpfs", "/workspace:rw,nosuid,nodev,size=8m,uid=10001,gid=10001,mode=0700",
                "--mount", f"type=bind,source={request},target=/workspace/.elitea-job.json,readonly",
                "--log-driver", "local", "--log-opt", "max-size=8m", "--log-opt", "max-file=1", "--log-opt", "compress=false",
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
            assert receipt["status"] == expected, (name, receipt["status"])
            assert len(receipt["stdout"].encode()) + len(receipt["stderr"].encode()) <= 524288
            if name == "success":
                assert receipt["stdout"] == "result\n"
            if name == "failure":
                assert receipt["exit_code"] == 7 and receipt["stderr"] == "diagnostic"
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
