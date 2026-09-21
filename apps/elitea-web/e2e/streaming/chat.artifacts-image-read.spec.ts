/**
 * READING AN IMAGE the model can actually see — the onetest `artifacts` image
 * cases (ELITEA-0298..0307) and the chat-attachment one (ELITEA-0510),
 * #939 group 5.
 *
 * ── The capability these cases were waiting for ────────────────────────────
 * `deploy/mock-llm/server.py` was a text echo: "the reply is an echo of the
 * last user message", with `IMAGE_MODEL`/`TINY_PNG_B64` serving only the
 * imagegen toolkit's WRITE path. Nothing on the READ side, so a case that
 * needs the model to SEE bytes could not be scripted at all. The mock now
 * decodes the image parts of a request (OpenAI `image_url` data URLs and
 * Anthropic `source.data`), identifies the format from the magic number, and
 * answers a deterministic sentence keyed by the MD5 OF THE BYTES — which is
 * what makes "it was shown the picture", "it told the formats apart" and "the
 * second read reused the first description" all assertable instead of
 * plausible.
 *
 * ── Where the assertions are read ─────────────────────────────────────────
 * From the mock's own REQUEST JOURNAL (`readMockLlmJournal`, `images[]` with
 * `{md5, format, bytes}`), not only from the answer. An answer that says
 * "this is a PNG" is written by the same model that was told to say it; the
 * journal entry is the wire, and it is the only place that can show the bytes
 * ever left the platform.
 *
 * ── Why the python leg ────────────────────────────────────────────────────
 * The image path is the SDK's: `read_file` fails to decode the object as text,
 * `parse_file_content` picks `EliteAImageLoader` by extension, and THAT calls
 * the model with the image part. The native worker's `artifact` family (#906)
 * returns text or a `content_too_large` object and has no loader stack, so on
 * the rust leg these cases assert the absence of a feature rather than the
 * feature. One leg guard beats nine skipped assertions.
 */
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import type { Page } from '@playwright/test';
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  callToolWithArgumentsPrompt,
  clearMockLlmJournal,
  createAgentWithVersion,
  expectStoredAssistantAnswer,
  readCallerIdentity,
  readCallerPersonalProjectId,
  readMockLlmJournal,
  readStoredTranscript,
} from '../fixtures/api';

const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

test.skip(
  (process.env['E2E_WORKER'] ?? 'rust') !== 'python',
  'the artifact image path is the SDK loader stack — the native worker has no image loader',
);

/** Every test owns the whole journal window: an image another test showed the model is not this one's. */
test.beforeEach(async ({ page }) => {
  await clearMockLlmJournal(page);
});

/**
 * One 288×288 picture in each format the SDK's `image_loaders_map` lists as
 * directly LLM-supported, each with its own colours and shape so no two share
 * a digest. Real files, not stand-ins: the mock identifies the format from the
 * magic number, and the loader opens them with PIL on the way out.
 *
 * ABOVE 256px ON THE LONG EDGE, deliberately. `EliteAImageLoader` calls
 * `ensure_min_image_size(image, min_dim=256)` first, and anything smaller is
 * upscaled — which sets `_was_scaled`, drops the ORIGINAL bytes, and sends
 * `encode_image_for_llm`'s re-encode instead. Measured on this stack: at 1×1
 * and at 96×96 every format reached the model as a re-encoded PNG (a fact
 * about the placeholder, not about the platform); at 288×288 the pass-through
 * branch of `encode_image_bytes_for_llm` keeps each file's own format, which
 * is what the case is actually about.
 */
