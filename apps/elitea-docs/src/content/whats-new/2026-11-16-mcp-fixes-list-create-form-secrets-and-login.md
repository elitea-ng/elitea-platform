---
date: 2026-11-16
order: 5
---
## MCP fixes: list, Create form, secrets and Login

The **MCPs** list now asks the server for MCPs only, so a project with many toolkits no longer shows an empty list or sends you to **New MCP**. The Create form no longer lists your other MCPs as tools before **Load Tools**. A saved **Headers** value such as `Authorization: Bearer {{secret.token}}` and a **Client Secret** that is a secret reference now read back as saved, instead of coming back empty and being dropped on the next save. **Load Tools** keeps each tool's input schema, so **Test settings** shows the input fields and enables **Run** for MCPs that need a login. **Login** on a saved MCP answers with a message: connected, or the reason it failed. A plain literal header or client secret is still never returned; reference it with `{{secret.name}}`.
