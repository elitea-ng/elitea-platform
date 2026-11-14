# Agent memory is recalled by meaning (issue 1657, ADR-0050 decision 1b)

- **What changed.**
  - **Recall ranks an agent's notes by similarity.** Main embeds each active note through the LLM gateway, with the
    default embedding model of the project, and writes it to `elitea-vector` as the new source `agent_memory`
    (`SOURCE_AGENT_MEMORY`, one namespace for each project and agent). At the start of a turn Main embeds the message and
    searches that namespace. The notes in the database stay the source: they decide the candidates, the people rule and
    the budgets. If the embedding or the vector store fails, recall uses the old ranking by words and recency.
  - **Removal is durable.** A trigger writes a row to the new journal `centry.agent_memory_vector_journal` in the
    transaction of every change to a note. A reconciler in each Main replica embeds an active note or removes the point
    of a note that is archived, pending, deleted or cleared. The removal uses the administrator certificate of Main and
    needs no embedding model.
  - **New migrations.** Shared migration `0248` adds the journal and enqueues the active notes the projects already hold.
    Tenant migration `0248` adds the trigger and enqueues the notes of its own project. Both enqueue steps are safe in any
    order, so no note is missed.
  - **New token.** Main signs its own short-lived token for `elitea-vector` with a key derived from
    `APPLICATION_SECRET_KEY`. `elitea-vector` verifies it through the existing token introspection of Main. No new
    setting is needed, and no new internal HTTP route.
- **Operator action.**
  1. Upgrade `elitea-vector` before or with Main. An older `elitea-vector` refuses the new source; recall then uses the
     keyword ranking and the journal rows wait with a growing pause (at most one hour).
  2. Apply the shared and the tenant migrations with `elitea-migrate`, as for every migration; both are safe to run again.
  3. Check that `ELITEA_VECTOR_GRPC_TARGET`, `ELITEA_VECTOR_INTROSPECTION_CLIENTS`, `LLM_GATEWAY_URL` and
     `APPLICATION_SECRET_KEY` are set. If one is missing, Main logs one line at start (`agent memory vectors are off`) and
     recall stays on the keyword ranking.
  4. Make sure each project has a default embedding model. A project without one cannot copy its notes; the rows wait.
- **Backfill.** The migrations enqueue the active notes of each project once. A reconciler copies them in the
  background, 64 notes at a time.
- **Changing the default embedding model.** A note is copied into the vector space of the model that is the default
  when it is saved. Main records the model of each project (`centry.agent_memory_vector_models`). When the default model
  changes, Main sees the new model at the next embedding and queues every active note of the project again, so the
  notes move to the new space. The old points leave when their note leaves. Until then, recall ranks the notes in the
  new space first.
- **Cost.** One embedding call for each note saved or edited, and one for each turn of an agent with memory on whose notes do not all fit the recall budget, with a
  non-empty message. The calls go through the gateway, so the budget and the audit of the project apply.
- **Turn it off.** Set `ELITEA_AGENT_MEMORY_VECTORS_ENABLED=false` on Main. Recall then ranks by words and recency. The
  journal keeps filling (the trigger is in the database), so set the switch back to `true` to copy the notes later.
- **Rollback.** Set the switch to `false`. The journal, the trigger and the vector namespaces are not read by older
  versions. A project delete (`DropProject`) removes the vectors with the project's other vectors.
