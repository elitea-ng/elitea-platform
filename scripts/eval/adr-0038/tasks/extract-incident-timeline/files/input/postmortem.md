# Postmortem draft — INC-2026-1028-A: Checkout payments failing

Status: draft, compiled from responder notes (not yet merged into one timeline)
Date of incident: Wednesday 28 October 2026

## Summary

For a bit over an hour, card payments at checkout failed for roughly 38% of
customers. The payments gateway could not complete TLS handshakes with our
third-party card processor (Paydora) after Paydora rotated its intermediate
CA certificate. Our payments-gateway pins a CA bundle that did not yet include
the new intermediate. Orders were rejected by checkout-api with "payment
unavailable". The search service was investigated early on (latency alert at
the same time) but turned out to be unaffected — its alert was a false
positive from a dashboard change.

Severity: opened as SEV-2. Upgraded to SEV-1 once it was confirmed that card
payments (not just a single region) were failing. Final severity: SEV-1.

Affected services: checkout-api, payments-gateway.

## Responder notes

Each responder wrote times in their own local time zone. Please convert before
merging.

### Dana Whitfield — incident commander (Seattle; times are local, Pacific)

- 09:05 — Synthetic checkout monitor fails; I see it in #payments-alerts.
  Treating this as the detection time.
- 09:30 — Escalated severity to SEV-1 after confirming all regions affected;
  notified the VP on call.
- 09:58 — Hotfix deployed: payments-gateway CA bundle now includes Paydora's
  new intermediate (mitigation). Error rate starts dropping.

### Jonas Brandt — EU on-call engineer (Berlin; times are local)

- 17:14 — Acknowledged the page, joined the bridge.
- 17:20 — Looked at search latency alert; dashboard query change, not real.
- 18:21 — Error rate back to baseline for 15 minutes. Declared resolved.

### Arjun Mehta — payments SRE (Bengaluru; times are IST, UTC+05:30)

- 22:10 — Found the cause: TLS handshake failures to Paydora, "unable to get
  local issuer certificate". Root cause identified.
- 22:25 — Opened vendor ticket with Paydora (#PD-55120).

### PagerDuty export (UTC)

- 16:07:12 — Page sent to payments-primary (Jonas Brandt).
- 16:14:40 — Acknowledged by Jonas Brandt.

## Impact

Later log analysis showed the first customer-facing payment errors at 15:52 UTC,
13 minutes before the synthetic monitor caught it. About 4,100 checkout attempts
failed; 2,300 of those customers retried successfully after resolution.

## Follow-ups

- Subscribe to Paydora's certificate-rotation notices.
- Alert on TLS handshake errors per upstream, not only on checkout success rate.
- Fix the search dashboard query that produced the false-positive alert.