const IMAGES = {
  png:
    'iVBORw0KGgoAAAANSUhEUgAAASAAAAEgCAIAAACb4TnXAAADV0lEQVR4nO3VUQ3CABQEQUrQUYwUX0VBhVUJTtAAyeYlzYyC+9ncsm77DWjcpwfAlQkMQgKDkMAgJDAICQxCAoOQwCAkMAgJDEICg5DAICQwCAkMQgKDkMAgJDAICQxCAoOQwCAkMAgJDEICg5DAICQwCAkMQgKDkMAgJDAICQxCAoOQwCAkMAgJDEICg5DAICQwCAkMQgKDkMAgJDAICQxCAoOQwCAkMAgJDEICg5DAICQwCAkMQgKDkMAgJDAICQxCAoOQwCAkMAgJDEICg9BjesA/PucxPYEZz9d7esJvPBiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQWtZtn94Al+XBICQwCAkMQgKDkMAgJDAICQxCAoOQwCAkMAgJDEICg5DAICQwCAkMQgKDkMAgJDAICQxCAoOQwCAkMAgJDEICg5DAICQwCAkMQgKDkMAgJDAICQxCAoOQwCAkMAgJDEICg5DAICQwCAkMQgKDkMAgJDAICQxCAoOQwCAkMAgJDEICg5DAICQwCAkMQgKDkMAgJDAICQxCAoOQwCAkMAh9AfvbBuVooZbiAAAAAElFTkSuQmCC',
  jpeg:
    '/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDABQODxIPDRQSEBIXFRQYHjIhHhwcHj0sLiQySUBMS0dARkVQWnNiUFVtVkVGZIhlbXd7gYKBTmCNl4x9lnN+gXz/2wBDARUXFx4aHjshITt8U0ZTfHx8fHx8fHx8fHx8fHx8fHx8fHx8fHx8fHx8fHx8fHx8fHx8fHx8fHx8fHx8fHx8fHz/wAARCAEgASADASIAAhEBAxEB/8QAHwAAAQUBAQEBAQEAAAAAAAAAAAECAwQFBgcICQoL/8QAtRAAAgEDAwIEAwUFBAQAAAF9AQIDAAQRBRIhMUEGE1FhByJxFDKBkaEII0KxwRVS0fAkM2JyggkKFhcYGRolJicoKSo0NTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqDhIWGh4iJipKTlJWWl5iZmqKjpKWmp6ipqrKztLW2t7i5usLDxMXGx8jJytLT1NXW19jZ2uHi4+Tl5ufo6erx8vP09fb3+Pn6/8QAHwEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoL/8QAtREAAgECBAQDBAcFBAQAAQJ3AAECAxEEBSExBhJBUQdhcRMiMoEIFEKRobHBCSMzUvAVYnLRChYkNOEl8RcYGRomJygpKjU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6goOEhYaHiImKkpOUlZaXmJmaoqOkpaanqKmqsrO0tba3uLm6wsPExcbHyMnK0tPU1dbX2Nna4uPk5ebn6Onq8vP09fb3+Pn6/9oADAMBAAIRAxEAPwDKooorlOwKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiimSypCu5zj0Hc0wH0ySaOL77ge3es+a/d8iMbF9e9VCSSSTknvWih3MnUXQ0H1Ef8ALOMnjqxqBr6djkMF9gP8arUVaikZucmPM0pBBkcg9txplFFUQFPWWRRhZGA9ATTKKALCXs64+bcB2IqdNR6CSP6lT/SqFFS4plKckbMVxFKBscZPY9alrBqzDfSx8N86+/X86h0+xqqvc1aKihuI5xlDz6HrUtZ7GqdwooopAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUVSvLvZmOI/N3YdqpK4m0ldkl1drB8qjc/p6VmO7SNudix96bRW0YpHNKTkFFFFUSFFFFABRRRQAUUUUAFFFFABRRRQAqsVOVJB9RWja3okISThux9azaKlxTKjJxN6is6zu9mI5T8vZj2rRrFqx0RkpIKKKKkoKKKKACiiigAooooAKKKKACiiigAooooAKKKZNKsMZdu3QeppgQ3lz5ChV++3T2rKp0jmR2dupOabW8Y2RyylzMKKKKokKKKKACiiigAooooAKKKKACiiigAooooAKKKKACtGxud4ET9QPlPqKzqASCCDgjvSauioy5Xc3qKhtpxPEG4DD7wHapq52rHUnfUKKKKQBRRRQAUUUUAFFFFABRRRQAUUUUAFZmoTb5fLU/KnX61oTSeVE7+g4+tYhJJJJyT3rWC6mVR6WCiiitTAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAns5vJmGT8rcGtesGte0l823Uk5YcGsqi6m1J9CeiiisjYKKKKACiiigAooooAKKKKACiiigCjqb/AConHJyfWs+rN+xa5IP8IAH8/wCtVq6IqyOabvIKKKKogKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKuaa+JWTj5hn8qp1LasVuYyP72Pz4pSV0VF2Zs0UUVzHUFFFFABRRRQAUUUUAFFFFABRRRQBiTkGeQg5BY8/jTKKK6jjCiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooA3QQQCDkHvS1HB/x7x/7g/lUlcx2IKKKKQBRRRQAUUUUAFFFFABRRRQBg0U+ZQszqvADECmV1HGFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQBtQf8e8f+4P5VJSKoVQq8ADApa5jsQUUUUgCiiigAooooAKKKKACiiigDJvl23LcYDYIqvV/U0+44HsT/L+tUK6Iu6OWatIKKKKokKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKkt13zxrjPzDI9qjq3pybpixHCjr6H/ADmk3ZFRV2adFFFcx1BRRRQAUUUUAFFFFABRRRQAUUUUARXEXmwsuMnGR9axq3qyr+Ly5tw+6/P4961pvoZVV1K1FFFamAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAVq2MXl24OPmfk/0rPtYvOmCn7o5P0rZrKo+htSXUKKKKyNgooooAKKKKACiiigAooooAKKKKACoriETxFD16j61LRT2Bq5hMpVircEHBpK0r62Mg8yMfMOo9aza3i7o5ZR5WFFFFUSFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRV6xtjkSyDj+EH+dJuyHFczsWLSDyIvm++33uasUUVzt3OpKysFFFFIYUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAVnXlpszJEPl7qO1aNFUnYmUVJGDRWjdWQf5oQFPdegNZxBBIIwR2rdNM55RcdwooopkhRRRQAUUUUAFFFFABRRRQAUUUUAFFFXrWyOQ8w46hf8aTaQ1Fy2I7S0Mx3vxGP1rTAAAAGAO1AAAAAwB2pawlK50xiooKKKKkoKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAqGe3ScfMMNjhvSpqKadgavuZE1pLDk43L6ioK3qgltIZSSVwx7rxWiqdzF0uxkUVcfTpB9x1bjvwaga2mU4MbfgM/yrRSTM3FoiooopkhRRRQAUVIlvK+Nsbc9DjAqdNPlbG8qo79yKTaRSi2VKlhtpZuVXC/3j0rQisoYwMje3q3+FWazdTsaKl3K9vaJBz95/7xqxRRWbdzVJLYKKKKQwooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigBCAQQRkHtTPIi/55J/3yKkophYj8iL/AJ5J/wB8inqoUYUAD0FLRQFgooopAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFAH/2Q==',
  gif:
    'R0lGODdhIAEgAYEAAB54PP8AAAAAAAAAACwAAAAAIAEgAUAI/wADCBxIsCCAgwgTKlzIsKHDhxAjSpxIsaLFixgzatyYsaDHjwY5ihxJsqTJkyhTcgTJsqVAlTBjypxJs+ZGlzg92tzJs6fPnxdzCn0JtKjRo0hTDhWatKnTp1AZLs0ZtarVqzyn4sTKtavXkVpdfh1LtqzDsC3Nql37FS1LtnDjQnULUq7du0DpfsTLt+9MvTr9Ch5MEnBIwogTVzRMULHjx2cZE4VMGbLkyZUzI74cQLNnwpw/i+4berRpuaVPq1aberXrtpdfy/baerbtubFv635ae7fvvLl/C//Ze7jxv8GPK5dZfLnzks2fS7+ZfLp16pKva8fOeLv3oJzFfv8fTzE7+fORzaNfnzA6++/u32+PL/86/frT7+N/rn//8v7+HQdggMMNSOBvBh64W4IK3sZgg7M9COFrEk64WoUWnoZhhqNtyOFnHn6oWYgiVkZiiZZVhyKCKq64YIsuOghjjBHOSCOFNt54YY46ashjjx3+CCSIQg45YpFGmmhYkrqFdxiTNYYFJY56TWnaiVaahWWWZG3JJWzqfalkmGKmSGaZjnmJ5lVqrllVm27idmacg8FJZ1N23olUnnoaxWefwM0J6F1/DtpToYbuhGiiNS3KKHKCPrqWo5LCRGmlSiGJKZuabvpmp57K2V2obF1KKlignpqUqapyt2SrZbH/CitGss5qUa22lpdqrj7hymtEvv76ULDCNkRssQsdi2x7uy4L6ajOivpqtLw1S61KykabrbPbLtstst8WG66w4/5aLq/n5pqurevO2i6s77Yar6rznlovqfeGmq+n+27aL6b/VhqwpNZemyldBluFVsJcOdkZw5wuBfGnWk1c7cIW71llxkVBy3FWkX5sacEi30pyybqGjPJJAyfasqEvDxozoDP3WbOeN9+ZM507x9mzmz+vGTSaQ5dZtJhHf5k0l0tn2bSVT08ZNZRTM1l1klcbmfWQWwPZdY9f6xj2jWPTWHaMZ7uY9opro9h2iW+LGPeHc3NYd4Z3W5j3hHtD/9h3g38rGPiBgxNYeICH+5f4fovj13h9j8sX+XuTs1f5epejl/l5m5PX+Xifw3fyysCOTvqwpp9ubOqqJ8t668yqDLurgM2O7eu2h+6d7vPhPjvv2gFvn++wC2+d8fkR3zry0jHPn/KqO++c9P9Bfzr1ymEvoPWka2+c9wVyvzL4wpHPouy20yo+yub71v6L6Kdvcvzyp+xx/SK93+T6JesvI/34Kx0AA4i6ARJwdQY8oOsSqMDY3a+B83sgBO03rQlGsIIWpGDtMnjBDXJQgxv74ET8ZxsSRkmCIkQgClO4wBWy0IEYfKFU+CcyE8rGhlRyoQwBgEPX9HBHDJzgDyBVM0QfxXCHLXQLEiUSwiVCRIlOHGHFogjCtFBxMUIJCAA7',
  webp:
    'UklGRmgBAABXRUJQVlA4IFwBAACQGgCdASogASABPm02mEikI6KhJHd4AIANiWdu4XdhGl1gBphFYFrhAKqLtkjmsC1wgFVF2yRzWBa4QCqf0r4Nc1d0XLsPvS1KpmE74BU/6CDCqi6GtLSRbW5dkjmqRTtwfjW9ALXB98MwqoB0tQQCqfwZF2xVYeyRzV84C4QBTpaggFU/gyLtiqw9kjmr5wFwgCnS1BAKp/BkXbFVh7JHNXzgLhAFOlqCAVT+DIu2J4jwAsCwLAsCwLAsCwLAjvGSyOVq2SOawLXKdVRdskc1gWuEAqou2SOawLW7gAD+/4qd//+Ur/8bv+wXb7879zMAAADR/wZT+6vtU73dXwojiidiN8P7OP7w0pj/1ToL/WhI/GP0sGJJTL7SxZBk2rZaCQewoAAAAAAVfYN+qOxAeCcb/q8EY7p3caR7vbWvuawpPxApQGcBlzAnR+/UHAe++PnfkjN+6BAAAAA=',
} as const;

