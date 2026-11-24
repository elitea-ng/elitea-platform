#!/usr/bin/env bash
# The NATS bootstrap issuer (deploy/helm/elitea/files/nats-bootstrap-issue.sh,
# #2911) against a fake Kubernetes API that behaves like cert-manager:
#
#   * a new Certificate reports Ready=False (reason DoesNotExist) first, then
#     Issuing=True, then Ready=True — the transient Ready=False that Argo CD's
#     Certificate health check read as Degraded, failing the sync. The issuer
#     must wait through it;
#   * a Secret deleted while its Certificate still exists is written straight
#     back (the retry that failed the old cleanup Job). The issuer must delete
#     a leftover Certificate, and see it gone, BEFORE it deletes the Secret;
#   * a restarted pod of the same Job finds its own Certificates and keeps them;
#   * a Certificate that never becomes Ready fails the run, naming its
#     conditions.
#
# Needs python3, sh, curl, jq and base64 (the nats-box image has all of them).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/../elitea/files/nats-bootstrap-issue.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
python3 - "$SCRIPT" "$TMP" <<'PY'
import base64, http.server, json, os, pathlib, subprocess, sys, threading

script, tmp = sys.argv[1], pathlib.Path(sys.argv[2])
NS, JOB, UID = "elitea", "elitea-nats-bootstrap", "job-uid-1"
ACCOUNTS = ["main", "gateway"]
CERT = lambda a: f"elitea-nats-bootstrap-{a}-nats-client"
SECRET = lambda a: f"elitea-nats-bootstrap-{a}-nats-client-tls"
results = []
def check(name, cond, detail=""):
    results.append((name, bool(cond), detail))

class API:
    """Certificates and Secrets, with cert-manager's ordering."""
    def __init__(self, certs=None, secrets=None, never_ready=()):
        self.certs = certs or {}        # name -> object (with "_gets" counter)
        self.secrets = secrets or {}    # name -> data
        self.never_ready = set(never_ready)
        self.log = []
        self.lock = threading.Lock()

    def cert_status(self, c):
        name = c["metadata"]["name"]
        c["_gets"] = c.get("_gets", 0) + 1
        if name in self.never_ready:
            return [{"type": "Ready", "status": "False", "reason": "DoesNotExist", "message": "Issuing certificate as Secret does not exist"}]
        if c.get("_ready"):
            return [{"type": "Ready", "status": "True", "reason": "Ready", "observedGeneration": 1}]
        n = c["_gets"]
        if n == 1:
            return [{"type": "Ready", "status": "False", "reason": "DoesNotExist", "message": "Issuing certificate as Secret does not exist", "observedGeneration": 1}]
        if n == 2:
            return [{"type": "Issuing", "status": "True", "reason": "DoesNotExist"},
                    {"type": "Ready", "status": "False", "reason": "DoesNotExist", "observedGeneration": 1}]
        # Issued: the Secret is written, then Ready=True.
        sec = c["spec"]["secretName"]
        self.secrets[sec] = {k: base64.b64encode(f"{name}/{k}".encode()).decode() for k in ("ca.crt", "tls.crt", "tls.key")}
        c["_ready"] = True
        return [{"type": "Ready", "status": "True", "reason": "Ready", "observedGeneration": 1}]

    def handle(self, method, path, body):
        with self.lock:
            parts = path.strip("/").split("/")
            self.log.append((method, path))
            if parts[:4] == ["api", "v1", "namespaces", NS] and parts[4] == "secrets":
                name = parts[5]
                if method == "GET":
                    return (200, {"data": self.secrets[name]}) if name in self.secrets else (404, {"message": f'secrets "{name}" not found'})
                if method == "DELETE":
                    if name not in self.secrets:
                        return 404, {"message": "not found"}
                    del self.secrets[name]
                    # cert-manager re-writes the Secret of a live Certificate.
                    if any(c["spec"]["secretName"] == name for c in self.certs.values()):
                        self.secrets[name] = {"tls.crt": base64.b64encode(b"rewritten").decode()}
                        self.log.append(("REWRITE", name))
                    return 200, {}
            if parts[:5] == ["apis", "cert-manager.io", "v1", "namespaces", NS] and parts[5] == "certificates":
                if method == "POST":
                    c = json.loads(body)
                    name = c["metadata"]["name"]
                    if name in self.certs:
                        return 409, {"message": f'certificates "{name}" already exists'}
                    c["metadata"]["generation"] = 1
                    self.certs[name] = c
                    return 201, c
                name = parts[6]
                if method == "GET":
                    if name not in self.certs:
                        return 404, {"message": "not found"}
                    c = self.certs[name]
                    out = {k: v for k, v in c.items() if not k.startswith("_")}
                    out["status"] = {"conditions": self.cert_status(c)}
                    return 200, out
                if method == "DELETE":
                    return (200, {}) if self.certs.pop(name, None) else (404, {"message": "not found"})
            return 400, {"message": f"unexpected {method} {path}"}

