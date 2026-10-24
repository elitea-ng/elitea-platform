/**
 * MSW handlers for unit S6's markdown-export transport (spec §5.7 row 4).
 *
 * The route is GET /api/v2/elitea_core/export_import/prompt_lib/{projectId}/
 * {applicationId} (exportApplication). Its 200 has three media types in
 * v2.yaml: the JSON document, the ?format=md markdown, and the multi-version
 * zip. orval's generated handler for such an operation answers an empty
 * octet-stream whatever the body, so:
 *  - `exportDocumentDefault()` is the default registry's handler for the
 *    route (handlers/index.ts lists it ahead of the generated ones): the
 *    generated JSON document mock, as JSON, every time;
 *  - the markdown body is a typed constant (R-M2); download.test.ts checks it
 *    equals the spec's `text/markdown` example.
 */
import { http, HttpResponse } from 'msw';
import type { RequestHandler } from 'msw';

import { getExportApplicationResponseMock } from '../../../shared/api/generated/applications/applications.msw';
import type { ErrorResponse } from '../../../shared/api/generated/model';

/** The shape export_markdown.go writes: YAML frontmatter, then the instructions. */
export const EXPORT_MARKDOWN: string = [
  '---',
  'name: Exported Agent',
  'description: A synthetic agent used by the export probe.',
  'model: gpt-4o',
  'temperature: 0.7',
  'agent_type: react',
  '---',
  '',
  "Answer the user's question.",
].join('\n');
/** ExportImportGet's own not-found and apierr.Write are both the flat shape. */
const NOT_FOUND: ErrorResponse = { error: 'not found' };

const EXPORT_PATH = '*/elitea_core/export_import/prompt_lib/*';

export interface CapturedExportRequest {
  url: string;
  authorization: string | null;
}

/** The default registry's answer: the JSON export document. */
export function exportDocumentDefault(): RequestHandler {
  return http.get(EXPORT_PATH, () => HttpResponse.json(getExportApplicationResponseMock()));
}

/** 200 with a Content-Disposition header carrying `filename`. */
export function exportOk(filename: string, sink?: CapturedExportRequest[]): RequestHandler {
  return http.get(EXPORT_PATH, ({ request }) => {
    sink?.push({ url: request.url, authorization: request.headers.get('authorization') });
    return new HttpResponse(EXPORT_MARKDOWN, {
      status: 200,
      headers: { 'Content-Type': 'text/markdown', 'Content-Disposition': `attachment; filename="${filename}"` },
    });
  });
}

/** 200 with NO Content-Disposition header — the fallback-filename branch. */
export function exportNoContentDisposition(): RequestHandler {
  return http.get(EXPORT_PATH, () =>
    new HttpResponse(EXPORT_MARKDOWN, { status: 200, headers: { 'Content-Type': 'text/markdown' } }),
  );
}

export function exportNotFound(): RequestHandler {
  return http.get(EXPORT_PATH, () => HttpResponse.json(NOT_FOUND, { status: 404 }));
}

export function exportNetworkError(): RequestHandler {
  return http.get(EXPORT_PATH, () => HttpResponse.error());
}