type ImageFormat = keyof typeof IMAGES;

/** The format name the mock reports for each — its `_image_format` magic-number table. */
const REPORTED_FORMAT: Readonly<Record<ImageFormat, string>> = {
  png: 'PNG',
  gif: 'GIF',
  jpeg: 'JPEG',
  webp: 'WEBP',
};

const EXTENSION: Readonly<Record<ImageFormat, string>> = {
  png: 'png',
  gif: 'gif',
  jpeg: 'jpg',
  webp: 'webp',
};

function imageBytes(format: ImageFormat): Buffer {
  return Buffer.from(IMAGES[format], 'base64');
}

interface ArtifactAgent {
  readonly projectId: string;
  readonly bucket: string;
  readonly conversationId: string;
  readonly upload: (fileName: string, body: Buffer, mimeType: string) => Promise<void>;
  readonly dispose: () => Promise<void>;
}

/**
 * A bucket, an `artifact` toolkit over it, an agent pinned to the mock model
 * with that toolkit attached, and a conversation open in the browser.
 *
 * Over the API rather than through the forms, the same trade
 * `createMockToolAgent` documents: the toolkit form's own behaviour belongs to
 * `chat.artifacts-toolkit.spec.ts`, and what these cases are about starts at
 * the turn. The attach is read back, because the relation route has answered
 * 200 while writing nothing before.
 */
