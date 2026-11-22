---
date: 2026-11-20
---
## Connection checks for OpenAPI client credentials and SharePoint tenants; Windows downloads carry the Mark of the Web; the Applications list filters on the server

**Test Connection** now runs for an OpenAPI credential that uses OAuth client credentials (client ID, client secret and an https token URL): it asks the token URL for a token, as the SDK does. The token URL's host must be on `ELITEA_TOOLKIT_CHECK_ALLOWLIST`, otherwise the check answers "could not be reached". A SharePoint client-secret credential that names its own Entra tenant (`tenant_id`) is checked without a site. On Windows, a file the browser pane downloads now carries the Mark of the Web (a `Zone.Identifier` stream, zone 3), so SmartScreen and Office's Protected View treat it as downloaded. The toolkits list takes a repeatable `type` filter, so the Applications tab no longer reads up to 1,000 toolkits and filters them in the browser. Client contract 1.65. See [Test connection](/menus/credentials#test-connection) and [The Windows and Linux browser drivers](/development/browser-driver-windows-linux#limits).
