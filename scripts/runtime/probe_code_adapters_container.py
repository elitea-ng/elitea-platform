#!/usr/bin/env python3
"""Real Linux language-adapter checks; only fresh test-owned containers are removed."""
import argparse
import json
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile

parser = argparse.ArgumentParser()
parser.add_argument("--image", required=True, help="already built immutable local image ID")
parser.add_argument("--runtime", choices=["deno", "rust"], default="deno")
parser.add_argument("--python-packages", action="store_true", help="verify the prepared idna/python-slugify test profile")
parser.add_argument("--javascript-packages", action="store_true", help="verify the prepared strip-ansi/slugify/csv-parse test profile")
parser.add_argument("--python-bundle", type=Path, help="prepared Python bundle for revision 2 delivery checks")
parser.add_argument("--bundle-digest", help="independently recorded SHA-256 of the Python bundle")
args = parser.parse_args()
if not args.image.startswith("sha256:"):
    parser.error("use a local immutable sha256 image ID")
if bool(args.python_bundle) != bool(args.bundle_digest):
    parser.error("provide both --python-bundle and --bundle-digest")
if args.python_bundle:
    if args.runtime != "deno" or len(args.bundle_digest) != 64 or any(c not in "0123456789abcdef" for c in args.bundle_digest):
        parser.error("Python bundles require the Deno runtime and a lowercase SHA-256 digest")

cases = [
    ("python", "python", "import asyncio\nawait asyncio.sleep(0)\nprint('diagnostic')\n{'answer': elitea_state['count'] + 1}", 15, "completed"),
    ("javascript", "javascript", "console.log('diagnostic'); export default async state => ({answer: state.count + 1});", 15, "completed"),
    ("typescript", "typescript", "interface R { answer: number }; export default async (state: {count: number}): Promise<R> => ({answer: state.count + 1});", 15, "completed"),
    ("python_error", "python", "raise ValueError('expected-code-error')", 15, "failed"),
    ("network_denied", "javascript", "await fetch('https://example.com'); export default null;", 15, "failed"),
    ("process_denied", "javascript", "await new Deno.Command('/bin/sh').output(); export default null;", 15, "failed"),
    ("timeout", "javascript", "while (true) {} export default null;", 1, "timeout"),
]

if args.python_packages:
    if args.runtime != "deno":
        parser.error("--python-packages requires the Deno runtime")
    cases += [
        ("python_import_dependency", "python", "from slugify import slugify\nassert slugify('Hello déjà vu') == 'hello-deja-vu'\n{'answer': 42}", 15, "completed"),
        ("python_inline_dependency", "python", "import micropip\nawait micropip.install('python-slugify==8.0.4')\nfrom slugify import slugify\nassert slugify('Hello déjà vu') == 'hello-deja-vu'\n{'answer': 42}", 15, "completed"),
        ("python_unprepared_version", "python", "import micropip\nawait micropip.install('python-slugify==0.0.0')", 15, "failed"),
    ]

if args.runtime == "rust":
    cases = [
        ("rust", "rust", 'pub fn run(state: serde_json::Value) -> Result<serde_json::Value, Box<dyn std::error::Error>> { println!("diagnostic"); Ok(serde_json::json!({"answer":state["count"].as_i64().unwrap()+1})) }', 15, "completed"),
        ("rust_compile_error", "rust", 'this is not valid Rust', 15, "failed"),
        ("rust_error", "rust", 'pub fn run(_: serde_json::Value) -> Result<serde_json::Value, Box<dyn std::error::Error>> { Err("expected-code-error".into()) }', 15, "failed"),
        ("rust_timeout", "rust", 'pub fn run(_: serde_json::Value) -> Result<serde_json::Value, Box<dyn std::error::Error>> { println!("entered-user-code"); loop { std::hint::spin_loop(); } }', 15, "timeout"),
    ]

if args.javascript_packages:
    if args.runtime != "deno":
        parser.error("--javascript-packages requires the Deno runtime")
    for language in ("javascript", "typescript"):
        cases.append((f"{language}_dependencies", language,
            "import stripAnsi from 'npm:strip-ansi@7.1.0'; "
            "import slugify from 'npm:slugify@1.6.6'; "
            "import { parse } from 'npm:csv-parse@5.6.0/sync'; "
            "const rows = parse('name,amount\\nHello World,2\\nSecond Row,3\\n', {columns:true}); "
            "if (stripAnsi('\\u001b[31mred\\u001b[39m') !== 'red' || slugify(rows[0].name, {lower:true}) !== 'hello-world' || rows.reduce((n,r) => n + Number(r.amount),0) !== 5) throw new Error('Npm state mismatch'); "
            "export default {answer:42};", 15, "completed"))
    cases.append(("javascript_unprepared_version", "javascript",
        "import slugify from 'npm:slugify@1.6.5'; export default slugify('A B');", 15, "failed"))

