# CI: web changes skip the macOS desktop host, and a never-acquired macOS host gets one second attempt (#2983)

**What changed.** This change affects the pull-request pipeline of this repository only:

- A change under `apps/elitea-web/src` (outside the desktop entry and the desktop transport), to
  `apps/elitea-web/vite.config.ts` or to `libs/ts/client-core` no longer starts `ci-desktop.yml`. Before, such a
  change started four hosted jobs (macOS, two Windows, one Linux). `ci-web.yml` `build` now builds the ELITEA
  desktop bundle on the self-hosted pool.
- If GitHub never gives the macOS host job a runner, the job ends as cancelled after 15 minutes with no step
  run. That job now gets one second attempt. `ci-gate` reads the workflow's new `verdict` output, so a second
  attempt that passes turns the gate green. A job that ran and failed still fails the gate.

`docs/contributing/ci.md`, "A job GitHub never acquired", has the evidence and the trade-off.

**Who is affected.** Contributors whose pull requests run CI. Deployments are not affected: no chart value,
image or setting changed.

**What to do.** No operator action is needed.
