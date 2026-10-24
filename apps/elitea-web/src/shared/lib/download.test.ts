/**
 * shared/lib/download.ts — spec §5.7 row 4 (unit S6).
 * Covers: the blob→anchor-click primitive (and its deliberate absence of a
 * beforeunload guard), Content-Disposition filename parsing, the export URL
 * assembly, and the fetch→blob→download orchestrator's failure paths.
 */
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { CORE_SCHEMA, load } from 'js-yaml';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { HttpResponse, http } from 'msw';

import { server } from '../../test/setup';
import {
  EXPORT_MARKDOWN,
  exportNetworkError,
  exportNoContentDisposition,
  exportNotFound,
  exportOk,
} from '../../test/msw/handlers/download';
import type { CapturedExportRequest } from '../../test/msw/handlers/download';

import {
  buildMarkdownExportUrl,
  downloadFromApi,
  exportMarkdown,
  filenameFromContentDisposition,
  triggerBlobDownload,
} from './download';

const ORIGIN = window.location.origin;

describe('the markdown export probe body', () => {
  it("is exportApplication's text/markdown example in v2.yaml", () => {
    // The handler's body is a constant because the generated mock cannot
    // serve one media type of three; this keeps it equal to the contract.
    const specPath = resolve(import.meta.dirname, '../../../../../services/elitea-main/api/openapi/v2.yaml');
    const spec = load(readFileSync(specPath, 'utf8'), { schema: CORE_SCHEMA }) as {
      paths: Record<string, { get: { responses: Record<string, { content: Record<string, { example?: unknown }> }> } }>;
    };
    const op = spec.paths['/elitea_core/export_import/prompt_lib/{project_id}/{id}'];
    expect(op?.get.responses['200']?.content['text/markdown']?.example).toBe(EXPORT_MARKDOWN);
  });
});

afterEach(() => {
  vi.unstubAllEnvs();
  vi.restoreAllMocks();
});

describe('triggerBlobDownload — blob → anchor-click, no beforeunload guard', () => {
  it('creates an object URL, clicks a detached anchor with the given filename, then revokes it', () => {
    const createSpy = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mock-url');
    const revokeSpy = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);

    triggerBlobDownload(new Blob(['x']), 'export.md');

    expect(createSpy).toHaveBeenCalledTimes(1);
    expect(clickSpy).toHaveBeenCalledTimes(1);
    expect(revokeSpy).toHaveBeenCalledWith('blob:mock-url');
    expect(document.body.querySelector('a[download]')).toBeNull(); // removed after click
  });

  it('never attaches a beforeunload listener (deliberate old-app parity — issue 3184)', () => {
    const addSpy = vi.spyOn(window, 'addEventListener');
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mock-url');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);

    triggerBlobDownload(new Blob(['x']), 'export.md');

    expect(addSpy).not.toHaveBeenCalledWith('beforeunload', expect.anything());
  });
});

describe('filenameFromContentDisposition — port of download.helpers.js:10-21', () => {
  it('prefers the RFC 5987 filename* parameter', () => {
    expect(filenameFromContentDisposition('attachment; filename="plain.md"; filename*=UTF-8\'\'%C3%A9toile.md', 'fallback.md')).toBe('étoile.md');
  });

  it('falls back to the legacy ASCII filename parameter', () => {
    expect(filenameFromContentDisposition('attachment; filename="plain.md"', 'fallback.md')).toBe('plain.md');
  });

  it('falls back to the default when neither parameter is present', () => {
    expect(filenameFromContentDisposition('attachment', 'fallback.md')).toBe('fallback.md');
  });

  it('falls back to the default for a null header', () => {
    expect(filenameFromContentDisposition(null, 'fallback.md')).toBe('fallback.md');
  });

  it('handles a malformed percent-escape without throwing', () => {
    expect(filenameFromContentDisposition("filename*=UTF-8''%", 'fallback.md')).toBe('%');
  });

  it('reads the branding package export header shape (ADR-0024 WP9)', () => {
    expect(filenameFromContentDisposition('attachment; filename="acme-branding.zip"', 'branding.zip')).toBe('acme-branding.zip');
    expect(filenameFromContentDisposition('inline', 'branding.zip')).toBe('branding.zip');
  });
});

