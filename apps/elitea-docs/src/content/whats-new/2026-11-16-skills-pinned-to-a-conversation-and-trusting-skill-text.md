---
date: 2026-11-16
order: 9
---
## Skills pinned to a conversation, and trusting skill text

A conversation can carry up to five skills, shown in their own **Skills** section of the conversation panel on web and desktop, and directly below **Participants** in the mobile conversation sheet. A pinned skill is not a participant: it applies to every reply, whichever agent or model answers, and shares the five-skill, 64 KiB limit with the skills one message invokes. A skill from another project shows an **Untrusted** badge until your project trusts that version of its text (`GET`, `POST` and `DELETE` on `skill_trust`; granting and revoking are audited, and an edit of the skill by a member of your project is trusted by that member). A pin never refuses a turn for an agent that already has a skill of the same name: the agent's own skill wins. **Run the migrations** (tenant `0251`) and upgrade Main, the worker and the clients together (client contract 1.57). See [Chat](/menus/chat#skills-in-a-conversation).
