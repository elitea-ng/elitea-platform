/**
 * Wire fixtures for the ADR-0025 native-client endpoints, shaped after the
 * server's handlers at elitea-platform commit 398985cdc (branch
 * feat/adr-0025-native-client-support): key names, key order, null-ness and
 * value formats match what those handlers marshal. Restated here as data; no
 * server code is copied.
 */

export const FIXTURE_ORIGIN = 'https://ai.example.com';

/**
 * Discovery with a registered native client: `native_auth` endpoints are the
 * public origin plus `/api/v2/auth/native/{authorize,token,revoke}`, S256 only;
 * the brand pack URL is content-addressed with the pack's ETag value.
 */
export function discoveryDocument(overrides: Record<string, unknown> = {}) {
  return {
    server_version: '1.62',
    client_contract: '1.0',
    deployment_kind: 'self_hosted',
    display_name: 'Elitea',
    brand_pack_url: `${FIXTURE_ORIGIN}/api/v2/branding/pack.json?v=4f1c2b9e0a7d`,
    native_auth: {
      issuer: FIXTURE_ORIGIN,
      authorization_endpoint: `${FIXTURE_ORIGIN}/api/v2/auth/native/authorize`,
      token_endpoint: `${FIXTURE_ORIGIN}/api/v2/auth/native/token`,
      revocation_endpoint: `${FIXTURE_ORIGIN}/api/v2/auth/native/revoke`,
      code_challenge_methods_supported: ['S256'],
    },
    client_policy: { require_device_lock: true, offline_enabled: false, min_client_version: '' },
    min_client_version: {},
    ...overrides,
  };
}

/** Discovery on a deployment with no registered native client (the server's defaults). */
export function discoveryWithoutNativeAuth() {
  return discoveryDocument({
    native_auth: null,
    client_policy: { require_device_lock: false, offline_enabled: true, min_client_version: '' },
  });
}

/** The full policy the token endpoint adds to every 200 (`client_policy`). */
export function clientPolicy(overrides: Record<string, unknown> = {}) {
  return {
    require_device_lock: true,
    idle_lock_seconds: 120,
    allow_screenshots: false,
    offline_retention_days: 0,
    offline_max_mb: 512,
    offline_attachments: false,
    min_client_version: '',
    ...overrides,
  };
}

/** Opaque token of the server's shape: prefix plus 43 base64url characters. */
export function opaqueToken(prefix: 'elnat_' | 'elnrt_', n: number): string {
  return `${prefix}${String(n).padStart(43, 'x')}`;
}

/** A token endpoint 200: opaque `elnat_` / `elnrt_` tokens, 15 min / 30 days. */
export function tokenResponse(n = 1, overrides: Record<string, unknown> = {}) {
  return {
    access_token: opaqueToken('elnat_', n),
    token_type: 'Bearer',
    expires_in: 900,
    refresh_token: opaqueToken('elnrt_', n),
    refresh_token_expires_in: 2592000,
    device_id: '7b0a3f9e-2c51-4d1a-9a59-6f3c1d2e8b40',
    client_policy: clientPolicy(),
    ...overrides,
  };
}

/** RFC 6749 §5.2 error body the native endpoints write. */
export function oauthError(error: string, description = 'refused') {
  return { error, error_description: description };
}

/** The flat 401 body for a revoked, expired, deactivated or reused device session. */
export const DEVICE_REVOKED_BODY = {
  error: 'device_revoked',
  error_description: "This device's sign-in was revoked. Sign in again.",
};

/** The nested 401 envelope the API answers an expired (not revoked) access token with. */
export const TOKEN_REJECTED_BODY = {
  error: { message: 'token validation failed', type: 'authentication_error', code: 'token_rejected' },
};

/** The 426 body (also carried by `X-Min-Client-Version`). */
export function upgradeRequiredBody(minimum = '0.2.0') {
  return {
    error: 'client_upgrade_required',
    error_description: 'This version of the app is no longer supported by this deployment. Update the app.',
    min_client_version: minimum,
  };
}

