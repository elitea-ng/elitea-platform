---
date: 2026-11-17
order: 0
---
## Credentials and toolkits: Test connection, secrets, base URL and the toolkit list

**Test connection** on a credential now works when its key is a named secret picked on the **Secret** tab, before you save: the platform redeems the secret on the server (the browser never sees it) and tests the real key. A model's **Test connection** sends 16 output tokens instead of 1, so a provider whose minimum is 16 (OpenAI models on Bedrock) no longer reports a working model as failing. A new GitHub credential opens with the Base URL `https://api.github.com` filled in. A toolkit is no longer blocked from saving when its credential's check could not reach the provider or the provider host is refused by the connection-check egress policy; only a credential the provider rejected blocks Save. **Open in new tab** next to a credential in the toolkit form opens that credential's configuration page in Elitea, not the service address. A toolkit you create or save shows in the list at once. SharePoint sign-in scopes may be separated by commas or spaces. See [Credentials](/menus/credentials).
