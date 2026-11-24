---
date: 2026-11-24
---
## Weak signing secrets are refused, and webhooks are always signed

The platform services refuse to start with a signing or identity secret that is shorter than 32 bytes, a placeholder such as `changeme`, or a value once published in the repository; the error names the variable, never the value. The LLM gateway and the DeepWiki and Inventory hosts also refuse to start without their identity secret. Every outbound webhook now has a signing secret: one is generated when you register a webhook without one, and existing webhooks without a secret get one at upgrade, so rotate their secret to read it. See [Signing secrets and identity secrets](/deployment/configuration#signing-secrets-and-identity-secrets).
