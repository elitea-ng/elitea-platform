import { z } from 'zod';

import { anonymousFetch, readJson, type AnonymousFetchOptions } from '@/api/anonymous';
import { getClientVersion } from '@/api/clientVersion';
import { isApiError } from '@/api/errors';
import type { components } from '@/api/generated/elitea.gen';
import { isSameOrigin, originOf } from '@/api/paths';
import { identity } from '@/config/identity';
import { log } from '@/lib/log';
import { isBelowMinimum } from '@/lib/semver';

/**
 * The anonymous discovery document (ADR-0025 decision 1,
 * `GET {origin}/.well-known/elitea-client`): what the app learns about a
 * workspace before anything is stored or any sign-in starts.
 *
 * The document is validated with zod (it is untrusted input), and every URL
 * in it must be on the origin the user chose: the authorization, token and
 * revocation endpoints, the issuer and the brand pack. A document that points
 * elsewhere is refused outright, because following it would send the user's
 * sign-in (and later the refresh token) to a host they never confirmed.
 */

/** Path of the document, at the origin root (RFC 8615). */
export const DISCOVERY_PATH = '/.well-known/elitea-client';
/** The only client contract major version this app speaks. */
export const SUPPORTED_CONTRACT_MAJOR = 1;
/** The native brand pack route; the discovery `brand_pack_url` must point at it. */
export const BRAND_PACK_PATH = '/api/v2/branding/pack.json';

const VERSION_TEXT = z.string().max(64);
const URL_TEXT = z.string().min(1).max(2048);

const NativeAuthSchema = z.looseObject({
  issuer: URL_TEXT,
  authorization_endpoint: URL_TEXT,
  token_endpoint: URL_TEXT,
  revocation_endpoint: URL_TEXT,
  code_challenge_methods_supported: z.array(z.string().max(32)).max(16),
});

const PublicPolicySchema = z.looseObject({
  require_device_lock: z.boolean(),
  offline_enabled: z.boolean(),
  min_client_version: VERSION_TEXT,
});

export const DiscoveryDocumentSchema = z.looseObject({
  server_version: z.string().max(32),
  client_contract: z.string().max(16),
  deployment_kind: z.enum(['saas', 'self_hosted']),
  display_name: z.string().max(200),
  brand_pack_url: URL_TEXT,
  native_auth: NativeAuthSchema.nullable(),
  client_policy: PublicPolicySchema,
  min_client_version: z.record(z.string().max(128), VERSION_TEXT),
});

export type DiscoveryDocument = z.infer<typeof DiscoveryDocumentSchema>;
export type NativeAuthEndpoints = z.infer<typeof NativeAuthSchema>;

// The runtime schema must stay assignable to the contract's type (overlay
// written against server commit 398985cdc); a drift fails typecheck here.
type Assignable<A, B> = A extends B ? true : false;
const contractCheck: Assignable<DiscoveryDocument, components['schemas']['DiscoveryDocument']> = true;
void contractCheck;

export type DiscoveryFailure =
  /** 404: the server predates ADR-0025 (or is not an Elitea workspace). */
  | 'not_supported'
  /** 503 or another non-2xx answer: retry later. */
  | 'unavailable'
  /** No HTTP answer (offline, DNS, TLS, timeout). */
  | 'unreachable'
  /** The body is not a valid discovery document. */
  | 'invalid'
  /** The document names a URL on another origin (see `canonicalOrigin`). */
  | 'origin_mismatch';

export type DiscoveryResult =
  | { ok: true; document: DiscoveryDocument }
  | { ok: false; error: DiscoveryFailure; canonicalOrigin?: string };

/** Last good document per origin, with its entity tag (revalidated with If-None-Match). */
const cache = new Map<string, { etag: string | null; document: DiscoveryDocument }>();

export function getCachedDiscovery(origin: string): DiscoveryDocument | null {
  return cache.get(origin)?.document ?? null;
}

