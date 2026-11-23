---
date: 2026-11-23
---
## Moving the mail relay or an MCP server asks for its credential again

On Admin › E-mail, changing the SMTP host or port now asks you to type the password again in the same save, or to remove it. In the MCP servers catalogue, moving a server's URL, base URL or token URL to another host asks for its client secret again in the same way. The reason shows under the password or secret field. A stored credential is never shown, so this stops it from being sent to a new server by someone who never knew it. Other edits, such as a new sender address or a new path on the same server, keep the stored credential.