bundle_errors = {
    "python_bundle_wrong_root": "content changed after resolution",
    "python_bundle_missing": "No such file or directory",
    "python_bundle_missing_metadata": "No such file or directory",
    "python_bundle_modified_wheel": "wheel does not match the native lock",
    "python_bundle_modified_metadata": "content changed after resolution",
    "python_bundle_downgrade": "invalid revision or Python dependency identity",
    "python_bundle_other_language": "invalid revision or Python dependency identity",
}
if args.python_bundle:
    cases.append(("python_bundle", "python",
        "from humanize import intcomma\nassert intcomma(12345) == '12,345'\n{'answer': elitea_state['count'] + 1}", 15, "completed"))
    for name in bundle_errors:
        cases.append((name, "javascript" if name.endswith("other_language") else "python",
            "print('ENTERED_UNVERIFIED_CODE')\n{'answer': 42}", 15, "failed"))

for name, language, source, timeout, expected in cases:
    container = "elitea-code-runner-test-" + secrets.token_hex(6)
    with tempfile.TemporaryDirectory(prefix="elitea-code-runner-") as directory:
        request = Path(directory) / "job.json"
        request.write_text(json.dumps({"argv": ["/usr/local/bin/elitea-code-execute", "/workspace/.elitea-code.json"], "timeout_seconds": timeout}))
        code_request = Path(directory) / "code.json"
        code = {"revision": 1, "language": language, "source": source,
            "input": {"count": 41}, "image_digest": args.image, "policy_revision": "probe-v1", "timeout_seconds": timeout}
        bundle_mount = []
        if name.startswith("python_bundle"):
            code.update(revision=2, dependency_bundle_sha256=args.bundle_digest)
            if name == "python_bundle_wrong_root":
                code["dependency_bundle_sha256"] = "0" * 64
            if name == "python_bundle_downgrade":
                code["revision"] = 1
            if name != "python_bundle_missing":
                bundle_dir = Path(directory) / "wheels"
                bundle_dir.mkdir()
                metadata = json.loads((args.python_bundle / "elitea-python-bundle.json").read_text())
                names = [entry["name"] for entry in metadata["files"]] + ["elitea-python-bundle.json"]
                for file_name in names:
                    assert Path(file_name).name == file_name and not (args.python_bundle / file_name).is_symlink()
                    shutil.copyfile(args.python_bundle / file_name, bundle_dir / file_name)
                if name == "python_bundle_missing_metadata":
                    (bundle_dir / "elitea-python-bundle.json").unlink()
                if name == "python_bundle_modified_wheel":
                    wheel = next(bundle_dir.glob("humanize-*.whl"))
                    data = bytearray(wheel.read_bytes())
                    data[0] ^= 1
                    wheel.write_bytes(data)
                if name == "python_bundle_modified_metadata":
                    metadata["requirements"] = ["humanize==0.0.0"]
                    (bundle_dir / "elitea-python-bundle.json").write_text(json.dumps(metadata))
                for entry in bundle_dir.iterdir():
                    entry.chmod(0o444)
                bundle_mount = ["--mount", f"type=bind,source={bundle_dir},target=/workspace/wheels,readonly"]
        code_request.write_text(json.dumps(code))
        code_request.chmod(0o644)
        request.chmod(0o644)
        created = False
        try:
            subprocess.run([
                "docker", "create", "--pull=never", "--name", container,
                "--user", "10001:10001", "--memory", "512m", "--memory-swap", "512m",
                "--cpus", "1", "--pids-limit", "64", "--network", "none",
                "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
                "--tmpfs", f"/workspace:rw,{'exec' if args.runtime == 'rust' else 'noexec'},nosuid,nodev,size=256m,uid=10001,gid=10001,mode=0700",
                "--mount", f"type=bind,source={request},target=/workspace/.elitea-job.json,readonly",
                "--mount", f"type=bind,source={code_request},target=/workspace/.elitea-code.json,readonly",
                *bundle_mount,
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
            if name in ("python_error", "rust_error"):
                assert "expected-code-error" in receipt["stderr"], receipt
            if name in ("network_denied", "process_denied"):
                assert "NotCapable" in receipt["stderr"], receipt
            if name == "rust_compile_error":
                assert "error:" in receipt["stderr"], receipt
            if name == "rust_timeout":
                assert "entered-user-code" in receipt["stderr"], receipt
            if name in bundle_errors:
                assert bundle_errors[name] in receipt["stderr"], receipt
                assert "ENTERED_UNVERIFIED_CODE" not in receipt["stderr"] + receipt["stdout"], receipt
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
