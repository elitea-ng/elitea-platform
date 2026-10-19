#!/usr/bin/env python3
"""Code runtime material for the standalone compose stack (PLAN §8.3: on by default).

The standalone stack runs agent node recovery, model-checkpoint recovery, Main's
Code owner recovery and the Code sandbox with dependency preparation for every
language by default. This script produces the material those need, from the
runtime CA that `gen-runtime-certs.sh` already minted. It is called by
`deploy/scripts/standalone-stack.sh certs` (the `certs` step) and `up` (the
`render` step); nothing else is required.

  certs    issue the sandbox leaves (gen-sandbox-certs.sh --native), Main's Code owner
           client certificate and the platform content key. Idempotent: existing valid
           material is kept, never rotated.
  render   write the seven supervisor profiles and the worker runtime JSON, bound to the
           runner images' digests, under deploy/certs/code/. Run after the runner images
           exist (the digests are their image ids).

Standard library only. Private files are 0600 in 0700 directories; nothing secret is
printed. The profile table is docs-mirrored in deploy/README.md ("Code runtime").
"""
import argparse
import base64
import json
import os
import pathlib
import re
import secrets
import shutil
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
OWNER_IDENTITY = "dns:elitea-code-owner-main"
CONTENT_ORIGIN = "https://elitea-main:9445"
# Compose-default database credentials (not a secret), TLS with full verification.
AGENTSTATE_URL = "postgres://elitea:elitea@postgres:5432/agentstate?sslmode=verify-full"
MIB = 1024 * 1024

# profile -> (port, audience, purpose, languages, policy, runner, timeout, memory, native workspace)
PROFILES = {
    "deno": (9446, "elitea-sandbox-deno", "execution", ["python"], "deno-v1", "deno", 60, 512 * MIB, None),
    "rust": (9447, "elitea-sandbox-rust", "execution", ["rust"], "rust-v1", "rust", 180, 2048 * MIB, 1024 * MIB),
    "denonative": (9450, "elitea-sandbox-deno-native", "execution", ["javascript", "typescript"], "deno-v1",
                   "deno", 60, 1024 * MIB, 512 * MIB),
    "prep": (9448, "elitea-sandbox-preparation", "preparation", ["python"], "python-preparation-v1", "deno",
             120, 512 * MIB, None),
    "jsprep": (9449, "elitea-sandbox-javascript-preparation", "preparation", ["javascript"],
               "deno-preparation-v1", "deno", 120, 1024 * MIB, 512 * MIB),
    "tsprep": (9451, "elitea-sandbox-typescript-preparation", "preparation", ["typescript"],
               "deno-preparation-v1", "deno", 120, 1024 * MIB, 512 * MIB),
    "rsprep": (9452, "elitea-sandbox-rust-preparation", "preparation", ["rust"], "cargo-preparation-v1",
               "rust", 300, 2048 * MIB, 1024 * MIB),
}
# Execution profiles Main's original-Code owner recovery reaches.
OWNER_SUPERVISORS = ("deno", "denonative", "rust")
# Worker language -> (execution profile, preparation profile, platform client)
LANGUAGES = {
    "python": ("deno", "prep", True),
    "javascript": ("denonative", "jsprep", True),
    "typescript": ("denonative", "tsprep", True),
    "rust": ("rust", "rsprep", False),
}
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")


def fail(message):
    raise SystemExit(f"code-material: {message}")


def run(cmd):
    proc = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if proc.returncode != 0:
        fail(f"{pathlib.Path(cmd[0]).name} failed: {proc.stderr.strip()[-300:]}")
    return proc.stdout


def private_dir(path):
    path.mkdir(parents=True, exist_ok=True)
    os.chmod(path, 0o700)
    return path


def write(path, text, mode=0o600):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, mode)
    with os.fdopen(fd, "w") as handle:
        handle.write(text)
    os.chmod(path, mode)


def copy(src, dst, mode=0o600):
    if src.is_symlink() or not src.is_file():
        fail(f"material source {src.name} must be a regular file")
    shutil.copyfile(src, dst)
    os.chmod(dst, mode)


def owner_certificate_valid(runtime, crt, key):
    if not (crt.is_file() and key.is_file()):
        return False
    checks = [
        ["openssl", "verify", "-purpose", "sslclient", "-CAfile", str(runtime / "runtime-ca.crt"), str(crt)],
        ["openssl", "x509", "-in", str(crt), "-checkend", "86400", "-noout"],
    ]
    if any(subprocess.run(c, capture_output=True, check=False).returncode for c in checks):
        return False
    public = subprocess.run(["openssl", "x509", "-in", str(crt), "-pubkey", "-noout"], capture_output=True, check=False)
    private = subprocess.run(["openssl", "pkey", "-in", str(key), "-pubout"], capture_output=True, check=False)
    return bool(public.stdout) and public.stdout == private.stdout


