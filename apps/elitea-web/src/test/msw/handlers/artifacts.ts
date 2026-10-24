/**
 * MSW handlers for `shared/api/artifacts.ts`. R-M2 (spec §6.5): a JSON body
 * is the operation's example in v2.yaml, served through orval's generated
 * `get<Op>ResponseMock()`; what the contract has no example for (raw object
 * bytes, error bodies) is a typed constant here.
 *
 * Routing note: every artifact route is mounted UNDER `/api/v2/artifacts`
 * by `mountArtifactRoutes` (router.go:255-311). These patterns previously
 * matched the legacy runtime plugin's root-level `/artifacts/s3/*` and
 * `/artifacts/artifact/default/*`, which elitea-main does not serve —
 * issue #138.
 */
import { http, HttpResponse } from 'msw';
import type { RequestHandler } from 'msw';

import {
  getListBucketsResponseMock,
  getListObjectsResponseMock,
  getUploadObjectResponseMock,
} from '../../../shared/api/generated/artifacts/artifacts.msw';
import type { Error as ArtifactError } from '../../../shared/api/generated/model';

/** Object bytes for a download — the route answers application/octet-stream. */
const ARTIFACT_CONTENT = 'hello artifact content';
/** The artifact API's error envelope (artifacts/handler.go writeError). */
const NOT_FOUND: ArtifactError = { error: { code: 'NotFound', message: 'not found' } };

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
  return http.post(OBJECTS_COLLECTION, async ({ request }) => {
    await capture(request, sink);
    return HttpResponse.json(getUploadObjectResponseMock(), { status: 201 });
  });
}

export function objectUploadNotFound(): RequestHandler {
  return http.post(OBJECTS_COLLECTION, () => HttpResponse.json(NOT_FOUND, { status: 404 }));
}

export function objectUploadNetworkError(): RequestHandler {
  return http.post(OBJECTS_COLLECTION, () => HttpResponse.error());
}

/* ── bucket / object list ─────────────────────────────────────────────────── */

export function bucketListOk(sink?: CapturedArtifactsRequest[]): RequestHandler {
  return http.get(BUCKETS_COLLECTION, async ({ request }) => {
    await capture(request, sink);
    return HttpResponse.json(getListBucketsResponseMock());
  });
}

export function bucketListNetworkError(): RequestHandler {
  return http.get(BUCKETS_COLLECTION, () => HttpResponse.error());
}

export function bucketListNotFound(): RequestHandler {
  return http.get(BUCKETS_COLLECTION, () => HttpResponse.json(NOT_FOUND, { status: 404 }));
}

/** 200 with a non-JSON body — exercises parseJsonBody's catch (raw-text) branch. */
export function bucketListLyingJson(): RequestHandler {
  return http.get(BUCKETS_COLLECTION, () => new HttpResponse('not json', { status: 200, headers: { 'Content-Type': 'application/json' } }));
}

export function artifactListOk(sink?: CapturedArtifactsRequest[]): RequestHandler {
  return http.get(OBJECTS_COLLECTION, async ({ request }) => {
    await capture(request, sink);
    return HttpResponse.json(getListObjectsResponseMock());
  });
}

export function artifactListNetworkError(): RequestHandler {
  return http.get(OBJECTS_COLLECTION, () => HttpResponse.error());
}

/* ── object download ──────────────────────────────────────────────────────── */

export function artifactContentOk(sink?: CapturedArtifactsRequest[]): RequestHandler {
  return http.get(OBJECT_ITEM, async ({ request }) => {
    await capture(request, sink);
    return HttpResponse.text(ARTIFACT_CONTENT);
  });
}

export function artifactContentNotFound(): RequestHandler {
  return http.get(OBJECT_ITEM, () => HttpResponse.json(NOT_FOUND, { status: 404 }));
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