describe('downloadFromApi — GET → blob → download (ADR-0024 WP9)', () => {
  const PACKAGE_PATH = '/admin/branding/package/administration';

  it('downloads under the Content-Disposition filename and resolves it', async () => {
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mock-url');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    server.use(
      http.get(`*${PACKAGE_PATH}`, () =>
        new HttpResponse(new Uint8Array([0x50, 0x4b, 0x03, 0x04]), {
          status: 200,
          headers: { 'Content-Type': 'application/zip', 'Content-Disposition': 'attachment; filename="acme-branding.zip"' },
        }),
      ),
    );

    const result = await downloadFromApi({ baseUrl: '/api/v2/', path: PACKAGE_PATH, fallbackName: 'branding.zip' });

    expect(result).toEqual({ ok: true, filename: 'acme-branding.zip' });
    expect(clickSpy).toHaveBeenCalledTimes(1);
    // A trailing slash on the base is not doubled, and the request carries the session.
    const [url, init] = fetchSpy.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(`/api/v2${PACKAGE_PATH}`);
    expect(init.credentials).toBe('same-origin');
  });

  it('falls back to the given name when the server sends no filename', async () => {
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mock-url');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    server.use(http.get(`*${PACKAGE_PATH}`, () => new HttpResponse('zip', { status: 200, headers: { 'Content-Type': 'application/zip' } })));

    const result = await downloadFromApi({ baseUrl: '/api/v2', path: PACKAGE_PATH, fallbackName: 'branding.zip' });
    expect(result).toEqual({ ok: true, filename: 'branding.zip' });
  });

  it("resolves an http failure with the server's own reason and never downloads", async () => {
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    server.use(
      http.get(`*${PACKAGE_PATH}`, () =>
        HttpResponse.json({ error: 'branding packages are not available on this deployment' }, { status: 503 }),
      ),
    );

    const result = await downloadFromApi({ baseUrl: '/api/v2', path: PACKAGE_PATH, fallbackName: 'branding.zip' });
    expect(result).toEqual({
      ok: false,
      error: { kind: 'http', status: 503, reason: 'branding packages are not available on this deployment' },
    });
    expect(clickSpy).not.toHaveBeenCalled();
  });

  it('resolves an http failure without a reason when the body is not the {error} shape', async () => {
    server.use(http.get(`*${PACKAGE_PATH}`, () => new HttpResponse('nope', { status: 500, headers: { 'Content-Type': 'text/plain' } })));
    const result = await downloadFromApi({ baseUrl: '/api/v2', path: PACKAGE_PATH, fallbackName: 'branding.zip' });
    expect(result).toEqual({ ok: false, error: { kind: 'http', status: 500, reason: undefined } });
  });

  it('resolves a network failure and an aborted signal without throwing', async () => {
    server.use(http.get(`*${PACKAGE_PATH}`, () => HttpResponse.error()));
    const network = await downloadFromApi({ baseUrl: '/api/v2', path: PACKAGE_PATH, fallbackName: 'branding.zip' });
    expect(network.ok).toBe(false);
    if (network.ok) throw new Error('unreachable');
    expect(network.error.kind).toBe('network');

    const controller = new AbortController();
    controller.abort();
    const aborted = await downloadFromApi({ baseUrl: '/api/v2', path: PACKAGE_PATH, fallbackName: 'branding.zip', signal: controller.signal });
    expect(aborted).toEqual({ ok: false, error: { kind: 'aborted' } });
  });
});

describe('buildMarkdownExportUrl — port of useExport.js:70-76', () => {
  it('always includes format=md', () => {
    expect(buildMarkdownExportUrl('/api/v2', 'p1', 'app1')).toBe('/api/v2/elitea_core/export_import/prompt_lib/p1/app1?format=md');
  });

  it('appends follow_version_ids joined by comma when given', () => {
    const url = buildMarkdownExportUrl('/api/v2', 'p1', 'app1', ['v1', 'v2']);
    expect(url).toBe('/api/v2/elitea_core/export_import/prompt_lib/p1/app1?format=md&follow_version_ids=v1%2Cv2');
  });

  it('omits follow_version_ids when the array is empty', () => {
    expect(buildMarkdownExportUrl('/api/v2', 'p1', 'app1', [])).toBe('/api/v2/elitea_core/export_import/prompt_lib/p1/app1?format=md');
  });

  it('does NOT strip /api/v2 — this endpoint is used as-is, unlike artifacts.ts rows 1/5', () => {
    expect(buildMarkdownExportUrl('/api/v2', 'p1', 'app1')).toContain('/api/v2');
  });
});

