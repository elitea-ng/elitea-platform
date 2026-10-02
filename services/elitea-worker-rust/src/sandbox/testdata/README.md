# Workload certificate parser fixtures

These self-signed DER certificates test identity extraction, not TLS trust.
Their private keys were temporary and were discarded. No production keys are included.

The common name is `worker.test` in every fixture. SAN values are:

| File | SAN |
| --- | --- |
| peer-dns.der | DNS:Worker.TEST. |
| peer-spiffe.der | URI:spiffe://elitea.test/worker/one |
| peer-mixed.der | DNS:worker.test,URI:spiffe://elitea.test/worker/one |
| peer-cn.der | Absent |
| peer-email.der | email:worker@example.test |

Generate replacements with `openssl req -x509 -newkey rsa:2048 -nodes -outform DER`.
Use a temporary key path, subject `/CN=worker.test`, and the SAN values above.
Certificate dates are irrelevant to these parser tests. The future TLS listener tests must verify trust and expiry separately.
