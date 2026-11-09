# Certificates and per-app VPN for the brand apps (#1719, ADR-0052 W6.3)

Docs only. The admin guide now explains how MDM-delivered trusted root
certificates, client certificates and per-app VPN apply to the apps.

- **Who is affected:** administrators who deploy the mobile or desktop app
  through an MDM, to a workspace behind a private CA, a VPN or a gateway
  that asks for a client certificate.
- **What to do:** nothing changes. Scope certificate and VPN profiles by the
  app identifiers of the build you deploy (ELITEA: `ai.elitea.app`,
  `ai.elitea.app.widgets`, `ai.elitea.desktop`). Docs: "Managed devices
  (MDM)" (Admin), "Certificates and per-app VPN".
- **Behaviour:** unchanged. The apps present no client certificate
  themselves, and Android trusts only system CAs.
