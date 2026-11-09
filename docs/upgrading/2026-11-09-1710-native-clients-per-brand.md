# Native clients per brand (#1710, ADR-0052 decision 9)

Each brand's mobile and desktop apps are their own native client: the brand's
bundle id and its `<oauthRedirectScheme>:/oauth/callback` redirect. One
deployment can serve several brands side by side.

- **Who is affected:** operators who serve the apps of more than one brand, and
  operators who list clients in `NATIVE_CLIENTS_PATH`.
- **What to do:** nothing for ELITEA's apps alone; their client stays
  `ai.elitea.app` with redirect `ai.elitea.app:/oauth/callback`. To allow another
  brand's apps, register that brand's client next to it (admin console › Native
  clients, or the file). The Helm values comment for `NATIVE_CLIENTS_PATH` shows
  both. Docs: "Allow a brand's apps on your deployment" (Admin).
- **Behaviour:** unchanged. `enabled: false` refuses only that brand's sign-in;
  `min_client_version` holds back only that brand's older apps.
