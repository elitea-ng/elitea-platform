# A Code node as a Map worker or a Parallel branch (W3-2a)

- **What changed.**
  - **A Code node can run directly as a Map worker or as a Parallel branch.** Each Map item and each branch runs its
    Code under its own child thread, with its own journal, so a completed item is never run again after a restart and
    a lost runtime is retried as for a root Code node. A Map Code worker reads its item (any JSON) and its index.
    - A Router is still refused as a direct Map worker or branch, now with its own text: "a Router runs inside a child
      pipeline, not as a Parallel branch or Map worker". Saved pipelines that contain Code, as a worker or branch, are
      still refused (W3-2b).
    - A Code item that may already have started, and whose effect no owner proof can settle, ends with a readable
      failure for that item and is never repeated; the other items keep their results. The Worker logs one WARN line,
      code `pipeline.node_recovery.child_effect_unknown`, with `node_id` and the item's `support_ref` (the same one its
      failed entry in the Map or Parallel result shows). Operator recovery per item is not in this change.
  - **A fan-out of Code items is checked against the run's deadline before any item starts.** When the items cannot
    finish in the time left (`ceil(items / min(concurrency, code_slots)) × Code timeout + 90 s`), the node is refused
    before anything is frozen or started and the chat shows: "The fan-out node could not start: too little time was
    left before the run's deadline for its items." The Worker logs one WARN line, code
    `graph.fanout.deadline_insufficient`, with `node_id`, `items`, `concurrency`, `slots`, `required_ms`,
    `remaining_ms` and `support_ref`. A fan-out restored after a restart is never refused this way.
  - **A Code attempt is checked again after its preparation**, just before its first submission. When dependency
    preparation used up the window, it ends with the existing "The Code node could not start: too little time was left
    before the run's deadline." and the WARN line has `cause: preparation`. An attempt that may already have started is
    never refused this way.
  - **A Code attempt never waits for a second Code slot.** If a code path ever asked for one inside an attempt that
    already holds a slot, the Worker refuses it at once and logs one ERROR line, code `pipeline.code.nested_slot`.
- **Who is affected.** Pipeline authors (Code is now offered as a Map worker and a Parallel branch); operators who
  roll out Main and the Workers separately.
- **What to do.**
  - Roll out Main first, then the Workers. A Main without this change refuses to save or start a pipeline with a
    Code worker or branch (`PIPELINE_MAP_FORM_UNSUPPORTED`, `PIPELINE_PARALLEL_BRANCH_UNSUPPORTED`), and refuses the
    Code visit of such an item (its owner scope is new), so the item fails readably and nothing hangs. No migration is
    needed.
  - `code_slots` (W3-1) now also bounds the deadline check of a fan-out: set it to each Supervisor's `concurrency`.
