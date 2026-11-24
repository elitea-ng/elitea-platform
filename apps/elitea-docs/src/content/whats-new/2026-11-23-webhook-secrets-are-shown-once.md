---
date: 2026-11-23
---
## Webhook secrets are shown once and kept in the vault

A project webhook's signing secret is now stored in the project's secret vault and shown only once: when you create the webhook and when you rotate its secret. The Webhooks settings page no longer shows or copies a stored secret. The Secret column tells you whether one is set. If you lose a secret, select **Rotate secret**: Elitea generates a new one, shows it once and deletes the old one. Existing webhooks keep their secrets, so your receivers keep verifying deliveries without any change.
