"""Fail unless a TLS connection to the live mock verifies against a trust bundle.

usage: verify-trust.py BUNDLE [HOST [PORT]]

Run by mcp-mock-trust right after it writes the bundle the NATIVE worker uses.
It checks what the consumer will do: verify the served chain against this
bundle. A mismatch (the bundle's CA is not the one that signed the served leaf)
surfaces here, named, instead of as an MCP outage in the worker.

This is an OpenSSL check; the native worker verifies with rustls, which is
stricter in places (e.g. BadSignature on same-subject CAs from different mints,
name constraints, EKU). It is still useful: the outage this guards against was
a CA/leaf mint mismatch, which OpenSSL rejects as "unable to get local issuer
certificate" / "signature failure" just the same. One attempt only: compose
already waits for the mock's service_healthy before starting this.
Exit status is non-zero in every failure case.
"""
import socket
import ssl
import sys


def main(argv: list) -> int:
    bundle = argv[1]
    host = argv[2] if len(argv) > 2 else "mcp-mock"
    port = int(argv[3]) if len(argv) > 3 else 8443
    sni = argv[4] if len(argv) > 4 else host
    context = ssl.create_default_context(cafile=bundle)
    try:
        with socket.create_connection((host, port), timeout=5) as raw:
            with context.wrap_socket(raw, server_hostname=sni):
                print("verify-trust: %s:%d verifies against %s" % (host, port, bundle))
                return 0
    except ssl.SSLCertVerificationError as exc:
        reason = "verify_code=%s verify_message=%s" % (exc.verify_code, exc.verify_message)
        msg = (exc.verify_message or "").lower()
        if "hostname" in msg or "ip address mismatch" in msg:
            hint = "the served certificate does not match the host name %r" % sni
        elif "expired" in msg:
            hint = "the served certificate (or a CA in the chain) has expired"
        else:
            hint = "the bundle does not contain the CA that signed the served chain"
        sys.stderr.write(
            "verify-trust: MISMATCH: %s:%d does not verify against %s: %s (%s)\n"
            % (host, port, bundle, hint, reason)
        )
    except ssl.SSLError as exc:
        sys.stderr.write(
            "verify-trust: TLS handshake failed: %s:%d: %s\n" % (host, port, exc.reason or exc)
        )
    except OSError as exc:
        sys.stderr.write("verify-trust: could not reach %s:%d: %s\n" % (host, port, exc))
    return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