describe('exportMarkdown — fetch → blob → download orchestrator', () => {
  it('downloads using the Content-Disposition filename and reports it', async () => {
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mock-url');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    server.use(exportOk('my-agent.md'));

    const result = await exportMarkdown({ baseUrl: '/api/v2', projectId: 'p1', applicationId: 'app1' }, 'fallback-name');

    expect(result).toEqual({ ok: true, filename: 'my-agent.md' });
    expect(clickSpy).toHaveBeenCalledTimes(1);
  });

  it('falls back to `${fallbackName}.md` when the server sends no Content-Disposition', async () => {
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mock-url');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    server.use(exportNoContentDisposition());

    const result = await exportMarkdown({ baseUrl: '/api/v2', projectId: 'p1', applicationId: 'app1' }, 'fallback-name');

    expect(result).toEqual({ ok: true, filename: 'fallback-name.md' });
  });

  it('attaches the DEV bearer only under import.meta.env.DEV', async () => {
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mock-url');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    const sink: CapturedExportRequest[] = [];
    server.use(exportOk('a.md', sink));

    await exportMarkdown({ baseUrl: '/api/v2', projectId: 'p1', applicationId: 'app1', devToken: 'dev-secret' }, 'fallback');
    expect(sink[0]?.authorization).toBe('Bearer dev-secret');
  });

  it('attaches no bearer outside DEV', async () => {
    vi.stubEnv('DEV', false);
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mock-url');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    const sink: CapturedExportRequest[] = [];
    server.use(exportOk('a.md', sink));

    await exportMarkdown({ baseUrl: '/api/v2', projectId: 'p1', applicationId: 'app1', devToken: 'dev-secret' }, 'fallback');
    expect(sink[0]?.authorization).toBeNull();
  });

  it('resolves a kind:http failure for a 404, never throws, and never downloads', async () => {
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    server.use(exportNotFound());

    const result = await exportMarkdown({ baseUrl: '/api/v2', projectId: 'p1', applicationId: 'app1' }, 'fallback');
    expect(result).toEqual({ ok: false, error: { kind: 'http', status: 404 } });
    expect(clickSpy).not.toHaveBeenCalled();
  });

  it('resolves a kind:network failure for a network error, never throws', async () => {
    server.use(exportNetworkError());
    const result = await exportMarkdown({ baseUrl: '/api/v2', projectId: 'p1', applicationId: 'app1' }, 'fallback');
    expect(result.ok).toBe(false);
    if (result.ok) throw new Error('unreachable');
    expect(result.error.kind).toBe('network');
  });

  it('resolves a kind:aborted failure for a pre-aborted signal', async () => {
    server.use(exportOk('a.md'));
    const controller = new AbortController();
    controller.abort();
    const result = await exportMarkdown({ baseUrl: '/api/v2', projectId: 'p1', applicationId: 'app1', signal: controller.signal }, 'fallback');
    expect(result).toEqual({ ok: false, error: { kind: 'aborted' } });
  });

  it('resolves same-origin credentials for the relative /api/v2 base', async () => {
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mock-url');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    const spy = vi.spyOn(globalThis, 'fetch');
    server.use(exportOk('a.md'));

    await exportMarkdown({ baseUrl: '/api/v2', projectId: 'p1', applicationId: 'app1' }, 'fallback');
    const init = spy.mock.calls[0]?.[1] as RequestInit;
    expect(init.credentials).toBe('same-origin');
  });

  it('resolves include credentials for a cross-origin absolute base', async () => {
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mock-url');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    const spy = vi.spyOn(globalThis, 'fetch');
    server.use(exportOk('a.md'));

    await exportMarkdown({ baseUrl: `${ORIGIN.replace('localhost', 'cross-origin.example')}/api/v2`, projectId: 'p1', applicationId: 'app1' }, 'fallback');
    const init = spy.mock.calls[0]?.[1] as RequestInit;
    expect(init.credentials).toBe('include');
  });
});
