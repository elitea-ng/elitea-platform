---
date: 2026-11-14
order: 12
---
## Agent memory is recalled by meaning

An agent's notes are now ranked by how close they are to your message, not only by shared words. The platform keeps a searchable copy of each active note in the vector service, made in the background with the project's default embedding model, and removes it when the note is archived, deleted, cleared, opted out or purged. If the embedding model or the vector store is down, recall falls back to the ranking by words and recency, so no turn fails. Notes saved before the upgrade are copied once in the background. **Run the migrations** (a shared and a tenant one) and upgrade Main and the vector service together. See [Agent memory](/how-tos/agents-pipelines/agent-memory/#recall-by-meaning).