def serve(api):
    class H(http.server.BaseHTTPRequestHandler):
        def log_message(self, *a): pass
        def do(self):
            n = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(n) if n else b""
            check_auth = self.headers.get("Authorization") == "Bearer test-token"
            code, obj = api.handle(self.command, self.path, body) if check_auth else (401, {"message": "no token"})
            data = json.dumps(obj).encode()
            self.send_response(code); self.send_header("Content-Length", str(len(data))); self.end_headers(); self.wfile.write(data)
        do_GET = do_POST = do_DELETE = do
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv

# A curl shim: the script talks to https://kubernetes.default.svc with the
# pod's CA; the fake API is plain HTTP on loopback.
bindir = tmp / "bin"; bindir.mkdir()
realcurl = subprocess.run(["sh", "-c", "command -v curl"], capture_output=True, text=True).stdout.strip()

def run(api, case, wait="30"):
    srv = serve(api)
    port = srv.server_address[1]
    (bindir / "curl").write_text(f"""#!/bin/sh
n=$#; i=0
while [ "$i" -lt "$n" ]; do
  a=$1; shift; i=$((i + 1))
  case "$a" in
    --cacert) shift; i=$((i + 1)) ;;
    https://kubernetes.default.svc*) set -- "$@" "http://127.0.0.1:{port}${{a#https://kubernetes.default.svc}}" ;;
    *) set -- "$@" "$a" ;;
  esac
done
exec {realcurl} "$@"
""")
    os.chmod(bindir / "curl", 0o755)
    d = tmp / case
    for sub in ("certs", "out", "work", "token"):
        (d / sub).mkdir(parents=True)
    (d / "token" / "token").write_text("test-token")
    (d / "token" / "namespace").write_text(NS)
    (d / "token" / "ca.crt").write_text("unused")
    for a in ACCOUNTS:
        (d / "certs" / f"certificate-{a}.json").write_text(json.dumps({
            "apiVersion": "cert-manager.io/v1", "kind": "Certificate",
            "metadata": {"name": CERT(a)}, "spec": {"secretName": SECRET(a), "commonName": f"elitea-nats-bootstrap-{a}"}}))
    env = dict(os.environ, PATH=f"{bindir}:{os.environ['PATH']}", ACCOUNTS=" ".join(ACCOUNTS),
               CERT_DIR=str(d / "certs"), OUT_DIR=str(d / "out"), WORK_DIR=str(d / "work"),
               TOKEN_DIR=str(d / "token"), JOB_NAME=JOB, JOB_UID=UID, JOB_UID_LEGACY="", ISSUE_WAIT=wait)
    p = subprocess.run(["sh", script], env=env, capture_output=True, text=True, timeout=120)
    srv.shutdown()
    return p, d

def ops(api, method, kind):
    return [i for i, (m, p) in enumerate(api.log) if m == method and f"/{kind}/" in p]

# ── 1. after a failed sync: a leftover hook Certificate and stale Secrets ──
api = API(certs={CERT("main"): {"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
                                  "metadata": {"name": CERT("main"), "generation": 1},
                                  "spec": {"secretName": SECRET("main")}, "_ready": True}},
          secrets={SECRET("main"): {"tls.crt": "b2xk"}, SECRET("gateway"): {"tls.crt": "b2xk"}})
p, d = run(api, "leftover")
check("leftover: the run succeeds", p.returncode == 0, p.stderr + p.stdout)
check("leftover: no Secret was re-written behind a delete", not [e for e in api.log if e[0] == "REWRITE"], api.log)
cdel = [i for i, (m, pth) in enumerate(api.log) if m == "DELETE" and pth.endswith("/certificates/" + CERT("main"))]
sdel = [i for i, (m, pth) in enumerate(api.log) if m == "DELETE" and pth.endswith("/secrets/" + SECRET("main"))]
post = [i for i, (m, pth) in enumerate(api.log) if m == "POST"]
check("leftover: the old Certificate is deleted before its Secret, the Secret before the new Certificate",
      cdel and sdel and post and cdel[0] < sdel[0] < post[0], api.log)
