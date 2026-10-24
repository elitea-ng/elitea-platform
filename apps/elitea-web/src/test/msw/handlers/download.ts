/**
 * MSW handlers for unit S6's markdown-export transport (spec §5.7 row 4).
 *
 * The route is GET /api/v2/elitea_core/export_import/prompt_lib/{projectId}/
 * {applicationId}?format=md (exportApplication). v2.yaml documents the
 * markdown body as a `text/markdown` 200 with an example, but the same 200
 * also has the JSON export document, and orval's generated mock folds the two
 * media types into one random `faker.helpers.arrayElement([...])`. A test
 * cannot pick the markdown one through it, so the body is a typed constant
 * here (R-M2), equal to the spec's example.
 */
import { http, HttpResponse } from 'msw';
import type { RequestHandler } from 'msw';

import type { ErrorResponse } from '../../../shared/api/generated/model';

/** The shape export_markdown.go writes: YAML frontmatter, then the instructions. */
const EXPORT_MARKDOWN = [
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
