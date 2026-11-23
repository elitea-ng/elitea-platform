# CI: the handoff publish load gate tells a runner stall from a regression (#2940)

- **What changed:** the CI load gate on the workspace publish path
  (`ci-handoff-loadgate`) runs a stall canary in every measured window. A
  window in which the runner stalled for more than 25 ms is measured again,
  up to 3 times. The first stall-free window decides pass or fail against the
  same 50 ms p99 gate. When no window is stall-free, the case fails with
  "measurement invalid". The job also starts on fewer changes: only the shared
  migrations that name a table of the publish path, not every shared
  migration.
- **Who is affected:** contributors whose pull requests run this job. No
  deployed service, chart, configuration or migration changed.
- **What to do:** nothing. No operator action is needed.
