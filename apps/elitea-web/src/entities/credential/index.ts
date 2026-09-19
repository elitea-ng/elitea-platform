/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 *
 * `CredentialWarningBanner` and the `toolkitCredentialHealth` group moved
 * here from `features/credentials` for #937: the "Credential setup required:"
 * banner has to reach an Agent's, a Pipeline's and a Chat participant's tool
 * card, all of which live in feature slices that `no-sideways-features`
 * forbids to import `features/credentials`. `features/credentials` still
 * re-exports the banner under its own `CredentialWarning.Banner`, so no
 * existing caller loses reach.
 */
export type { Credential, CredentialPage, ModelInfo } from './model/types';
export { CredentialWarningBanner } from './ui/CredentialWarningBanner';
export type { CredentialWarningBannerProps } from './ui/CredentialWarningBanner';
export {
  collectCredentialTitles,
  isToolkitCredentialMissing,
  readToolkitCredentialReference,
} from './model/toolkitCredentialHealth';
export type { CredentialTitleSource, ToolkitCredentialReference } from './model/toolkitCredentialHealth';
export {
  credentialDisplayName,
  credentialScope,
  credentialUrl,
  providerDisplayName,
  sortCredentialsPinnedFirst,
} from './model/selectors';
