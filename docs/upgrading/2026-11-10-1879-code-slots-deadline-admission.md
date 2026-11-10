# Worker Code slots per Supervisor and deadline admission for Code nodes (W3-1)

- **What changed.**
  - **The Worker bounds its own Code attempts per Supervisor.** Each Worker process runs at most `code_slots` Code
    attempts at once on one sandbox Supervisor target. One attempt holds one slot from before its first effect
    (dependency preparation, submission) through any Busy or Pending backoff until its result. An attempt that finds
    every slot taken waits for one instead of making the Supervisor answer Busy.
    - The setting is `sandbox_runtimes[].code_slots` in the Worker's `runtime.json`, 1 to 1024. Without it the Worker
      uses 4, the shipped Supervisor `concurrency`.
    - Entries that point at one Supervisor share one set of slots. The target is compared after normalising it (host in
      lower case, no trailing dot, port 443 when absent), so javascript and typescript on one Supervisor share it.
      Entries on one Supervisor that set different values stop the Worker at startup with "sandbox_runtimes that share
      one Supervisor target must set the same code_slots".
    - At startup the Worker logs one INFO line per target, event `code_slots_configured`, with the value in force.
    - Helm: `worker.runtime.sandboxRuntimes[].codeSlots`, written as `code_slots` only when set. The render refuses a
      value that is not a whole number from 1 to 1024.
  - **Code nodes are admitted against the run's deadline.** A Code attempt that has not started yet runs only when
    the run has at least the Code timeout (`timeout_seconds`) plus 90 seconds left. Otherwise it stops before anything
    is sent, and the chat shows: "The Code node could not start: too little time was left before the run's
    deadline." An attempt that may already have started (recovery after a restart) is never refused this way; it is
    reconciled as before.
    - With the default 24-hour run deadline and the shipped 60 s and 180 s Code timeouts, only the last 150 s or 270 s
      of a run are affected.
    - The Worker logs the refusal as one WARN line, code `pipeline.code.deadline_insufficient`, with `node_id`,
      `required_ms`, `remaining_ms`, `slots` and `support_ref` (the execution id). When the wait for a slot used up the
      time, the line also has `cause: slot_wait` and `slot_wait_ms`: the Worker was full, not the run late.
  - **One more registered failure text,** for W3-2: "The fan-out node could not start: too little time was left
    before the run's deadline for its items." Nothing raises it yet.
- **Who is affected.**
  - Deployments whose Supervisor `concurrency` is not 4: the Worker now limits itself to 4 per target unless
    `code_slots` says otherwise.
  - Operators who run Workers and Main at different versions during a rollout.
- **What to do.**
  - Set `code_slots` (Helm `codeSlots`) on every sandbox runtime entry to that Supervisor's `concurrency`. Several
    Worker replicas each get their own slots, so a Supervisor shared by N Workers can still see up to N times
    `code_slots` attempts and answer Busy; the existing backoff handles that.
  - Roll out Main first, then the Workers. A Main without this change does not know the new failure text and refuses
    it; the Worker then replaces it once with the generic Code failure ("The pipeline stopped at a Code node. …"), so
    the run still ends with a readable failure and nothing hangs, but the specific sentence is lost until Main is
    upgraded.
