import { useSyncExternalStore } from 'react';
import { z } from 'zod';

import type { DiscoveryDocument } from '@/api/discovery';
import { log } from '@/lib/log';
import { isEndingSession, registerWipeHook } from '@/security/endSession';
import { SECURE_KEYS, getSecureItem, setSecureItem } from '@/security/secureStorage';
import { getSession } from '@/state/session';
import { onDiscovery } from '@/state/workspaceInfo';
import { acceptImage, brandFontAlias, fetchBrandTheme, mapScheme, type BrandTheme } from '@/theme/brandPack';

/**
 * The applied brand theme of the connected workspace.
 *
 * It follows the discovery document: whenever a document arrives (connect,
 * launch refresh) with a brand pack URL not yet applied, the pack is fetched
 * and mapped in the background; the UI never waits for it and keeps the
 * fallback palette until then. The mapped theme (not the pack) is kept in
 * secure storage so the next launch starts branded; it is part of the
 * registry and therefore wiped with the workspace.
 */

let theme: BrandTheme | null = null;
let appliedPackUrl: string | null = null;
const listeners = new Set<() => void>();

function emit() {
  for (const listener of [...listeners]) listener();
}

export function getBrandTheme(): BrandTheme | null {
  return theme;
}

export function subscribeBrandTheme(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function useBrandTheme(): BrandTheme | null {
  return useSyncExternalStore(subscribeBrandTheme, getBrandTheme, getBrandTheme);
}

/** The persisted form stays well under SecureStore's ~2 KB value budget. */
const MAX_PERSISTED_BYTES = 2000;

export function setBrandTheme(next: BrandTheme | null, packUrl: string | null = null): void {
  theme = next;
  appliedPackUrl = next === null ? null : packUrl;
  emit();
  if (next !== null) void persist(next);
}

/** The session is being wiped: nothing may be (re)stored for it. */
function sessionEnding(): boolean {
  return getSession().ending || isEndingSession();
}

async function persist(value: BrandTheme): Promise<void> {
  // A write landing after endSession's secure delete would outlive the wipe.
  if (sessionEnding()) return;
  // Inline data: logos are not persisted (size); they come back with the next fetch.
  const slim = {
    ...value,
    logoMark: value.logoMark?.uri.startsWith('data:') ? null : value.logoMark,
    logoFull: value.logoFull?.uri.startsWith('data:') ? null : value.logoFull,
  };
  const json = JSON.stringify(slim);
  if (json.length > MAX_PERSISTED_BYTES) return;
  try {
    await setSecureItem(SECURE_KEYS.brandTheme, json);
  } catch (error) {
    log.info('brand theme not persisted', { error });
  }
}

const PersistedSchema = z.object({
  origin: z.string(),
  productName: z.string().max(80),
  productDefault: z.boolean(),
  logoMark: z.object({ uri: z.string(), kind: z.enum(['svg', 'raster']) }).nullable(),
  logoFull: z.object({ uri: z.string(), kind: z.enum(['svg', 'raster']) }).nullable(),
  light: z.record(z.string(), z.string()),
  dark: z.record(z.string(), z.string()),
  fontFaces: z.array(z.object({ alias: z.string(), url: z.string() })).max(4),
  fontFamily: z.string().nullable(),
});

/**
 * Launch: restores the persisted theme for `origin`. Everything is
 * re-validated as if it came from the server (same-origin images, plain
 * colours with contrast), so a tampered value cannot widen what is loaded.
 */
export async function restoreBrandTheme(origin: string): Promise<void> {
  let raw: string | null;
  try {
    raw = await getSecureItem(SECURE_KEYS.brandTheme);
  } catch {
    return;
  }
  if (raw === null) return;
  try {
    const parsed = PersistedSchema.parse(JSON.parse(raw));
    if (parsed.origin !== origin) return;
    const image = (value: { uri: string } | null) => (value ? acceptImage(value.uri, origin) : null);
    theme = {
      ...parsed,
      logoMark: image(parsed.logoMark),
      logoFull: image(parsed.logoFull),
      light: mapScheme(schemeTokens(parsed.light), ''),
      dark: mapScheme(schemeTokens(parsed.dark), ''),
      ...restoredFonts(parsed.fontFaces, parsed.fontFamily, origin),
    };
    emit();
  } catch {
    log.info('persisted brand theme ignored');
  }
}

/**
 * The persisted faces of `origin`, renamed with the current alias (a theme
 * stored by an older build carries a weaker name that another workspace
 * could collide with).
 */
function restoredFonts(
  faces: readonly { alias: string; url: string }[],
  family: string | null,
  origin: string,
): Pick<BrandTheme, 'fontFaces' | 'fontFamily'> {
  const kept = faces
    .filter((face) => face.url.startsWith(`${origin}/`))
    .map((face) => ({ alias: brandFontAlias(origin, face.url), url: face.url }));
  if (family === null || kept.length === 0) return { fontFaces: [], fontFamily: null };
  return { fontFaces: kept, fontFamily: kept[0].alias };
}

/** Re-expresses an app palette as pack token ids, so `mapScheme` re-validates it. */
function schemeTokens(palette: Record<string, string>): Record<string, string> {
  const ids: Record<string, string> = {
    text: 'text.secondary',
    textSecondary: 'text.primary',
    background: 'background.default',
    backgroundElement: 'background.secondary',
    border: 'border.lines',
    accent: 'primary.main',
    onAccent: 'text.button.primary',
    danger: 'error.main',
  };
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(palette)) if (ids[key]) out[ids[key]] = value;
  return out;
}

/**
 * Applies the pack named by a discovery document when it is not the one
 * already applied. Exposed for tests; wired below to every accepted document.
 */
export async function applyBrandFromDiscovery(
  origin: string,
  document: Pick<DiscoveryDocument, 'brand_pack_url'>,
  fetchTheme: typeof fetchBrandTheme = fetchBrandTheme,
): Promise<void> {
  if (appliedPackUrl === document.brand_pack_url && theme?.origin === origin) return;
  const next = await fetchTheme(origin, document.brand_pack_url);
  // Workspace changed (or wiped, or being wiped) while fetching: drop it.
  if (next === null || getSession().serverOrigin !== origin || sessionEnding()) return;
  setBrandTheme(next, document.brand_pack_url);
}

onDiscovery((origin, document) => {
  void applyBrandFromDiscovery(origin, document);
});

registerWipeHook('brand_theme', () => {
  theme = null;
  appliedPackUrl = null;
  emit();
});

/** Test helper. */
export function __resetBrandThemeForTests(): void {
  theme = null;
  appliedPackUrl = null;
  listeners.clear();
}
