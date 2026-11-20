---
date: 2026-11-09
order: 1
---
## Sub-agent profiles and briefs

`spawn_agent` now takes a `profile`: a display name, a short role (the soul), the tools, the model (`inherit`) and an avatar key. It also takes a `context` brief: quotes of messages, this turn's attachments, facts and recalled memory entries. The platform checks that the agent itself sees each reference, and refuses the call when it does not. Each spawn of a cloud run is recorded once, by digest, never by its text. A local turn in the desktop app now checks a brief too: it accepts quotes of the messages its agent read and the images attached to the current message. It still refuses memory entries. See [Sub-agent profiles and briefs](/how-tos/agents-pipelines/sub-agent-profiles/#in-the-desktop-app).
