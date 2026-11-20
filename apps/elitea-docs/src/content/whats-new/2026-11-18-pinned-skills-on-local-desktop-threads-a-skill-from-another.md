---
date: 2026-11-18
order: 11
---
## Pinned skills on local desktop threads; a skill from another project is quoted data on the desktop too

In the desktop app, the `/` menu of a local thread has **Pin to this thread** on each project skill, **Unpin** on a pinned one, and a **Pinned** label. The desktop app keeps the pin with the thread (only the skill's id, never its text) and applies every pinned skill to each message of that thread, within the usual limit of five skills and 64 KiB together. A pin on a skill you can no longer read is skipped. A skill written in another project that your project has not trusted reaches the model as quoted data in a local thread, as it does in the cloud: this applies to a skill you pick with `/` too, not only to a pin. **Use in a chat** and **Add to a conversation** work on a local thread too: for a Marketplace listing the skill is copied into your project first (the desktop app never reads listings), and the copy is pinned. A pinned skill that was deleted, or that you can no longer read, is shown in the menu as unavailable with **Unpin**, and does not count toward the limit of five. See [Skills in a conversation](/menus/chat#skills-in-a-conversation).