/** Validates the URLs inside a parsed document against the chosen origin. */
export function checkDiscoveryOrigins(
  origin: string,
  document: DiscoveryDocument,
): { ok: true } | { ok: false; canonicalOrigin?: string } {
  const packUrl = document.brand_pack_url;
  if (!isSameOrigin(packUrl, origin) || !packUrl.slice(originOf(packUrl)!.length).startsWith(BRAND_PACK_PATH)) {
    return { ok: false, canonicalOrigin: originOf(packUrl) ?? undefined };
  }
  const nativeAuth = document.native_auth;
  if (nativeAuth !== null) {
    const urls = [
      nativeAuth.issuer,
      nativeAuth.authorization_endpoint,
      nativeAuth.token_endpoint,
      nativeAuth.revocation_endpoint,
    ];
    if (!urls.every((url) => isSameOrigin(url, origin))) {
      return { ok: false, canonicalOrigin: originOf(nativeAuth.issuer) ?? undefined };
    }
  }
  return { ok: true };
}

export async function fetchDiscovery(origin: string, options: AnonymousFetchOptions = {}): Promise<DiscoveryResult> {
  const base = originOf(origin);
  if (base === null) return { ok: false, error: 'invalid' };
  const cached = cache.get(base);
  const headers: Record<string, string> = {};
  if (cached?.etag) headers['If-None-Match'] = cached.etag;

  let response: Response;
  try {
    response = await anonymousFetch(base, `${base}${DISCOVERY_PATH}`, { headers }, options);
  } catch (error) {
    if (isApiError(error) && error.kind === 'origin_mismatch') return { ok: false, error: 'origin_mismatch' };
    log.info('discovery request failed', { error });
    return { ok: false, error: 'unreachable' };
  }

  if (response.status === 304 && cached) return { ok: true, document: cached.document };
  if (response.status === 404) return { ok: false, error: 'not_supported' };
  if (response.status !== 200) return { ok: false, error: 'unavailable' };

  const parsed = DiscoveryDocumentSchema.safeParse(await readJson(response));
  if (!parsed.success) {
    log.warn('discovery document rejected', { issues: parsed.error.issues.length });
    return { ok: false, error: 'invalid' };
  }
  const origins = checkDiscoveryOrigins(base, parsed.data);
  if (!origins.ok) {
    return { ok: false, error: 'origin_mismatch', canonicalOrigin: origins.canonicalOrigin };
  }
  cache.set(base, { etag: response.headers.get('ETag'), document: parsed.data });
  return { ok: true, document: parsed.data };
}

/** What the app can do with a workspace, judged from its discovery document. */
export type DiscoveryAssessment =
  | { kind: 'ready' }
  /** No registered native client (`native_auth: null`), or no S256 PKCE. */
  | { kind: 'native_auth_unavailable' }
  /** The server speaks a client contract major this app does not. */
  | { kind: 'contract_unsupported'; serverContract: string }
  /** This app is older than the workspace's minimum. */
  | { kind: 'upgrade_required'; minVersion: string };

/** The effective minimum for this app: its own entry, else the deployment-wide one. */
export function minimumVersionFor(document: DiscoveryDocument, clientId: string = identity.bundleId): string {
  return document.min_client_version[clientId] ?? document.client_policy.min_client_version;
}

export function contractMajor(contract: string): number | null {
  const match = /^(\d{1,4})(?:\.\d{1,6})?$/.exec(contract.trim());
  return match ? Number(match[1]) : null;
}

export function assessDiscovery(
  document: DiscoveryDocument,
  clientVersion: string = getClientVersion(),
  clientId: string = identity.bundleId,
): DiscoveryAssessment {
  if (contractMajor(document.client_contract) !== SUPPORTED_CONTRACT_MAJOR) {
    return { kind: 'contract_unsupported', serverContract: document.client_contract };
  }
  const minimum = minimumVersionFor(document, clientId);
  if (isBelowMinimum(clientVersion, minimum)) return { kind: 'upgrade_required', minVersion: minimum };
  if (document.native_auth === null || !document.native_auth.code_challenge_methods_supported.includes('S256')) {
    return { kind: 'native_auth_unavailable' };
  }
  return { kind: 'ready' };
}

/** Test helper. */
export function __clearDiscoveryCacheForTests(): void {
  cache.clear();
}