for a in ACCOUNTS:
    c = api.certs.get(CERT(a), {})
    check(f"leftover: {a}'s Certificate is owned by the Job",
          c.get("metadata", {}).get("ownerReferences") == [{"apiVersion": "batch/v1", "kind": "Job", "name": JOB, "uid": UID}], c.get("metadata"))
    check(f"leftover: {a}'s Certificate went through Ready=False/DoesNotExist and Issuing before Ready", c.get("_gets", 0) >= 3, c.get("_gets"))
    for k in ("ca.crt", "tls.crt", "tls.key"):
        f = d / "out" / a / k
        check(f"leftover: {a}/{k} is the issued certificate", f.exists() and f.read_text() == f"{CERT(a)}/{k}", f.exists() and f.read_text())
    # As a Secret volume gives them: the bootstrap container may run as any uid.
    check(f"leftover: {a}'s files are readable whatever uid the bootstrap runs as",
          (d / "out" / a / "tls.key").stat().st_mode & 0o777 == 0o644 and (d / "out" / a).stat().st_mode & 0o777 == 0o755,
          oct((d / "out" / a / "tls.key").stat().st_mode))
check("leftover: the log says what it did", "deleted the Certificate" in p.stdout and "is Ready" in p.stdout, p.stdout)

# ── 2. a restarted pod of the same Job ────────────────────────────────────
api.log.clear()
p, d = run(api, "restart")
check("restart: the run succeeds", p.returncode == 0, p.stderr + p.stdout)
check("restart: its own Certificates and Secrets are kept", not [e for e in api.log if e[0] in ("DELETE", "POST")], api.log)
check("restart: the identities are copied again", all((d / "out" / a / "tls.key").exists() for a in ACCOUNTS))

# ── 2b. this Job's Certificate, but not the identity rendered now ─────────
api.certs[CERT("main")]["spec"]["commonName"] = "someone-else"
api.log.clear()
p, d = run(api, "drifted")
check("drifted: the run succeeds", p.returncode == 0, p.stderr + p.stdout)
check("drifted: a Certificate of this Job with another identity is re-created",
      [m for m, pth in api.log if pth.endswith("/certificates/" + CERT("main")) and m == "DELETE"]
      and [m for m, pth in api.log if m == "POST"] and api.certs[CERT("main")]["spec"]["commonName"] == "elitea-nats-bootstrap-main", api.log)
check("drifted: the matching one is kept", not [e for e in api.log if e[0] in ("DELETE", "POST") and CERT("gateway") in e[1]], api.log)

# ── 3. a Certificate that never becomes Ready ─────────────────────────────
api = API(never_ready={CERT("gateway")})
# The deadline is shared by every Certificate, and main's takes three GETs two
# seconds apart (at least 4s) before gateway's is polled. A wait under that,
# plus a slow runner's process start, timed out on main instead of gateway.
p, d = run(api, "never", wait="10")
check("never ready: the run fails", p.returncode != 0, p.stdout)
check("never ready: it names the Certificate and its condition",
      CERT("gateway") in p.stderr and "DoesNotExist" in p.stderr and "Issuing certificate as Secret does not exist" in p.stderr, p.stderr)

# ── 4. an API refusal is reported with its status and reason ──────────────
api = API()
orig = api.handle
api.handle = lambda m, pth, b: (403, {"message": "certificates.cert-manager.io is forbidden"}) if m == "POST" else orig(m, pth, b)
p, d = run(api, "forbidden")
check("forbidden: the run fails, naming the HTTP status and the API's reason",
      p.returncode != 0 and "HTTP 403" in p.stderr and "is forbidden" in p.stderr, p.stderr)

failed = [r for r in results if not r[1]]
for name, ok, detail in results:
    print(("  ok: " if ok else "  FAIL: ") + name + ("" if ok or not detail else f" — {detail}"))
print(f"nats-bootstrap-issue: {len(results)} assertion(s), {len(failed)} failed")
FLOOR = 20
if len(results) < FLOOR:
    sys.exit(f"only {len(results)} assertion(s) ran, under the floor of {FLOOR}")
sys.exit(1 if failed else 0)
PY
