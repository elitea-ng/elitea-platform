# Pipelines: a pipeline reads the conversation so far as `chat_history` (#1668, ADR-0050 decision 9)

- **What changed.** When a pipeline answers in a conversation, the Rust worker now seeds the pipeline's own context
  view into the root graph's state as `chat_history`, on the run's first step. `chat_history` becomes a built-in
  state key that nodes can read: an LLM node can map it (`chat_history: {type: variable, value: chat_history}`), and
  so can an fstring or a mapping into a saved child pipeline. Before, a pipeline that named `chat_history` in a
  mapping or an input was refused at assembly. Nodes still cannot write it.

  Messages from another agent or person reach the model as user messages in the untrusted author envelope.
  Resumes and recoveries keep the history the run started with. The seed keeps the newest messages that fit: at most
  256 messages and about 200 KiB. A single message over about 60 KB is cut, and ends with a truncation marker.
- **`messages` changes too.** A pipeline that declares no `messages` state now starts with the same history ahead of
  the current message in `messages`, as the Python SDK did. Every node that reads `messages` by default (LLM nodes
  without an explicit mapping, and Decision nodes) now sees the earlier conversation, not only the current question.
  That can change a Decision node's routing, and it makes prompts longer, so model spend per turn can rise.
- **Who is affected.** Pipelines that run in a conversation with earlier messages and do not declare `messages`.
  Pipelines that declare `messages`, and pipelines run outside a conversation, see no change in `messages`.
- **What to do.** To keep a node's input to the current question only, declare a `messages` state in the pipeline,
  or map the node's input to `input`. Check that Decision nodes that route on `messages` still pick the intended
  branch with the earlier turns present.
