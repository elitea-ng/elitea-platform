# IT support contact in the apps (#1718, ADR-0052 W6.2)

The native client policy has a new field, **IT Support Contact**
(`support_contact`). The mobile and desktop apps show it as "IT support" in
About and Settings. A device's managed configuration can set its own
`support_contact`, which wins.

- **Who is affected:** administrators of the native client policy and of
  managed devices; client builders (client contract 1.45).
- **What to do:** nothing. To show a line, set Admin › Configuration › Native
  client policy › IT Support Contact, or the managed key `support_contact`.
  Docs: "Managed devices (MDM)" (Admin), "IT support contact".
- **Behaviour:** client contract 1.45 (additive). `support_contact` is in the
  token response's `client_policy` and in the discovery document's
  `client_policy` (empty when unset). The save path refuses a value that is
  not one line of at most 200 characters; the loader ignores a stored one.
  The apps make at most one link from it (https, email or phone). The macOS
  app reads only a value a configuration profile enforces.
