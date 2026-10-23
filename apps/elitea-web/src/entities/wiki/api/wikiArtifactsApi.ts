/**
 * Reading wikis out of a project's artifact bucket.
 *
 * THE ROUTES ARE THE MODERN ONES, and that is not a preference. The vendored
 * bundle and the provider both speak
 * /api/v2/artifacts/artifact(s)/default/{projectId}/{bucket}[/{name}], which
 * elitea-main serves NO route in — proven six ways in
 * parity/notes/deepwiki-artifact-store.md and filed as a shipped defect. Those
 * paths 404 on the standalone Go stack and work only behind the legacy runtime.
 * Porting them would port a defect, so DWIKI-004 records them as waived and
 * this file uses /api/v2/artifacts/objects/... instead.
 *
 * eliteaFetch RETURNS THE ENVELOPE, not the body. The generated client's
 * mutator resolves to the whole `{ data, ... }` response shape, and typing a
 * call as the body gives an object whose fields are all undefined on a
 * successful 200 — the #132 defect, which happened twice on one endpoint.
 * Every reader here goes through `unwrapData`.
 */
import { eliteaFetch } from '@/shared/api/generated/mutator';

import type { WikiManifest, WikiObject } from '../model/types';
import { wikiPageObjectKey } from '../lib/pageKey';

/** The bucket wikis are stored in. Fixed by the provider at invoke time. */
// Not exported: nothing outside this module names the bucket yet. The
// settings feature (DWIKI-010) is what makes it configurable.
const WIKI_BUCKET = 'wiki-artifacts';

/** A wiki manifest object key, e.g. `acme--repo--main/wiki_manifest_2026….json`. */
const MANIFEST_KEY = /(^|\/)wiki_manifest[^/]*\.json$/;

interface ObjectListEnvelope {
  data?: { objects?: WikiObject[]; common_prefixes?: string[] };
  objects?: WikiObject[];
  common_prefixes?: string[];
}

/**
 * Unwrap the eliteaFetch envelope.
 *
 * Accepts both shapes because the envelope is applied by the transport and a
 * raw body is what the MSW fixtures serve. Returning `undefined` for neither is
 * deliberate — a caller that gets an empty list must be able to tell it from a
 * shape it did not understand.
 */
function unwrapData<T>(response: unknown, pick: (v: Record<string, unknown>) => T | undefined): T | undefined {
  if (!response || typeof response !== 'object') return undefined;
  const envelope = response as Record<string, unknown>;
  const inner = envelope.data;
  if (inner && typeof inner === 'object') {
    const fromInner = pick(inner as Record<string, unknown>);
    if (fromInner !== undefined) return fromInner;
  }
  return pick(envelope);
}

// NO /api/v2 PREFIX. eliteaFetch prepends the configured client baseUrl, which
// IS /api/v2 — writing it here too produces /api/v2/api/v2/... and a 404 that
// looks like an empty bucket. Every other entity API in this codebase passes a
// path relative to the base for the same reason.
function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

function objectsPath(projectId: string | number, bucket: string): string {
  return `/artifacts/objects/${encodeURIComponent(String(projectId))}/${encodeURIComponent(bucket)}`;
}

function objectPath(projectId: string | number, bucket: string, key: string): string {
  // The key is a PATH with slashes (`wiki_id/wiki_pages/section/page.md`), so
  // each segment is encoded separately: encodeURIComponent on the whole key
  // would turn its slashes into %2F and address an object that does not exist.
  const encodedKey = key.split('/').map(encodeURIComponent).join('/');
  return `${objectsPath(projectId, bucket)}/${encodedKey}`;
}

/** Every object in the project's wiki bucket. */
export async function listWikiObjects(
  projectId: string | number,
  bucket: string = WIKI_BUCKET,
): Promise<WikiObject[]> {
  const response = await eliteaFetch<ObjectListEnvelope>(objectsPath(projectId, bucket));
  return unwrapData(response, (v) => v.objects as WikiObject[] | undefined) ?? [];
}

/** The object keys that are wiki manifests. */
export function manifestKeys(objects: WikiObject[]): string[] {
  return objects.filter((o) => MANIFEST_KEY.test(o.key)).map((o) => o.key);
}

/** One wiki manifest. */
export async function fetchWikiManifest(
  projectId: string | number,
  key: string,
  bucket: string = WIKI_BUCKET,
): Promise<WikiManifest | undefined> {
  const response = await eliteaFetch<unknown>(objectPath(projectId, bucket, key));
  return unwrapData(response, (v) => (typeof v.wiki_id === 'string' ? (v as WikiManifest) : undefined));
}

/**
 * One wiki page's markdown.
 *
 * NOT `unwrapData`. A page is TEXT, not JSON: the transport hands back a string
 * in `data`, and running it through the JSON reader would return undefined for
 * every page.
 *
 * `key` is the manifest's own `wiki_id` joined to the path the manifest lists
 * in `pages[]`, because that is how the provider writes them:
 * `{wiki_id}/wiki_pages/{section}/{page}.md`. Asking without the wiki id asks
 * for a key that does not exist, and the page renders as "could not be loaded".
 */
export async function fetchWikiPage(
  projectId: string | number,
  wikiId: string,
  page: string,
  bucket: string = WIKI_BUCKET,
): Promise<string | undefined> {
  const response = await eliteaFetch<unknown>(objectPath(projectId, bucket, wikiPageObjectKey(wikiId, page)));
  const body = isRecord(response) && 'data' in response ? response.data : response;
  return typeof body === 'string' ? body : undefined;
}

/**
 * Write one wiki page back into the bucket.
 *
 * `overwrite=true`, because every caller here REPLACES: an edit saves over the
 * page it opened, a quick fix saves over the block it repaired. The object
 * route answers 409 AlreadyExists by default, and without the flag the second
 * save of any page would fail.
 *
 * The KEY travels as the multipart filename — appended as the third argument,
 * not wrapped in `new File(...)`, whose constructor replaces `/` with `:` and
 * would mangle every `{wiki_id}/wiki_pages/...` key. `eliteaFetch` passes a
 * FormData body through untouched, so `fetch` sets the multipart boundary.
 */
export async function putWikiPage(
  projectId: string | number,
  key: string,
  markdown: string,
  bucket: string = WIKI_BUCKET,
): Promise<void> {
  const body = new FormData();
  body.append('file', new Blob([markdown], { type: 'text/markdown' }), key);
  await eliteaFetch<unknown>(`${objectsPath(projectId, bucket)}?overwrite=true`, {
    method: 'POST',
    body,
  });
}
