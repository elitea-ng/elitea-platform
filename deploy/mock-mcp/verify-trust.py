"""Fail unless a TLS connection to the live mock verifies against a trust bundle.

usage: verify-trust.py BUNDLE [HOST [PORT]]

Run by the trust one-shots (mcp-mock-trust, worker-trust) right after they write
their bundle. This is the check that matters: not "does a file look right" but
"does what the consumer will do — verify the served chain against this bundle —
succeed". A mismatch (the bundle's CA is not the one that signed the served leaf)
surfaces here, named, instead of as an MCP outage in the worker.
"""
import socket
import ssl
import sys
import time


def main(argv: list) -> int:
    bundle = argv[1]
    host = argv[2] if len(argv) > 2 else "mcp-mock"
    port = int(argv[3]) if len(argv) > 3 else 8443
    context = ssl.create_default_context(cafile=bundle)
    last = None
    for _ in range(30):
        try:
            with socket.create_connection((host, port), timeout=3) as raw:
                with context.wrap_socket(raw, server_hostname=host):
                    print("verify-trust: %s:%d verifies against %s" % (host, port, bundle))
                    return 0
        except ssl.SSLCertVerificationError as exc:
            sys.stderr.write(
                "verify-trust: MISMATCH: %s:%d does not verify against %s (%s); the "
                "bundle's mock CA is not the one that signed the served chain\n"
                % (host, port, bundle, exc)
            )
            return 1
        except (OSError, ssl.SSLError) as exc:  # mock still starting
            last = exc
            time.sleep(1)
    sys.stderr.write("verify-trust: could not reach %s:%d: %s\n" % (host, port, last))
    return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
