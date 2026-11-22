/**
 * features/microsoft-login — the web sign-in of the Microsoft credentials (Outlook, Teams,
 * Microsoft Graph, Dataverse; ADR-0057 decision 6, epic 2254 story S1d).
 *
 * It owns the flow (popup, handoff on postMessage and BroadcastChannel matched on
 * `state`, redeem) and the credential form's sign-in block. It owns no token store: the
 * caller keeps the access token where runs read it, through `onSignedIn`. The slice
 * imports nothing from another feature, so the page that composes it wires the store.
 */
export { useMicrosoftLogin } from './model/useMicrosoftLogin';
export type { SignedIn, UseMicrosoftLoginResult } from './model/useMicrosoftLogin';
export type { LoginSession } from './lib/session';
export { MicrosoftLoginPanel } from './ui/MicrosoftLoginPanel';
export { isMicrosoftCredentialType } from './lib/credentialKey';
export { readSession } from './lib/session';
