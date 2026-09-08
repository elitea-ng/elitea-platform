/**
 * The LIVE gitlab lane — ported from the legacy public suite's own
 * `tests/ui/toolkits` cases for this provider (see `liveToolkits.ts` for the
 * shape of both journeys and why the Test-Settings case is driven through a
 * chat turn rather than through a panel this platform does not compose).
 *
 * This file is LISTED only when every variable in
 * `LIVE_TOOLKIT_PROVIDERS.gitlab.requiredEnv` is set — see
 * `playwright.config.ts`'s `toolkits-live` project and `e2e/live/README.md`.
 * There is no `test.skip` here, and there must never be one: an unconfigured
 * provider contributes zero tests to the report instead of a green skip.
 */
import { LIVE_TOOLKIT_PROVIDERS } from './liveEnv';
import { registerLiveToolkitJourneys } from './liveToolkits';

registerLiveToolkitJourneys(LIVE_TOOLKIT_PROVIDERS.gitlab);