def issue_owner_certificate(runtime):
    crt, key = runtime / "code-owner-client.crt", runtime / "code-owner-client.key"
    if owner_certificate_valid(runtime, crt, key):
        return
    scratch = private_dir(runtime / ".code-owner-issue")
    try:
        ext = scratch / "extensions"
        ext.write_text("basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\n"
                       "extendedKeyUsage=clientAuth\nsubjectAltName=DNS:elitea-code-owner-main\n")
        run(["openssl", "req", "-new", "-newkey", "rsa:2048", "-nodes", "-keyout", str(scratch / "key"),
             "-out", str(scratch / "request"), "-subj", "/CN=elitea-code-owner-main"])
        run(["openssl", "x509", "-req", "-in", str(scratch / "request"), "-CA", str(runtime / "runtime-ca.crt"),
             "-CAkey", str(runtime / "runtime-ca.key"), "-set_serial", "0x" + secrets.token_hex(16),
             "-days", "30", "-sha256", "-extfile", str(ext), "-out", str(scratch / "certificate")])
        os.chmod(scratch / "key", 0o600)
        os.replace(scratch / "key", key)
        os.chmod(scratch / "certificate", 0o644)
        os.replace(scratch / "certificate", crt)
    finally:
        shutil.rmtree(scratch, ignore_errors=True)


def cmd_certs(args):
    certs = pathlib.Path(args.certs)
    runtime = certs / "runtime"
    for need in ("runtime-ca.crt", "runtime-ca.key", "command-signing-keyring.json", "agent-checkpoint-connection"):
        if not (runtime / need).is_file():
            fail(f"{runtime / need} missing; run deploy/scripts/gen-runtime-certs.sh first")
    run(["bash", str(ROOT / "deploy/scripts/gen-sandbox-certs.sh"), str(runtime), str(certs / "sandbox"), "--native"])
    issue_owner_certificate(runtime)
    keys = runtime / "code-platform-content-keys.json"
    if not keys.is_file():
        kid = "standalone-k1"
        raw = base64.urlsafe_b64encode(secrets.token_bytes(32)).decode().rstrip("=")
        write(keys, json.dumps({"revision": 1, "current_key_id": kid,
                                "keys": [{"id": kid, "key_base64url": raw}]}, indent=2) + "\n")
    print("Code runtime certificates ready (sandbox leaves, owner identity, platform content key).")


def supervisor_config(profile, owner_prefix, digests, native_platform, network):
    port, audience, purpose, languages, policy, runner, timeout, memory, workspace = PROFILES[profile]
    base = f"/run/elitea-sandbox/{profile}"
    cfg = {
        "revision": 1, "purpose": purpose, "listen_address": f"0.0.0.0:{port}",
        "owner": f"{owner_prefix}-sandbox-{profile}-1", "audience": f"dns:{audience}",
        "ca_path": f"{base}/client-ca.pem", "certificate_path": f"{base}/server.pem",
        "private_key_path": f"{base}/server.key", "verification_keyring_path": f"{base}/command-signing-keyring.json",
        "database_url_path": f"{base}/database-url", "database_ca_path": f"{base}/database-ca.pem",
        "database_connections": 4 if purpose == "execution" else 2,
        "image_digest": digests[runner], "policy_revision": policy, "languages": languages,
        "concurrency": 4 if purpose == "execution" else 1, "memory_bytes": memory, "cpu_limit": 1.0,
        "timeout_seconds": timeout,
        "dependency_content": {"origin": CONTENT_ORIGIN, "ca_path": f"{base}/content-ca.pem",
                               "certificate_path": f"{base}/content-client.pem",
                               "private_key_path": f"{base}/content-client.key",
                               "staging_root": f"/run/elitea-sandbox-content/{profile}",
                               "capacity": 1, "timeout_seconds": 30},
    }
    if workspace is not None:
        cfg["native_platform"] = native_platform
        cfg["native_workspace_bytes"] = workspace
    if purpose == "execution":
        cfg["code_owner_requester"] = OWNER_IDENTITY
        if profile != "rust":
            cfg["code_platform_profile"] = True
    else:
        cfg["backend"] = {"kind": "docker"}
        cfg["preparation_network"] = network
    return cfg


