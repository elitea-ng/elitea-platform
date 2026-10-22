/**
 * MSW handlers for `shared/api/artifacts.ts`. Same R-M2/R-M3/R-M4 discipline
 * as transport.ts (unit F4): every response body derives from a fixture
 * file, validated at registration time.
 *
 * Routing note: every artifact route is mounted UNDER `/api/v2/artifacts`
 * by `mountArtifactRoutes` (router.go:255-311). These patterns previously
 * matched the legacy runtime plugin's root-level `/artifacts/s3/*` and
 * `/artifacts/artifact/default/*`, which elitea-main does not serve —
 * issue #138.
 */
import { http, HttpResponse } from 'msw';
import type { RequestHandler } from 'msw';
import { z } from 'zod';
import type { ZodType } from 'zod';

import { registerValidatedHandlers } from '../register';
import artifactContent200 from '../fixtures/artifacts/artifact-content.200.json';
import artifactList200 from '../fixtures/artifacts/artifact-list.200.json';
import bucketList200 from '../fixtures/artifacts/bucket-list.200.json';
import notFound404 from '../fixtures/artifacts/error.404.json';
import objectUpload201 from '../fixtures/artifacts/object-upload.201.json';

const objectUploadBody = z.object({ key: z.string(), size_bytes: z.number(), etag: z.string() });
const bucketListBody = z.object({
  buckets: z.array(z.object({ name: z.string(), is_pinned: z.boolean(), created_at: z.string() })),
});
const artifactListBody = z.object({
  objects: z.array(z.object({ key: z.string(), size_bytes: z.number(), modified_at: z.string() })),
  common_prefixes: z.array(z.string()),
});
const contentBody = z.string();
const errorBody = z.object({ error: z.string() });

/** Request facts captured for assertions (headers/credentials, §5.7 probes). */
export interface CapturedArtifactsRequest {
  url: string;
  method: string;
  credentials: RequestCredentials;
  authorization: string | null;
  contentType: string | null;
  bodyText: string | null;
}

async function capture(request: Request, sink?: CapturedArtifactsRequest[]): Promise<void> {
  if (sink === undefined) return;
  const bodyText = request.method === 'GET' || request.method === 'HEAD' ? null : await request.clone().text();
  sink.push({
    url: request.url,
    method: request.method,
    credentials: request.credentials,
    authorization: request.headers.get('authorization'),
    contentType: request.headers.get('content-type'),
    bodyText,
  });
}

/** R-M3 seam: validate one entry through F2's registration wrapper. */
function validated(id: string, schema: ZodType, fixture: { recordedAt: string; body: unknown }, handler: RequestHandler): RequestHandler {
  const [first] = registerValidatedHandlers([{ id, handler, schema, fixture }]);
  if (first === undefined) throw new Error(`artifacts handlers: registration returned nothing for ${id}`);
  return first;
}

/* ── upload: POST /api/v2/artifacts/objects/{projectID}/{bucket} ─────────── */

/**
 * The object-plane list and upload share one path — only the method differs
 * — so `http.post` and `http.get` on the same pattern are how the two are
 * told apart, exactly as chi tells them apart on the server.
 */
const OBJECTS_COLLECTION = '*/api/v2/artifacts/objects/:projectId/:bucket';
const OBJECT_ITEM = '*/api/v2/artifacts/objects/:projectId/:bucket/*';
const BUCKETS_COLLECTION = '*/api/v2/artifacts/buckets/:projectId';

export function objectUploadOk(sink?: CapturedArtifactsRequest[]): RequestHandler {
  return validated('artifacts.object.upload.ok', objectUploadBody, objectUpload201, http.post(OBJECTS_COLLECTION, async ({ request }) => {
    await capture(request, sink);
    return HttpResponse.json(objectUpload201.body, { status: 201 });
  }));
}

export function objectUploadNotFound(): RequestHandler {
  return validated('artifacts.object.upload.notFound', errorBody, notFound404, http.post(OBJECTS_COLLECTION, () =>
    HttpResponse.json(notFound404.body, { status: 404 }),
  ));
}

export function objectUploadNetworkError(): RequestHandler {
  return http.post(OBJECTS_COLLECTION, () => HttpResponse.error());
}

/* ── bucket / object list ─────────────────────────────────────────────────── */

export function bucketListOk(sink?: CapturedArtifactsRequest[]): RequestHandler {
  return validated('artifacts.bucketList.ok', bucketListBody, bucketList200, http.get(BUCKETS_COLLECTION, async ({ request }) => {
    await capture(request, sink);
    return HttpResponse.json(bucketList200.body);
  }));
}

export function bucketListNetworkError(): RequestHandler {
  return http.get(BUCKETS_COLLECTION, () => HttpResponse.error());
}

export function bucketListNotFound(): RequestHandler {
  return validated('artifacts.bucketList.notFound', errorBody, notFound404, http.get(BUCKETS_COLLECTION, () =>
    HttpResponse.json(notFound404.body, { status: 404 }),
  ));
}

/** 200 with a non-JSON body — exercises parseJsonBody's catch (raw-text) branch. */
export function bucketListLyingJson(): RequestHandler {
  return http.get(BUCKETS_COLLECTION, () => new HttpResponse('not json', { status: 200, headers: { 'Content-Type': 'application/json' } }));
}

export function artifactListOk(sink?: CapturedArtifactsRequest[]): RequestHandler {
  return validated('artifacts.artifactList.ok', artifactListBody, artifactList200, http.get(OBJECTS_COLLECTION, async ({ request }) => {
    await capture(request, sink);
    return HttpResponse.json(artifactList200.body);
  }));
}

export function artifactListNetworkError(): RequestHandler {
  return http.get(OBJECTS_COLLECTION, () => HttpResponse.error());
}

/* ── object download ──────────────────────────────────────────────────────── */

export function artifactContentOk(sink?: CapturedArtifactsRequest[]): RequestHandler {
  return validated('artifacts.content.ok', contentBody, artifactContent200, http.get(OBJECT_ITEM, async ({ request }) => {
    await capture(request, sink);
    return HttpResponse.text(artifactContent200.body);
  }));
}

export function artifactContentNotFound(): RequestHandler {
  return validated('artifacts.content.notFound', errorBody, notFound404, http.get(OBJECT_ITEM, () =>
    HttpResponse.json(notFound404.body, { status: 404 }),
  ));
}

export function artifactContentNetworkError(): RequestHandler {
  return http.get(OBJECT_ITEM, () => HttpResponse.error());
}

/** Serves a distinct blob per requested file path — the ZIP-loop probe. */
export function artifactContentByPath(bodies: Readonly<Record<string, string>>, sink?: CapturedArtifactsRequest[]): RequestHandler {
  return http.get(OBJECT_ITEM, async ({ request }) => {
    await capture(request, sink);
    const pathname = new URL(request.url).pathname;
    const key = Object.keys(bodies).find((k) => pathname.endsWith(k));
    const body = key === undefined ? undefined : bodies[key];
    if (body === undefined) return new HttpResponse(null, { status: 404 });
    return HttpResponse.text(body);
  });
}
