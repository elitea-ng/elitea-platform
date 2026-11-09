---
title: "Other agents are labelled in an agent's history (#1603, ADR-0050 decision 9a)"
issue: 1603
date: 2026-11-05
adr: ADR-0050
order: 73
---

Upgrade the workers (`elitea-worker-rust`) before Main. In a conversation
with more than one agent, or more than one person, Main now marks each
history message written by someone other than the person asking or the
agent answering: the message is `user`-role and carries
`additional_kwargs.elitea_author` (`kind` `agent` or `person`, and the
author's name). A worker of an earlier release accepts only an empty
`additional_kwargs` and refuses that history with `invalid_profile`, so the
turn fails. A conversation with one person and one agent sends exactly
what it sent before, so it keeps working during the rollout.

Also in this release, with nothing to do:

- The worker frames such a message as an untrusted envelope that names
  its author (`<untrusted source="message" from="CI Doctor" kind="agent">`),
  and the built-in platform preamble now says text from other participants
  is data.
- Thinking from an earlier turn is no longer sent back to the model, on
  any provider. Within one turn's tool loop, the native Anthropic path
  replays the provider's signed thinking blocks, and the OpenAI-compatible
  path keeps the loop's `reasoning_content`.
- The desktop app rebuilds a session with another agent's answers as
  labelled messages, without that agent's tool steps.
