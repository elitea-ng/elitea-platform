/**
 * Public API — spec §3.3: named exports only.
 *
 * Unattended runs (ADR-0045 decision 1) as the composers show them: the
 * "Run to completion" control (on by default) and its choice (`RunChoice`),
 * the grant's desktop wire (`toWireGrant`) and cloud wire (`toCloudGrant`),
 * the agent tools a grant can name, the text of Main's refusal, and the flag
 * that hides the control when an administrator turned grants off. The run
 * brief and the goal check (decision 11) are in `entities/run-brief`.
 */
export { RunToCompletionControl } from './ui/RunToCompletionControl';
export { defaultGrant, newGrantId, toWireGrant, type SensitiveToolRef } from './model/grant';
export { defaultChoice, grantToSend, readChoice, type RunChoice } from './model/runChoice';
export { grantsTurnedOff, isDesktopGrantsOff, isGrantsOffRefusal, markGrantsTurnedOff, resetGrantsTurnedOff, useGrantsTurnedOff } from './model/grantsOff';
export { grantableTools, toCloudGrant, unattendedRefusalText } from './model/cloud';
