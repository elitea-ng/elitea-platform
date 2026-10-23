import {
  assessDiscovery,
  fetchDiscovery,
  type DiscoveryAssessment,
  type DiscoveryDocument,
  type DiscoveryFailure,
} from '@/api/discovery';
import { originOf } from '@/api/paths';
import { identity } from '@/config/identity';
import { parseConnectLink } from '@/linking/parseConnectLink';
import { acceptDiscovery } from '@/state/workspaceInfo';
import { connectWorkspace } from '@/state/workspace';
import { fetchBrandTheme, type BrandTheme } from '@/theme/brandPack';
import { setBrandTheme } from '@/theme/brandStore';

/**
 * Connecting to a workspace is two steps: CHECK (read the anonymous discovery
 * document, store nothing) and CONFIRM (the user has seen the host, the
 * workspace name and what it supports; only then is the origin stored).
 */

export type ConnectCheck =
  | { kind: 'failed'; origin: string; error: DiscoveryFailure; canonicalOrigin?: string }
  | {
      kind: 'confirm';
      origin: string;
      document: DiscoveryDocument;
      assessment: DiscoveryAssessment;
      /** The workspace's brand (logo, name, colours) for the confirmation; null when unavailable. */
      brand: BrandTheme | null;
    };

export interface ConnectFlowDeps {
  fetchDiscovery?: typeof fetchDiscovery;
  fetchBrandTheme?: typeof fetchBrandTheme;
  clientVersion?: string;
  /** Plain http to a private host is acceptable (development builds only; defaults to `__DEV__`). */
  allowPrivateHttp?: boolean;
}

/**
 * A canonical origin offered by a refused discovery document, or undefined.
 * It must pass the same address rules as a typed one (https only, plain http
 * solely for a private host in development) and already be normalised: the
 * confirmation shows it without its scheme, so an http:// value would read
 * exactly like the secure one.
 */
export function offerableCanonical(candidate: string | undefined, allowPrivateHttp: boolean): string | undefined {
  if (!candidate || originOf(candidate) !== candidate) return undefined;
  const parsed = parseConnectLink(candidate, { connectSchemes: identity.connectSchemes, allowPrivateHttp });
  return parsed.ok && parsed.origin === candidate ? candidate : undefined;
}

export async function checkWorkspace(origin: string, deps: ConnectFlowDeps = {}): Promise<ConnectCheck> {
  const result = await (deps.fetchDiscovery ?? fetchDiscovery)(origin);
  if (!result.ok) {
    // Only offer a canonical address that a user could have typed here.
    const canonical = offerableCanonical(result.canonicalOrigin, deps.allowPrivateHttp ?? __DEV__);
    return { kind: 'failed', origin, error: result.error, canonicalOrigin: canonical };
  }
  // The brand is public and decorative: a failure leaves the confirmation unbranded.
  const brand = await (deps.fetchBrandTheme ?? fetchBrandTheme)(origin, result.document.brand_pack_url);
  return {
    kind: 'confirm',
    origin,
    document: result.document,
    assessment: assessDiscovery(result.document, deps.clientVersion),
    brand,
  };
}

/**
 * Whether "Connect" may proceed. A workspace without native sign-in (or a
 * server without discovery at all) is still connectable in development
 * builds, which can sign in with a personal access token.
 */
export function canConnect(check: ConnectCheck, devSignIn: boolean): boolean {
  if (check.kind === 'failed') return devSignIn && check.error === 'not_supported';
  switch (check.assessment.kind) {
    case 'ready':
      return true;
    case 'native_auth_unavailable':
      return devSignIn;
    default:
      return false;
  }
}

/** The user confirmed: store the origin, then publish the document. */
export async function confirmConnection(check: ConnectCheck): Promise<boolean> {
  const stored = await connectWorkspace(check.origin);
  if (stored && check.kind === 'confirm') {
    // Apply the previewed brand first so publishing the document does not refetch it.
    if (check.brand) setBrandTheme(check.brand, check.document.brand_pack_url);
    acceptDiscovery(check.origin, check.document);
  }
  return stored;
}

/** `ai.example.com` (with a port when there is one) for the prominent host line. */
export function displayHost(origin: string): string {
  return origin.replace(/^https?:\/\//i, '');
}
