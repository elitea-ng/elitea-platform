/**
 * Journey: an image and a PDF stored in a bucket preview in the artifacts
 * browser's own pane. The image is an `<img>` on a blob URL (an SVG uploaded
 * without a type included: it shows only when the preview types it from the
 * file name); the PDF is drawn by pdf.js onto a `<canvas>`, one page at a
 * time, with its worker served from the app's own origin under the CSP the
 * nginx image sends.
 */
import { test, expect, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

function uniqueBucket(): string {
  return `autotest-prv-${Date.now().toString(36)}${Math.floor(Math.random() * 1000)}`;
}

/** A two-page PDF: text in a non-embedded standard font and a filled rectangle. */
function twoPagePdf(): Buffer {
  const stream = 'BT /F1 24 Tf 40 100 Td (Hello PDF) Tj ET 0 0 1 rg 20 20 80 40 re f';
  const objects = [
    '<< /Type /Catalog /Pages 2 0 R >>',
    '<< /Type /Pages /Kids [3 0 R 5 0 R] /Count 2 >>',
    '<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 200] /Contents 4 0 R /Resources << /Font << /F1 6 0 R >> >> >>',
    `<< /Length ${stream.length} >>\nstream\n${stream}\nendstream`,
    '<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 200] /Contents 4 0 R /Resources << /Font << /F1 6 0 R >> >> >>',
    '<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>',
  ];
  let body = '%PDF-1.4\n';
  const offsets: number[] = [];
  objects.forEach((object, index) => {
    offsets.push(body.length);
    body += `${index + 1} 0 obj\n${object}\nendobj\n`;
  });
  const xref = body.length;
  body += `xref\n0 ${objects.length + 1}\n0000000000 65535 f \n`;
  body += offsets.map((offset) => `${String(offset).padStart(10, '0')} 00000 n \n`).join('');
  body += `trailer\n<< /Size ${objects.length + 1} /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF\n`;
  return Buffer.from(body, 'latin1');
}

const SVG = '<svg xmlns="http://www.w3.org/2000/svg" width="40" height="20"><rect width="40" height="20" fill="#36c"/></svg>';

async function selectedProjectId(page: Page): Promise<string> {
  const id = await page.evaluate(() => localStorage.getItem('el.project.id'));
  expect(id, 'app-shell must have persisted a selected project').not.toBeNull();
  return id as string;
}

async function seed(request: APIRequestContext, projectId: string, bucket: string): Promise<void> {
  const created = await request.post(`/api/v2/artifacts/buckets/${projectId}`, { data: { name: bucket } });
  expect([200, 201, 409]).toContain(created.status());
  const files = [
    { name: 'report.pdf', mimeType: 'application/pdf', buffer: twoPagePdf() },
    { name: 'logo.svg', mimeType: 'application/octet-stream', buffer: Buffer.from(SVG) },
  ];
  for (const file of files) {
    const uploaded = await request.post(`/api/v2/artifacts/objects/${projectId}/${bucket}?overwrite=true`, {
      multipart: { file },
    });
    expect(uploaded.status(), await uploaded.text()).toBe(201);
  }
}

/** Settle on the app shell, not the URL: WebKit can report the URL before the shell renders. */
async function openPreview(page: Page, bucket: string, file: string): Promise<void> {
  await page.goto(`${BASE_URL}/app/artifacts?bucket=${bucket}&file=${file}`);
  await expect(page.getByTestId('sidebar-collapse-toggle')).toBeVisible({ timeout: 30_000 });
}

test('a stored PDF and SVG preview as a paged canvas and an image', async ({ page, request }) => {
  test.setTimeout(120_000);
  const bucket = uniqueBucket();
  await page.goto(BASE_URL + '/app/artifacts');
  await expect(page.getByTestId('sidebar-collapse-toggle')).toBeVisible({ timeout: 30_000 });
  const projectId = await selectedProjectId(page);
  await seed(request, projectId, bucket);

  try {
    await openPreview(page, bucket, 'report.pdf');
    const indicator = page.getByTestId('artifacts-pdf-page-indicator');
    await expect(indicator).toHaveText('Page 1 of 2', { timeout: 30_000 });
    const canvas = page.getByTestId('artifacts-pdf-canvas');
    await expect(canvas).toBeVisible();
    await expect.poll(() => canvas.evaluate((element) => (element as HTMLCanvasElement).width)).toBeGreaterThan(0);
    await expect(page.getByRole('button', { name: 'Save' })).toHaveCount(0);
    await page.getByRole('button', { name: 'Next page' }).click();
    await expect(indicator).toHaveText('Page 2 of 2');

    await openPreview(page, bucket, 'logo.svg');
    const image = page.getByRole('img', { name: 'logo.svg' });
    await expect(image).toBeVisible({ timeout: 30_000 });
    await expect.poll(() => image.evaluate((element) => (element as HTMLImageElement).naturalWidth)).toBe(40);
  } finally {
    await request.delete(`/api/v2/artifacts/buckets/${projectId}/${bucket}`);
  }
});