def sandbox_runtimes(digests, native_platform):
    entries = []
    for language, (execution, preparation, platform) in LANGUAGES.items():
        port, audience, _, _, policy, runner, timeout, _, _ = PROFILES[execution]
        target = f"{audience}:{port}"
        entry = {"language": language, "target": target, "audience": f"dns:{audience}",
                 "image_digest": digests[runner], "policy_revision": policy, "timeout_seconds": timeout}
        if platform:
            entry["platform_client"] = {"target": target, "audience": f"dns:{audience}",
                                        "image_digest": digests[runner], "policy_revision": policy,
                                        "timeout_seconds": timeout, "max_calls": 32, "max_total_bytes": 1048576}
        p_port, p_audience, _, _, p_policy, p_runner, p_timeout, _, p_workspace = PROFILES[preparation]
        prep = {"target": f"{p_audience}:{p_port}", "audience": f"dns:{p_audience}",
                "image_digest": digests[p_runner], "policy_revision": p_policy, "timeout_seconds": p_timeout}
        if p_workspace is not None:
            prep = {"native_platform": native_platform, **prep}
        entry["preparation"] = prep
        entries.append(entry)
    return entries


def cmd_render(args):
    certs = pathlib.Path(args.certs)
    runtime, issued = certs / "runtime", certs / "sandbox"
    out = private_dir(pathlib.Path(args.out))
    digests = {"deno": args.deno_digest, "rust": args.rust_digest}
    for name, value in digests.items():
        if not DIGEST.fullmatch(value):
            fail(f"--{name}-digest must be sha256:<64 hex> (the runner image id)")
    if args.arch not in ("arm64", "amd64"):
        fail("--arch must be arm64 or amd64")
    if not re.fullmatch(r"[a-z0-9][a-z0-9_.-]{0,62}", args.owner_prefix):
        fail("--owner-prefix must be a compose project name")
    native_platform = {"os": "linux", "arch": args.arch, "abi": "gnu"}
    for need in ("runtime-ca.crt", "command-signing-keyring.json", "code-owner-client.crt", "code-owner-client.key",
                 "code-platform-content-keys.json"):
        if not (runtime / need).is_file():
            fail(f"{runtime / need} missing; run: deploy/scripts/standalone-stack.sh certs")
    pg = private_dir(out / "pg")
    copy(issued / "postgres.crt", pg / "server.crt", 0o644)
    copy(issued / "postgres.key", pg / "server.key")
    sandbox = private_dir(out / "sandbox")
    for profile, (_, audience, _, _, _, _, _, _, _) in PROFILES.items():
        target = private_dir(sandbox / profile)
        copy(runtime / "runtime-ca.crt", target / "client-ca.pem")
        copy(runtime / "runtime-ca.crt", target / "database-ca.pem")
        copy(runtime / "runtime-ca.crt", target / "content-ca.pem")
        copy(runtime / "command-signing-keyring.json", target / "command-signing-keyring.json")
        copy(issued / f"{audience}.crt", target / "server.pem")
        copy(issued / f"{audience}.key", target / "server.key")
        copy(issued / f"{audience}-content-client.crt", target / "content-client.pem")
        copy(issued / f"{audience}-content-client.key", target / "content-client.key")
        write(target / "database-url", AGENTSTATE_URL)
        config = supervisor_config(profile, args.owner_prefix, digests, native_platform, args.network)
        write(target / "config.json", json.dumps(config, indent=2) + "\n")
    worker = json.loads((ROOT / "deploy/runtime/worker-runtime.rust.json").read_text())
    worker["sandbox_runtimes"] = sandbox_runtimes(digests, native_platform)
    write(out / "worker-runtime.json", json.dumps(worker, indent=2) + "\n", 0o644)
    print(f"Code runtime material rendered: 7 supervisor profiles, 4 languages with preparation "
          f"(deno={digests['deno'][:19]} rust={digests['rust'][:19]} arch={args.arch}).")


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="command", required=True)
    certs = sub.add_parser("certs")
    certs.add_argument("--certs", default=str(ROOT / "deploy/certs"))
    render = sub.add_parser("render")
    render.add_argument("--certs", default=str(ROOT / "deploy/certs"))
    render.add_argument("--out", default=str(ROOT / "deploy/certs/code"))
    render.add_argument("--deno-digest", required=True)
    render.add_argument("--rust-digest", required=True)
    render.add_argument("--arch", required=True)
    render.add_argument("--network", required=True)
    render.add_argument("--owner-prefix", required=True)
    args = parser.parse_args(argv)
    {"certs": cmd_certs, "render": cmd_render}[args.command](args)


if __name__ == "__main__":
    main(sys.argv[1:])