/**
 * A brand pack as `pack.json` serves it: asset references absolute against the
 * public origin, the scheme token records in the web app's vocabulary. Only
 * the token ids the app maps are included; their values are those of the
 * server's product default pack at 398985cdc, plus a gradient the mapper must
 * ignore.
 */
export function brandPack(overrides: Record<string, unknown> = {}) {
  return {
    $schema: 'https://elitea.ai/schemas/brand-pack/1.json',
    id: 'default',
    version: '1.0.0',
    product: { name: 'Elitea', shortName: 'Elitea', docsUrl: `${FIXTURE_ORIGIN}/docs/` },
    assets: {
      logoFull: `${FIXTURE_ORIGIN}/brand/logo-full.svg`,
      logoMark: `${FIXTURE_ORIGIN}/brand/logo-mark.svg`,
      favicon: `${FIXTURE_ORIGIN}/brand/favicon.svg`,
    },
    typography: {
      fontFamily: '"Montserrat", Roboto, Arial, sans-serif',
      fontFamilyMono: '"Roboto Mono", Consolas, "Courier New", monospace',
      baseSize: 14,
      scale: 1.2,
    },
    shape: { radiusSm: 4, radiusMd: 8, radiusLg: 16, radiusPill: 9999, density: 'comfortable' },
    locale: { default: 'en-GB', dateLocale: 'en-GB' },
    brand: { hue: '#6ae8fa' },
    schemes: {
      light: {
        'background.default': 'rgba(248, 252, 255, 1)',
        'background.secondary': '#FFFFFF',
        'background.agentModal.background': 'linear-gradient(224.97deg, #DEEDFF 0%, #F9DFFE 100%)',
        'border.lines': 'rgba(203, 206, 214, 1)',
        'error.main': '#D71616',
        'primary.main': 'rgba(196, 40, 221, 1)',
        'text.button.primary': 'rgba(248, 252, 255, 1)',
        'text.primary': '#777A83',
        'text.secondary': '#0E131D',
      },
      dark: {
        'background.default': '#0E131D',
        'background.secondary': '#181F2A',
        'border.lines': '#3B3E46',
        'error.main': '#D71616',
        'primary.main': '#6ae8fa',
        'text.button.primary': '#0E131D',
        'text.primary': '#A9B7C1',
        'text.secondary': '#FFFFFF',
      },
    },
    ...overrides,
  };
}

/**
 * Client contract 1.1 `attachments` block, as the v4.2.0 demo deployment
 * served it (`GET /.well-known/elitea-client`, 2026-10-05): the server's
 * defaults (150 MB files, 3 MB images, 5 MiB chunks, 25 MiB extraction,
 * 48 KiB inline images, server-side downscale on).
 */
export function attachmentPolicy(overrides: Record<string, unknown> = {}) {
  return {
    max_files: 10,
    max_total_bytes: 157286400,
    max_file_bytes: 157286400,
    max_image_bytes: 3145728,
    chunk_bytes: 5242880,
    accepted_extensions: [
      '.csv', '.doc', '.docx', '.gif', '.htm', '.html', '.jpeg', '.jpg', '.json', '.md', '.pdf',
      '.png', '.pptx', '.svg', '.txt', '.webp', '.xls', '.xlsx', '.xml', '.yaml', '.yml',
    ],
    max_extract_bytes: 26214400,
    inline_image_max_bytes: 49152,
    inline_image_formats: ['.gif', '.jpeg', '.jpg', '.png', '.webp'],
    inline_image_downscale: true,
    ...overrides,
  };
}

/** A client contract 1.1 discovery document (v4.2.0): `attachments` added. */
export function discoveryDocument11(overrides: Record<string, unknown> = {}) {
  return discoveryDocument({ server_version: '4.2', client_contract: '1.1', attachments: attachmentPolicy(), ...overrides });
}
