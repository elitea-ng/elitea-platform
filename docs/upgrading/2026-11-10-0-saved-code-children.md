# Code inside a saved pipeline, at the root and as a Map worker or Parallel branch (W3-2b)

- **What changed.**
  - **A saved pipeline that contains Code nodes now runs them.** This holds when an Agent node of the started pipeline
    runs it, and when it is the Map worker or a Parallel branch of the started pipeline. A Router inside it is allowed,
    for example the ETL shape `validate (Code) → route_quality (Router) → normalize (Code)` as a Map worker.
    - Before this change, Code in a saved pipeline run by an Agent node always failed typed: the Worker had no
      journal or owner scope for its attempt, and Main refused such a visit. The same pipeline as a Map worker or Parallel branch was refused before it started with
      `graph.fanout.child_contains_code`. That code is gone.
    - Each Code attempt of a saved pipeline is journaled under the saved pipeline's own thread, scoped to that visit.
      After a Worker restart it re-attaches. A lost runtime is re-run once. A platform-client attempt that may have
      run is reconciled from Main's owner proof, or ends typed with `pipeline.node_recovery.child_effect_unknown`.
      Nothing is repeated blind. A Map item or Parallel branch that ends this way names that code in its result.
    - A fan-out of such saved pipelines is checked against the run's deadline before any item starts. Each item's Code
      time is the sum of its saved pipeline's Code timeouts, each node counted once. A Router loop that re-enters Code
      beyond that is checked per attempt: it ends with "The Code node could not start: too little time was left before
      the run's deadline." and never overruns.
  - **Still refused, readably, before the run starts:**
    - Code in a saved pipeline that another saved pipeline runs. New code: `graph.fanout.nested_saved_code`.
    - A Map or Parallel of Code inside a saved pipeline. Unchanged: `graph.fanout.code_below_root`.
    - A Router as the direct Map worker or Parallel branch. Unchanged.
  - **Main** resolves the owner of every such Code visit in the visit's own transaction, against its own freeze
    capture of the saved pipeline and the run root's frozen definition, and fails closed. A refused visit writes the
    usual WARN line `code.registration_refused`, with `stage` naming the failed check: `saved_child_scope`,
    `saved_child_parent`, `saved_child_thread`, `saved_child_edge`, `saved_child_capture` or `saved_child_code`.
- **Who is affected.**
  - Pipeline authors who use saved pipelines with Code.
  - Operators who roll out Main and the Workers separately.
- **What to do.**
  - Roll out Main before the Workers. Mixed versions are safe:
    - An older Main refuses the new owner scope fail-closed. Each affected Code attempt ends with
      `pipeline.code_authorization_failed` before any intent or dispatch, so nothing runs.
    - A newer Main with an older Worker changes nothing: the older Worker never names the new scope.
  - There is no migration and no configuration change.
