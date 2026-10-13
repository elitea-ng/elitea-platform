"""Run: python3 deploy/mock-mcp/test_tls_material.py (needs openssl).

Guards the invariant the native worker's MCP journeys depend on: the CA the
trust bundle publishes is the CA the served chain ends in.
"""
import os
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import server  # noqa: E402


def mint(directory: str) -> None:
    def run(*args: str) -> None:
        subprocess.run(args, cwd=directory, check=True, capture_output=True)

    run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", "ca.key",
        "-out", "ca.crt", "-days", "2", "-subj", "/CN=elitea-mock-mcp-ca")
    run("openssl", "req", "-newkey", "rsa:2048", "-nodes", "-keyout", "server.key",
        "-out", "server.csr", "-subj", "/CN=mcp-mock")
    run("openssl", "x509", "-req", "-in", "server.csr", "-CA", "ca.crt", "-CAkey", "ca.key",
        "-CAcreateserial", "-out", "server.crt", "-days", "2")
    with open(os.path.join(directory, "server-chain.crt"), "w") as out:
        for name in ("server.crt", "ca.crt"):
            with open(os.path.join(directory, name)) as part:
                out.write(part.read())


class CaMatchesChain(unittest.TestCase):
    def test_same_mint_matches_and_a_second_mint_with_the_same_subject_does_not(self) -> None:
        with tempfile.TemporaryDirectory() as a, tempfile.TemporaryDirectory() as b:
            mint(a)
            mint(b)
            chain = os.path.join(a, "server-chain.crt")
            self.assertTrue(server.ca_matches_chain(chain, os.path.join(a, "ca.crt")))
            self.assertFalse(server.ca_matches_chain(chain, os.path.join(b, "ca.crt")))


if __name__ == "__main__":
    unittest.main()