async function createArtifactAgent(page: Page, label: string): Promise<ArtifactAgent> {
  const suffix = `${String(Date.now() % 1_000_000)}${label}`;
  const bucket = `autotest-img-${suffix}`;
  const toolkitName = `${AUTOTEST_PREFIX}imgtk-${suffix}`;
  const agentName = `${AUTOTEST_PREFIX}imgagent-${suffix}`;

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona must work inside its own project').not.toBe('');
  const caller = await readCallerIdentity(page.request);

  const bucketCreated = await page.request.post(`${BASE_URL}/api/v2/artifacts/buckets/${projectId}`, {
    data: { name: bucket },
  });
  expect([200, 201, 409]).toContain(bucketCreated.status());

  const toolkit = await page.request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${projectId}`, {
    data: {
      name: toolkitName,
      type: 'artifact',
      settings: { bucket, selected_tools: ['list_files', 'read_file', 'get_file_metadata'] },
    },
  });
  expect(
    toolkit.status(),
    `the artifact toolkit must be creatable: ${(await toolkit.text()).slice(0, 300)}`,
  ).toBe(201);
  const toolkitId = String(((await toolkit.json()) as { id?: unknown }).id ?? '');

  const agent = await createAgentWithVersion(
    page.request,
    agentName,
    {
      instructions: 'You are an autotest agent. Call the tools you are asked to call.',
      model: { modelName: MOCK_MODEL },
    },
    projectId,
  );
  const attached = await page.request.patch(`${API_BASE}/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`, {
    data: {
      entity_version_id: Number(agent.versionId),
      entity_id: Number(agent.id),
      entity_type: 'agent',
      has_relation: true,
    },
  });
  expect(attached.status(), `the toolkit must attach: ${(await attached.text()).slice(0, 300)}`).toBeLessThan(300);
  const storedAgent = await page.request.get(`${API_BASE}/elitea_core/application/prompt_lib/${projectId}/${agent.id}`);
  const tools =
    ((await storedAgent.json()) as { version_details?: { tools?: readonly { name?: string }[] } }).version_details
      ?.tools ?? [];
  expect(
    tools.map((tool) => tool.name),
    'the agent version carries no reference to the toolkit — the attach was a no-op',
  ).toContain(toolkitName);

  const conversation = await page.request.post(`${API_BASE}/elitea_core/conversations/prompt_lib/${projectId}`, {
    data: { name: `${AUTOTEST_PREFIX}imgconv-${suffix}`, is_private: true },
  });
  expect(conversation.status()).toBe(201);
  const conversationId = String(((await conversation.json()) as { id?: unknown }).id ?? '');
  const participants = await page.request.post(
    `${API_BASE}/elitea_core/participants/prompt_lib/${projectId}/${conversationId}`,
    {
      data: [
        {
          entity_name: 'application',
          entity_meta: { id: agent.id, project_id: Number(projectId), name: agentName },
          entity_settings: { version_id: agent.versionId },
        },
        { entity_name: 'user', entity_meta: { id: Number(caller.id) } },
      ],
    },
  );
  expect(participants.status()).toBe(200);

  await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 45_000 });

  return {
    projectId,
    bucket,
    conversationId,
    upload: async (fileName, body, mimeType): Promise<void> => {
      const uploaded = await page.request.post(
        `${BASE_URL}/api/v2/artifacts/objects/${projectId}/${bucket}?overwrite=true`,
        { multipart: { file: { name: fileName, mimeType, buffer: body } } },
      );
      expect(uploaded.status(), await uploaded.text()).toBe(201);
    },
    dispose: async (): Promise<void> => {
      await page.request.delete(`${API_BASE}/elitea_core/application/prompt_lib/${projectId}/${agent.id}`);
      await page.request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`);
      await page.request.delete(`${BASE_URL}/api/v2/artifacts/buckets/${projectId}/${bucket}`).catch(() => {});
    },
  };
}

