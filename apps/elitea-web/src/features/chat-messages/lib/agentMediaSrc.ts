/**
 * Where an `<img>` or `<video>` loads an agent's avatar media and icon from (#3132).
 *
 * Main stores them as server paths: a pack's slots as `/api/v2/agent_media/<project>/<sha256>.<ext>`, an icon as its
 * `icon_meta.url` (`/…`). Rendered as they are, those paths resolve against the PAGE's origin:
 *
 * - on the web, where one gateway serves the app and Main, that is Main, and the path is used as it is; a deployment
 *   whose `vite_server_url` names another origin gets the path on that origin;
 * - in the desktop app the page's origin is the app itself (`tauri://localhost`), which has no such path, so every
 *   preview showed a broken image and chat drew no avatar. The desktop also loads images and media only from itself,
 *   `data:` and `blob:` (its CSP), and reaches the deployment only through the native transport (the host's fetch,
 *   `hostFetch.ts`). So the desktop fetches the file through it (`fetchServerFileBlob`) and shows it as a `blob:` URL. The files are content-addressed and
 *   immutable, so each is fetched once per app run and its `blob:` URL shared by every avatar that shows it.
 *
 * A URL that already names a scheme (`data:`, `blob:`, `https:`) is used as it is.
 */
import { useEffect, useState } from 'react';

import { fetchServerFileBlob } from '@/shared/api/artifacts';
import { getNativeTransport } from '@/shared/api/nativeTransport';
import { getConfig } from '@/shared/config/get-config';

/**
 * A path Main serves without a credential, as media elements load them: the pack media (`/api/v2/agent_media/…`) and
 * the uploaded icons (`/icons/…`). Other paths (the app's own default icons) are left to the page.
 */
function isServerPath(url: string): boolean {
  return url.startsWith('/api/') || url.startsWith('/icons/');
}

/** The deployment's origin from `vite_server_url`, when it names one other than the page's; else `undefined`. */
function serverOrigin(): string | undefined {
  const config = getConfig();
  if (config.status !== 'ok') return undefined;
  try {
    const origin = new URL(config.config.vite_server_url, globalThis.location?.href).origin;
    return origin === globalThis.location?.origin ? undefined : origin;
  } catch {
    return undefined;
  }
}

/** The desktop app (its native transport): media come through the host, as `blob:` URLs. */
const throughHost = (): boolean => getNativeTransport() !== undefined;

/** The URL the web loads the media from: a Main path on the deployment's origin, else as it is. */
export function webMediaUrl(url: string): string {
  if (!isServerPath(url)) return url;
  const origin = serverOrigin();
  return origin === undefined ? url : `${origin}${url}`;
}

/** The desktop's `blob:` URLs, by server path: fetched once per run (the files never change). */
const blobs = new Map<string, Promise<string | undefined>>();

function desktopBlob(url: string): Promise<string | undefined> {
  const known = blobs.get(url);
  if (known !== undefined) return known;
  const loading = fetchServerFileBlob(url)
    .then((file) => (file === undefined ? undefined : URL.createObjectURL(file)))
    .then((blob) => {
      // A failure is not kept: the next avatar that shows the file asks again.
      if (blob === undefined) blobs.delete(url);
      return blob;
    });
  blobs.set(url, loading);
  return loading;
}

/** Test-only: forgets the desktop's fetched files. */
export function resetAgentMediaForTests(): void {
  blobs.clear();
}

/**
 * The `src` for media stored at `url`: at once on the web; on the desktop once the host fetched it (`undefined`
 * until then, or when it could not be fetched, so the avatar shows its fallback).
 */
export function useAgentMediaSrc(url: string | undefined): string | undefined {
  const fetched = url !== undefined && throughHost() && isServerPath(url);
  const [loaded, setLoaded] = useState<{ url: string; src: string | undefined } | null>(null);
  useEffect(() => {
    if (!fetched) return undefined;
    let current = true;
    void desktopBlob(url).then((src) => {
      if (current) setLoaded({ url, src });
    });
    return () => {
      current = false;
    };
  }, [fetched, url]);
  if (url === undefined) return undefined;
  if (!fetched) return webMediaUrl(url);
  return loaded?.url === url ? loaded.src : undefined;
}
