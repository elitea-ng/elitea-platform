/**
 * Public API — spec §3.3: named exports only.
 *
 * Unattended runs (ADR-0045 decision 1): the grant as the person edits it, its
 * desktop wire (`toWireGrant`), the grant form and its read-only summary.
 * Used by the desktop composer's Unattended mode; the cloud wiring reuses the
 * form with `cloud` and `sensitiveTools`, its cloud wire (`toCloudGrant`), the
 * agent tools a grant can name and the text of Main's refusal.
 */
export { UnattendedGrantForm, type SensitiveToolOption, type UnattendedGrantFormProps } from './ui/UnattendedGrantForm';
export { UnattendedGrantSummary } from './ui/UnattendedGrantSummary';
export {
  chainErrors,
  defaultGrant,
  newGrantId,
  readGrant,
  toWireGrant,
  type ChainDraft,
  type SensitiveToolRef,
  type UnattendedChain,
  type UnattendedGrant,
  type UnattendedGrantValue,
  type WireGrantResult,
} from './model/grant';
export { grantableTools, toCloudGrant, unattendedRefusalText, type CloudGrantResult } from './model/cloud';