/** Send one scripted tool call and return everything the transcript stored for it. */
async function callTool(
  page: Page,
  agent: ArtifactAgent,
  tool: string,
  args: Readonly<Record<string, unknown>>,
  tail: string,
): Promise<string> {
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 30_000 });
  await input.fill(callToolWithArgumentsPrompt(tool, args, tail));
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('chat-send-button').click();
  expect(
    (await started).status(),
    'the image-read turn must be admitted',
  ).toBe(200);

  await expectStoredAssistantAnswer(page, agent.projectId, agent.conversationId, {
    timeout: 180_000,
    message: 'the image-read turn produced no stored answer',
  });
  const rows = await readStoredTranscript(page, agent.projectId, agent.conversationId);
  return rows
    .filter((row) => row.role !== 'user')
    .map((row) => row.content)
    .join('\n');
}

/** Every image the mock has been shown, newest last. */
async function shownImages(page: Page): Promise<readonly { md5: string; format: string; bytes: number }[]> {
  const journal = await readMockLlmJournal(page);
  return journal.flatMap(
    (entry) => ((entry as unknown as { images?: readonly { md5: string; format: string; bytes: number }[] }).images ?? []),
  );
}

/* onetest: ELITEA-0305, ELITEA-0306, ELITEA-0304, ELITEA-0307, ELITEA-0302 — an image stored in an Artifacts
 * bucket is read by an agent in chat and the MODEL IS SHOWN THE BYTES: the mock's journal carries the image
 * part, identified as a PNG, and the description it keyed to those bytes is what the answer quotes. (The
 * seeded model is a `vllm` provider, not a Claude one, so this is also the alternative-multimodal-model
 * case: nothing in the path is Claude-specific.) */
