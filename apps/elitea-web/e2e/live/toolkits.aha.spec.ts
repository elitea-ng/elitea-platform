/**
 * The LIVE Aha! lane — the highest-value read case from the legacy public
 * suite's `toolkits-credentials/aha-toolkit` package (see
 * `S/port/ledger-P8-toolkits-A.tsv` for the other 73 cases' verdicts).
 *
 * This file is LISTED only when every variable in
 * `LIVE_TOOLKIT_PROVIDERS.aha.requiredEnv` is set — see
 * `playwright.config.ts`'s `toolkits-live` project and `e2e/live/README.md`.
 * There is no `test.skip` here, and there must never be one: an unconfigured
 * provider contributes zero tests to the report instead of a green skip.
 */
import { LIVE_TOOLKIT_PROVIDERS } from './liveEnv';
import { registerLiveToolkitJourneys } from './liveToolkits';

registerLiveToolkitJourneys(LIVE_TOOLKIT_PROVIDERS.aha);
