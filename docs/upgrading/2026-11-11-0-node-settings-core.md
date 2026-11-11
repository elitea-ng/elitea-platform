# Pipelines: per-node settings for State Modifier, Router, Split Out and Aggregate (5b-1 PR A)

**Upgrade Main first, then the Worker, then the Web.** Main is the save-time check for the new `settings:` block on a
pipeline node: with the new Main, a block the runtime cannot honour is refused on save with a readable 400
(`PIPELINE_NODE_SETTINGS_INVALID`, naming the node and the fix). An older Main stores such a block unchecked; the
new Worker still refuses it when the run starts, but the author only learns why from the editor. An older Worker
refuses any node with `settings:` (its strict node parser does not know the key), typed and before any effect, so
upgrade the Worker before authors start using the panel.

What changed:
- A State Modifier, Router, Split Out or Aggregate node may carry `settings:` with `on_error` (`stop`, `continue`,
  `error_output` + `error_transition`), `always_output_data` and `execute_once` ("Run once per execution"). Retry and
  timeout are refused on these node types; Code gets them in the next change. Every other node type refuses the
  block.
- A failure settled by `continue` or `error_output` is recorded as one entry in the `node_errors` state list, which
  is declared automatically when a node continues on error. A user's own `node_errors` key must then be a list with
  no reducer or `append`.
- Pipelines without `settings:` are unchanged: same definition digest, same checkpoints, same behaviour.

Who is affected: authors who use the new Settings section in the pipeline editor. No configuration, Helm value or
migration changes. One new operator log line, `pipeline.node.failed_continued` (INFO), per settled failure.
