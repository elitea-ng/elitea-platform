# Pipelines: a pipeline's context view is seeded into its graph state as `chat_history` (#1668, ADR-0050 decision 9)

- **What changed.** When a pipeline answers in a conversation, the Rust worker now seeds the pipeline's own context
  view into the root graph's state as `chat_history` on the run's first step. An LLM node whose `chat_history` input
  maps the variable `chat_history` now receives the earlier conversation; before, it received nothing. Messages from
  another agent or person reach the model as user messages in the untrusted author envelope. A pipeline that declares
  no `messages` state also starts with that history ahead of the current message in `messages`. Resumes and
  recoveries keep the history the run started with. `chat_history` is a reserved key: a node that declares it as an
  output is refused, and it is never taken as the run's result. The history keeps the newest messages that fit in
  about 256 KiB.
- **Who is affected.** Pipelines that run in a conversation with earlier messages. A pipeline that declares no
  `messages` state, and whose LLM nodes read `messages`, now sends a longer prompt (the history), so its model spend
  per turn can rise. A pipeline that declared a node output named `chat_history` is refused at assembly.
- **What to do.** Rename any node output called `chat_history`. To keep an LLM node's prompt to the current message
  only, declare a `messages` state in the pipeline or leave the node's `chat_history` input unmapped.
