---
date: 2026-11-24
---
## Let gh run with its login, without a question (desktop)

On macOS, an Allow rule that you write in **Settings ▸ Approval rules** can now give the GitHub CLI its stored login and limit the command to a few named hosts, still inside the sandbox. A command the rule covers runs without asking, even in **Act** mode, and the token never shows in the output or the transcript. Conditions keep writes out of a read-only rule: forbid flags such as `-f` or `--input`, require `--draft`, allow only `-X GET`, or admit GraphQL queries but not mutations. A repository's rules can never grant a login or hosts. On Linux and Windows such rules grant nothing yet, and network commands are asked as before. See [Decide which commands run without asking (desktop)](/how-tos/chat-conversations/desktop-approval-rules/).
