# CI: the eng-runner image bakes in the action archives (#3003)

**What changed.** The self-hosted CI runner image (`deploy/ci-runners/image/`)
now carries a copy of every GitHub Action the repository's workflows use, and
sets `ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE` so the runner reads them from the
image instead of downloading them from GitHub at the start of every job. Jobs
on the pool no longer fail in "Set up job" when GitHub rate-limits those
downloads. An action that is not in the image is downloaded as before.

**Who is affected.** Only the project's own CI runner pool. The product, its
charts, images and configuration are unchanged.

**What to do.** No operator action. A deployment of Elitea needs nothing.
Whoever runs the CI pool picks the change up with the next runner image pin.
