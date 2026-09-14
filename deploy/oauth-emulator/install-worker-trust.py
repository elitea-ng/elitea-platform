"""Append the fixture CA to a mounted rehearsal bundle. Preserve its first copy."""

import os
from pathlib import Path
import ssl


def main():
    if os.environ.get("OAUTH_EMULATOR_ENABLED") != "test-only":
        raise SystemExit("This command requires the isolated test opt-in.")
    bundle = Path("/trust/mcp-mock-ca-bundle.pem")
    backup = bundle.with_suffix(".before-oauth-emulator.pem")
    anchor = Path("/opt/oauth-emulator/tls/ca.crt").read_bytes()
    ssl.PEM_cert_to_DER_cert(anchor.decode())
    original = bundle.read_bytes()
    if anchor.strip() in original:
        print("The fixture CA is already trusted.")
        return
    if not backup.exists():
        with backup.open("xb") as output:
            output.write(original)
    # Preserve any roots another operator added after the first installation.
    combined = original.rstrip() + b"\n" + anchor
    temporary = bundle.with_suffix(".oauth-emulator.tmp")
    with temporary.open("xb") as output:
        output.write(combined)
        output.flush()
        os.fsync(output.fileno())
    temporary.chmod(bundle.stat().st_mode & 0o777)
    temporary.replace(bundle)
    print("The fixture CA is installed. The original bundle is preserved.")


if __name__ == "__main__":
    main()
