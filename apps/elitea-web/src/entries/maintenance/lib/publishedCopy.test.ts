/**
 * The standalone splash's run-time copy reader.
 *
 * Every case here is about the same rule: this page must render whether or not
 * the platform answers. A reader that threw, or that surfaced an error state,
 * would turn a maintenance window into a broken maintenance page.
 */
import { describe, expect, it, vi } from 'vitest';

import { fetchPublishedCopy, NO_PUBLISHED_COPY, readPublishedCopy } from './publishedCopy';

describe('readPublishedCopy', () => {
  it('reads the three fields off the maintenance object', () => {
    expect(
      readPublishedCopy({
        maintenance: { enabled: true, title: ' Upgrade ', html: ' <p>hi</p> ', message: ' soon ' },
      }),
    ).toEqual({ title: 'Upgrade', html: '<p>hi</p>', message: 'soon' });
  });

  it('is nothing when the document carries no maintenance object', () => {
    expect(readPublishedCopy({ mcp_enabled: true })).toEqual(NO_PUBLISHED_COPY);
    expect(readPublishedCopy(null)).toEqual(NO_PUBLISHED_COPY);
    expect(readPublishedCopy('nope')).toEqual(NO_PUBLISHED_COPY);
  });

  it('drops non-string fields rather than rendering them', () => {
    expect(readPublishedCopy({ maintenance: { title: 42, html: null } })).toEqual(NO_PUBLISHED_COPY);
  });
});

describe('fetchPublishedCopy', () => {
  it('reads the copy off a 200', async () => {
    const fetchImpl = vi.fn().mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({ maintenance: { title: 'Upgrade', html: '<p>hi</p>', message: '' } }),
    });
    await expect(fetchPublishedCopy(fetchImpl as unknown as typeof fetch)).resolves.toEqual({
      title: 'Upgrade',
      html: '<p>hi</p>',
      message: '',
    });
    expect(fetchImpl).toHaveBeenCalledWith(
      '/api/v2/elitea_core/platform_settings/prompt_lib',
      expect.objectContaining({ credentials: 'same-origin' }),
    );
  });

  it('resolves to nothing on a non-2xx, so the page shows its own words', async () => {
    const fetchImpl = vi.fn().mockResolvedValue({ ok: false, status: 503, json: () => Promise.resolve({}) });
    await expect(fetchPublishedCopy(fetchImpl as unknown as typeof fetch)).resolves.toEqual(
      NO_PUBLISHED_COPY,
    );
  });

  it('resolves to nothing when the platform is DOWN, which is this page´s main case', async () => {
    const fetchImpl = vi.fn().mockRejectedValue(new TypeError('Failed to fetch'));
    await expect(fetchPublishedCopy(fetchImpl as unknown as typeof fetch)).resolves.toEqual(
      NO_PUBLISHED_COPY,
    );
  });

  it('resolves to nothing when the body is not JSON', async () => {
    const fetchImpl = vi.fn().mockResolvedValue({
      ok: true,
      json: () => Promise.reject(new SyntaxError('Unexpected token <')),
    });
    await expect(fetchPublishedCopy(fetchImpl as unknown as typeof fetch)).resolves.toEqual(
      NO_PUBLISHED_COPY,
    );
  });
});
