---
date: 2026-11-16
order: 8
---
## Skills in a model chat, and untrusted skill text

The message start for a chat with only a model now accepts project skills (`payload.skills`, ids only), applied through the same skill loading an agent chat uses, with the same limits (five skills, 64 KiB, a skill you cannot read refuses the message). Text of a skill forked from another project reaches the model as quoted data until your project trusts that version of it; skills that existed before keep working as before. **Run the migrations** (tenant `0250`) and upgrade Main and the worker together. See [Skills](/menus/skills#skills-from-another-project-are-untrusted-until-you-trust-them).
