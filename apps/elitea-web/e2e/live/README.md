# `e2e/live` — the lanes that talk to something real

Every journey in this directory needs a credential or a model that no stack in
this repository can fake. They exist because the legacy public suite's
credential-bound cases had to be REUSED rather than dropped: the wave rule is
that a legacy test is ported so it runs wherever its prerequisites exist, and
never parked behind a permanent `test.skip`.

There is **no `test.skip` anywhere under `e2e/live`**, and there must never be
one. A skip is reported as a pass-shaped row; an unconfigured provider here
contributes **zero** tests instead.

## The two projects

| Project | Files | Prerequisite |
|---|---|---|
| `toolkits-live` | `toolkits.*.spec.ts` | one or more provider credentials (below) |
| `image-live` | `image.*.spec.ts` | `E2E_LIVE_IMAGE_MODEL` |

`playwright.config.ts` reads `e2e/live/liveEnv.ts` and adds each spec to the
project's `testIgnore` when its variables are absent, which is the same
mechanic `chromium`/`webkit` use to keep the stack-gated DeepWiki and Support
journeys out of themselves. The observable contract:

```bash
# no live variables set
npx playwright test --list --project=toolkits-live   # 0 tests
npx playwright test --list --project=image-live      # 0 tests
```

A `--project=image-live` run with no model therefore ends on Playwright's own
"no tests found" error and a non-zero exit. That is deliberate: the lane fails
loudly rather than reporting a green run of nothing.

Both lanes need the **full standalone stack** with a real model — the
`chat-stream-real` shape — because every journey drives a real chat turn.
Run them through `scripts/chat-stream-e2e.sh` with
`PLAYWRIGHT_PROJECT=toolkits-live` (or `image-live`); that script forwards
every `E2E_LIVE_*` variable into the Playwright container.

## The variables

`E2E_LIVE_` prefixes every one of them on purpose. The legacy suite read bare
`GITHUB_TOKEN` / `JIRA_API_KEY` out of a `.env.test`, and a developer's
ordinary shell very often already exports those — the prefix is what stops a
lane that talks to a real provider from arming itself by accident.

A provider is **configured** when every one of its required variables is set
and non-empty. Optional variables have a working default.

### GitHub — `toolkits.github.spec.ts`, `toolkits.agent.spec.ts`

| Variable | Required | Default |
|---|---|---|
| `E2E_LIVE_GITHUB_TOKEN` | yes | — |
| `E2E_LIVE_GITHUB_REPOSITORY` | yes | — (`owner/name`) |
| `E2E_LIVE_GITHUB_BASE_URL` | no | `https://api.github.com` |
| `E2E_LIVE_GITHUB_BRANCH` | no | `main` — also the evidence string |

### Jira — `toolkits.jira.spec.ts`, `toolkits.indicators.spec.ts`

| Variable | Required | Default |
|---|---|---|
| `E2E_LIVE_JIRA_BASE_URL` | yes | — |
| `E2E_LIVE_JIRA_USERNAME` | yes | — |
| `E2E_LIVE_JIRA_API_KEY` | yes | — |
| `E2E_LIVE_JIRA_PROJECT_KEY` | yes | — the evidence string |

### GitLab — `toolkits.gitlab.spec.ts`

| Variable | Required | Default |
|---|---|---|
| `E2E_LIVE_GITLAB_PRIVATE_TOKEN` | yes | — |
| `E2E_LIVE_GITLAB_REPOSITORY` | yes | — |
| `E2E_LIVE_GITLAB_URL` | no | `https://gitlab.com` |
| `E2E_LIVE_GITLAB_BRANCH` | no | `main` — also the evidence string |

### Bitbucket — `toolkits.bitbucket.spec.ts`

| Variable | Required | Default |
|---|---|---|
| `E2E_LIVE_BITBUCKET_USERNAME` | yes | — |
| `E2E_LIVE_BITBUCKET_TOKEN` | yes | — |
| `E2E_LIVE_BITBUCKET_PROJECT` | yes | — |
| `E2E_LIVE_BITBUCKET_REPOSITORY` | yes | — |
| `E2E_LIVE_BITBUCKET_URL` | no | `https://api.bitbucket.org` |
| `E2E_LIVE_BITBUCKET_BRANCH` | no | `master` — also the evidence string |

### Confluence — `toolkits.confluence.spec.ts`

| Variable | Required | Default |
|---|---|---|
| `E2E_LIVE_CONFLUENCE_BASE_URL` | yes | — |
| `E2E_LIVE_CONFLUENCE_USERNAME` | yes | — |
| `E2E_LIVE_CONFLUENCE_API_KEY` | yes | — |
| `E2E_LIVE_CONFLUENCE_SPACE` | yes | — the evidence string |
| `E2E_LIVE_CONFLUENCE_LABEL` | no | `test` |

### Image generation — `image.creation.spec.ts`, `image.imagegen-toolkit.spec.ts`

| Variable | Required | Default |
|---|---|---|
| `E2E_LIVE_IMAGE_MODEL` | yes | — the model name **as the picker spells it** |

Both files share this ONE variable — `image-live`'s prerequisite is
deliberately singular, per the table above. `image.creation.spec.ts` selects
the model through the ordinary chat picker (the `llm` configuration section).
`image.imagegen-toolkit.spec.ts` (the `imagegen` TOOLKIT, #864, distinct from
the built-in "image_generation" module the first file drives) resolves the
SAME name against the project's `image_generation` configuration SECTION
instead — a different lookup the gateway makes
(`internal/application/configurations/models.go`'s
`CurrentModelSectionImageGeneration`). A deployment that runs `image-live`
must therefore file the credential `E2E_LIVE_IMAGE_MODEL` names under BOTH
sections for both files to pass; one filed under only `llm` fails the second
file on the gateway's own "model is not configured for this project" — which
names exactly what is missing. See that file's own header for the full
account of why no mock-backed credential can stand in for this at all
(`streaming/chat.imagegen-toolkit.spec.ts`'s header has the SSRF-guard/
provider-capability audit).

## The "evidence string"

Each provider descriptor carries a `probeEvidence()` — a value only the real
provider can put in an answer (a branch name, a project key, a space key). It
replaces the legacy Test-Settings panel's raw-result read, and it must never
appear in the prompt: a model that never called the tool could otherwise
satisfy the assertion by repeating the question. `scripts/live-env.test.mjs` asserts
that separation for every provider.

## What is expected RED, and why it is here anyway

`toolkits.indicators.spec.ts` (LIVE-TK-IND-1..3) is written against the
intended behaviour and fails today on two named product gaps — no
toolkit-type connection check (`checkableConnectionTypes`, #319) and no
credential picker on the agent or pipeline page. Its own header states both
with file references. Ported as journeys rather than skips so the day either
gap closes, this lane says so.

## The CI job

`.github/workflows/ci-web-e2e.yml` carries a `workflow_dispatch` input
`live_toolkits`. The job maps `secrets.E2E_LIVE_*` into its environment and
asks Playwright how many tests the two live projects list. With no secrets
configured that count is 0, and the job prints which variables were missing
and passes — the count itself is asserted, so a job that passed because the
projects were broken rather than unconfigured fails instead.