test('an image in a bucket is read in chat and the model is shown its bytes', async ({ page }) => {
  test.setTimeout(300_000);
  const agent = await createArtifactAgent(page, 'read');
  try {
    const fileName = `sunset-${String(Date.now() % 1_000_000)}.png`;
    const body = imageBytes('png');
    await agent.upload(fileName, body, 'image/png');

    const answer = await callTool(page, agent, 'read_file', { filename: fileName }, 'describe the picture');

    const images = await shownImages(page);
    expect(images, 'the model must have been shown exactly one image').toHaveLength(1);
    expect(images[0]?.format, 'the bytes that reached the model must still be a PNG').toBe('PNG');
    expect(images[0]?.bytes, 'the image part must carry bytes, not a placeholder').toBeGreaterThan(0);
    // The DESCRIPTION the mock keyed to those exact bytes, quoted back in the
    // answer: the tie between what the model saw and what the user was told.
    expect(answer, 'the answer must carry the description the model produced for those bytes').toContain(
      images[0]?.md5.slice(0, 12) ?? 'no-digest',
    );
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-0298 — every image format the platform claims to support is read, and each reaches the
 * model AS THAT FORMAT: a JPEG that arrives as a PNG is a conversion nobody asked for, and a format the
 * loader cannot open does not arrive at all. */
test('every supported image format is read and reaches the model as itself', async ({ page }) => {
  test.setTimeout(420_000);
  const agent = await createArtifactAgent(page, 'formats');
  try {
    const formats: readonly ImageFormat[] = ['png', 'jpeg', 'gif', 'webp'];
    const stamp = String(Date.now() % 1_000_000);
    for (const format of formats) {
      await agent.upload(`shot-${stamp}.${EXTENSION[format]}`, imageBytes(format), `image/${format}`);
    }

    const seen: string[] = [];
    for (const format of formats) {
      await clearMockLlmJournal(page);
      const answer = await callTool(
        page,
        agent,
        'read_file',
        { filename: `shot-${stamp}.${EXTENSION[format]}` },
        `describe the ${format}`,
      );
      const images = await shownImages(page);
      expect(images, `the ${format} must have reached the model as an image part`).not.toHaveLength(0);
      const last = images[images.length - 1];
      expect(
        answer,
        `the answer must quote the description the model produced for the ${format}`,
      ).toContain(last?.md5.slice(0, 12) ?? 'no-digest');
      seen.push(last?.format ?? '');
    }
    expect(seen, 'each file must reach the model as its own format').toEqual(
      formats.map((format) => REPORTED_FORMAT[format]),
    );
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-0299, ELITEA-0301 — text and images in the SAME conversation: a text file comes back as
 * text with no image part on the wire at all, and an image in the same session still reaches the model as
 * one. Reading a non-image file is unaffected by the image path. */
test('a text file and an image read in one session stay on their own paths', async ({ page }) => {
  test.setTimeout(420_000);
  const agent = await createArtifactAgent(page, 'mixed');
  try {
    const stamp = String(Date.now() % 1_000_000);
    const textName = `notes-${stamp}.txt`;
    const textBody = `autotest plain text ${stamp}\nsecond line\n`;
    await agent.upload(textName, Buffer.from(textBody), 'text/plain');
    const imageName = `diagram-${stamp}.png`;
    await agent.upload(imageName, imageBytes('png'), 'image/png');

    const textAnswer = await callTool(page, agent, 'read_file', { filename: textName }, 'read the notes');
    expect(textAnswer, 'a text file must come back as its own text').toContain(`autotest plain text ${stamp}`);
    expect(
      await shownImages(page),
      'reading a TEXT file must put no image part on the wire — the loader stack is for images',
    ).toHaveLength(0);

    await clearMockLlmJournal(page);
    const imageAnswer = await callTool(page, agent, 'read_file', { filename: imageName }, 'describe the diagram');
    const images = await shownImages(page);
    expect(images, 'the image in the same session must still reach the model').toHaveLength(1);
    expect(imageAnswer).toContain(images[0]?.md5.slice(0, 12) ?? 'no-digest');
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-0300 — a bucket holding several images reads back the one that was NAMED, not whichever the
 * listing happens to return first. */
test('the named image is the one read, in a bucket holding several', async ({ page }) => {
  test.setTimeout(420_000);
  const agent = await createArtifactAgent(page, 'several');
  try {
    const stamp = String(Date.now() % 1_000_000);
    await agent.upload(`one-${stamp}.png`, imageBytes('png'), 'image/png');
    await agent.upload(`two-${stamp}.gif`, imageBytes('gif'), 'image/gif');
    await agent.upload(`three-${stamp}.webp`, imageBytes('webp'), 'image/webp');

    const answer = await callTool(
      page,
      agent,
      'read_file',
      { filename: `two-${stamp}.gif` },
      'describe the second picture',
    );
    const images = await shownImages(page);
    expect(images, 'exactly one of the three must have been read').toHaveLength(1);
    expect(images[0]?.format, 'the NAMED file is the one that must reach the model').toBe(REPORTED_FORMAT.gif);
    expect(answer).toContain(images[0]?.md5.slice(0, 12) ?? 'no-digest');
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-0510 — an image ATTACHED in the composer (not stored in a bucket) is accepted and analysed:
 * the bytes reach the model as an image part, for every extension the composer accepts. */
test('an image attached in the composer is analysed by the model', async ({ page }) => {
  test.setTimeout(420_000);
  const stamp = String(Date.now() % 1_000_000);
  const directory = mkdtempSync(join(tmpdir(), 'autotest-img-'));
  const file = join(directory, `attached-${stamp}.png`);
  writeFileSync(file, imageBytes('png'));

  await page.goto(`${BASE_URL}/app/chat`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });
  await page.getByTestId('model-selector-button').click();
  const modelOption = page.getByRole('menuitem').filter({ hasText: 'E2E-MOCK-MODEL' }).first();
  await expect(modelOption, 'the seeded model must be offered').toBeVisible({ timeout: 20_000 });
  await modelOption.click();

  // The real control: the attach row of the composer's "+" menu, whose hidden
  // file input exists only while that menu is rendered.
  const plus = page.getByTestId('plus-menu-button');
  await expect(plus).toBeEnabled({ timeout: 20_000 });
  await plus.click();
  const attachRow = page.getByTestId('plus-menu-attachments');
  await expect(attachRow).toBeVisible();
  await attachRow.locator('xpath=ancestor::div[1]').locator('input[type="file"]').setInputFiles(file);

  const created = page.waitForResponse(
    (r) => /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 45_000 },
  );
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 20_000 });
  await input.fill(`describe the attached picture ${stamp}`);
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('chat-send-button').click();

  const createdResponse = await created;
  const projectId = /\/prompt_lib\/(\d+)$/.exec(new URL(createdResponse.url()).pathname)?.[1] ?? '';
  const conversationId = String(((await createdResponse.json()) as { id?: unknown }).id ?? '');
  expect((await started).status(), 'the attachment turn must be admitted').toBe(200);
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 180_000,
    message: 'the attachment turn produced no stored answer',
  });

  const images = await shownImages(page);
  expect(images, 'an attached image must reach the model as an image part, not as a filename').toHaveLength(1);
  expect(images[0]?.format, 'the attached PNG must arrive as a PNG').toBe(REPORTED_FORMAT.png);
});

/* onetest: ELITEA-0303 — a PIPELINE run reads the image, not only an agent: the same artifact toolkit bound
 * to a pipeline's LLM node puts the bytes on the wire the same way. */
test('a pipeline run reads an image from a bucket', async ({ page }) => {
  test.setTimeout(420_000);
  const suffix = String(Date.now() % 1_000_000);
  const bucket = `autotest-pipeimg-${suffix}`;
  const toolkitName = `${AUTOTEST_PREFIX}pipeimgtk-${suffix}`;
  const pipelineName = `${AUTOTEST_PREFIX}pipeimg-${suffix}`;
  const fileName = `pipeline-shot-${suffix}.png`;

  const projectId = await readCallerPersonalProjectId(page.request);
  const caller = await readCallerIdentity(page.request);

  const bucketCreated = await page.request.post(`${BASE_URL}/api/v2/artifacts/buckets/${projectId}`, {
    data: { name: bucket },
  });
  expect([200, 201, 409]).toContain(bucketCreated.status());
  const uploaded = await page.request.post(
    `${BASE_URL}/api/v2/artifacts/objects/${projectId}/${bucket}?overwrite=true`,
    { multipart: { file: { name: fileName, mimeType: 'image/png', buffer: imageBytes('png') } } },
  );
  expect(uploaded.status(), await uploaded.text()).toBe(201);

  const toolkit = await page.request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${projectId}`, {
    data: {
      name: toolkitName,
      type: 'artifact',
      settings: { bucket, selected_tools: ['list_files', 'read_file', 'get_file_metadata'] },
    },
  });
  expect(toolkit.status(), `the artifact toolkit must be creatable: ${(await toolkit.text()).slice(0, 300)}`).toBe(201);
  const toolkitId = String(((await toolkit.json()) as { id?: unknown }).id ?? '');

  // A pipeline is an `interface` application whose version is `agent_type:
  // pipeline` and whose `instructions` are the graph. The `state` block uses
  // the DESCRIPTOR form, the only one both compilers accept
  // (`chat.pipeline.spec.ts` measured the SDK rejecting a bare type name).
  //
  // A `toolkit` NODE, not an llm node with the toolkit attached. Measured on
  // this stack: a pipeline's llm node is offered NO tools (the mock's journal
  // records `tools: []` for it), because a pipeline calls a toolkit through a
  // node that names it — `langraph_agent.py` resolves `toolkit_name` + `tool`
  // against the version's toolkits and compiles a `FunctionTool` that runs
  // without asking the model. So the read here is a step of the graph, which
  // is what the case describes.
  const graph = [
    'state:',
    '  input:',
    '    type: str',
    '  messages:',
    '    type: list',
    'entry_point: READ_1',
    'nodes:',
    '  - id: READ_1',
    '    type: toolkit',
    `    toolkit_name: ${toolkitName}`,
    '    tool: read_file',
    '    input:',
    '      - input',
    '    input_mapping:',
    '      filename:',
    '        type: fstring',
    `        value: ${fileName}`,
    '    output:',
    '      - messages',
    '    transition: END',
    '',
  ].join('\n');
  const created = await page.request.post(`${API_BASE}/elitea_core/applications/prompt_lib/${projectId}`, {
    data: {
      name: pipelineName,
      description: `${pipelineName} description`,
      type: 'interface',
      versions: [
        {
          name: 'base',
          agent_type: 'pipeline',
          instructions: graph,
          conversation_starters: [],
          variables: [],
          meta: { step_limit: 25, internal_tools: [] },
          llm_settings: { model_name: MOCK_MODEL },
        },
      ],
    },
  });
  expect(created.status(), `the pipeline must be creatable: ${(await created.text()).slice(0, 300)}`).toBe(201);
  const createdBody = (await created.json()) as { id?: unknown; version_details?: { id?: unknown } };
  const pipelineId = String(createdBody.id ?? '');
  const versionId = String(createdBody.version_details?.id ?? '');

  try {
    const attached = await page.request.patch(`${API_BASE}/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`, {
      data: {
        entity_version_id: Number(versionId),
        entity_id: Number(pipelineId),
        entity_type: 'agent',
        has_relation: true,
      },
    });
    expect(attached.status(), `the toolkit must attach to the pipeline version`).toBeLessThan(300);

    const conversation = await page.request.post(`${API_BASE}/elitea_core/conversations/prompt_lib/${projectId}`, {
      data: { name: `${AUTOTEST_PREFIX}pipeimgconv-${suffix}`, is_private: true },
    });
    expect(conversation.status()).toBe(201);
    const conversationId = String(((await conversation.json()) as { id?: unknown }).id ?? '');
    const participants = await page.request.post(
      `${API_BASE}/elitea_core/participants/prompt_lib/${projectId}/${conversationId}`,
      {
        data: [
          {
            entity_name: 'application',
            entity_meta: { id: pipelineId, project_id: Number(projectId), name: pipelineName },
            entity_settings: { version_id: versionId },
          },
          { entity_name: 'user', entity_meta: { id: Number(caller.id) } },
        ],
      },
    );
    expect(participants.status()).toBe(200);

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 45_000 });
    await clearMockLlmJournal(page);

    const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
      timeout: 60_000,
    });
    const input = page.getByTestId('chat-message-input');
    // No `[[mock:call_tool]]` marker: the graph's own node performs the read,
    // so nothing here depends on the model choosing to call anything.
    await input.fill(`describe the picture ${suffix}`);
    await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
    await page.getByTestId('chat-send-button').click();
    expect((await started).status(), 'the pipeline turn must be admitted').toBe(200);
    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 180_000,
      message: 'the pipeline produced no stored answer',
    });

    const images = await shownImages(page);
    expect(images, 'the pipeline run must have shown the model the image').toHaveLength(1);
    expect(images[0]?.format, 'the bytes that reached the model must still be a PNG').toBe(REPORTED_FORMAT.png);
  } finally {
    await page.request.delete(`${API_BASE}/elitea_core/application/prompt_lib/${projectId}/${pipelineId}`);
    await page.request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`);
    await page.request.delete(`${BASE_URL}/api/v2/artifacts/buckets/${projectId}/${bucket}`).catch(() => {});
  }
});
