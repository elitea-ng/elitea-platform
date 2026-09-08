/**
 * The credential connection-test verdict, in the one place both
 * `./useCredentialValidation.ts` (the hook) and
 * `./credentialValidation.helpers.ts` (its pure helpers) can import it from.
 *
 * Its own module rather than the hook's file because the helpers are called
 * BY the hook: importing the type back out of the hook would close a cycle
 * that `scripts/check-layer-cycle.mjs` refuses.
 */
export type CredentialValidationStatus = 'idle' | 'checking' | 'valid' | 'invalid' | 'unsupported';

/**
 * The toolkit probe's closed refusal vocabulary
 * (`internal/api/v2/configurations/toolkit_check.go`). These two mean the
 * platform reached a verdict ABOUT THE CREDENTIAL: the provider rejected it,
 * or nothing answered at all. Every other refusal shape — a missing probe, a
 * dependency this deployment does not compose ("Connection checking is not
 * available right now.") — carries no `reason` at all, and must never be read
 * as a statement about the credential.
 */
const CREDENTIAL_REFUSAL_REASONS: readonly string[] = ['auth_failed', 'unreachable'];

/** True only for a verdict that refuses THE CREDENTIAL — see {@link CREDENTIAL_REFUSAL_REASONS}. */
export function isCredentialRefusalReason(reason: string | undefined): boolean {
  return reason !== undefined && CREDENTIAL_REFUSAL_REASONS.includes(reason);
}
