---
title: "Mobile: Android managed configuration, `lock_server`, read-only managed server (#1688, ADR-0052 W1b)"
issue: 1688
date: 2026-11-08
adr: ADR-0052
order: 110
---

The mobile app now reads managed configuration on Android as well as iOS
(same keys; see Admin › Managed devices in the docs, or
`apps/elitea-mobile/docs/mdm-appconfig.md`).

- **MDM admins:** `server_url` is no longer a suggestion. The connect screen
  shows it read only and offers no other server. A device that should still
  connect elsewhere must not get `server_url`.
- **New key `lock_server`:** with `server_url`, every workspace on another
  server is signed out and its data removed from the device when the key
  arrives. Removing the key does not bring them back.
- **Android:** the app declares its managed keys, so Intune, Workspace ONE
  and Test DPC list them. Nothing to do for existing deployments.
