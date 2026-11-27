/**
 * Public API — per-folder trust on the desktop (ADR-0061 decision 5, #3062):
 * the trust dialog, the "No sandbox" / "Trust paused" badge with Revoke,
 * the folder menu's item, the `sandbox_unavailable` offer and Settings ›
 * Trusted folders. Reached only from the `desktop` build's modules.
 */
export { TrustBadge } from './ui/TrustBadge';
export { TrustedFoldersList } from './ui/TrustedFoldersList';
/** The desktop harness hands in its own client (`entries/desktop/devHarness.tsx`). */
export { TrustIpcProvider } from './model/useFolderTrust';
export { SandboxUnavailableOffer, useFolderTrustBadged, useFolderTrusted, useFolderTrustMenu } from './ui/trustEntryPoints';
