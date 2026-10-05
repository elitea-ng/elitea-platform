# Worker golden frames (client contract 1.2)

`python.json` and `rust.json` are the agent progress frames the two Elitea
workers emit, **captured by the workers' own unit tests** and committed
upstream as `testdata/client-frames/{python,rust}.json` in
`elitea-ng/elitea-platform` at `a9cada5eff87b5b56ff5fae91ca092534d2ad0ae`
(tag `v4.3.2`, Apache-2.0; see `contract/LICENSE.elitea`; `python.json` gained
`tool_paused` in #1067, `rust.json` is unchanged since `v4.2.0`). The server's
`TestClientFrameCatalogueAcceptsWorkerFrames` validates the same files
against the `x-elitea-client-frames` catalogue, so they are the frames a
real worker writes, not hand-written ones. Copied verbatim as test data.

Each value is the `data` of one `execution.node_event`. The tool, pause and
output-limit tests in `../../toolActivity.test.ts` and
`../../turnReducer.test.ts` are driven from them; the sequences they need
(several chunks, a parallel pause, a fan-out child) are built from these
frames by changing only the fields the catalogue says vary (`index`,
`content`, `hitl_interrupts`, `metadata.child_thread_id`).
