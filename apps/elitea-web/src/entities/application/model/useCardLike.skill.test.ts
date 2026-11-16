import { act, renderHook } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { server } from '../../../test/setup';

import { useCardLike } from './useCardLike';

/**
 * Skills epic 2169, S6 (ADR-0051 amendment 1, A1.5): a skill is liked as the
 * `skill` entity through the generic like route, and a skill and an agent that
 * share a numeric id do not share a like.
 */
describe('useCardLike for a skill', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('POSTs the generic like route with the skill entity and DELETEs it to unlike', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const calls: string[] = [];
    server.use(
      http.post('*/social/like/prompt_lib/:projectId/skill/:entityId', ({ request }) => {
        calls.push(`POST ${new URL(request.url).pathname}`);
        return HttpResponse.json({ ok: true }, { status: 200 });
      }),
      http.delete('*/social/like/prompt_lib/:projectId/skill/:entityId', ({ request }) => {
        calls.push(`DELETE ${new URL(request.url).pathname}`);
        return HttpResponse.json({ ok: true }, { status: 200 });
      }),
    );

    const { result } = renderHook(() =>
      useCardLike({ applicationId: '501', entityType: 'skill', projectId: 'pub-1', initialLiked: false, initialCount: 1 }),
    );
    await act(async () => {
      await result.current.toggleLike();
    });
    expect(result.current.isLiked).toBe(true);
    expect(result.current.likeCount).toBe(2);

    await act(async () => {
      await result.current.toggleLike();
    });
    expect(result.current.isLiked).toBe(false);
    expect(result.current.likeCount).toBe(1);
    expect(calls).toEqual([
      'POST /api/v2/social/like/prompt_lib/pub-1/skill/501',
      'DELETE /api/v2/social/like/prompt_lib/pub-1/skill/501',
    ]);
  });

  it('keeps an agent and a skill with the same id apart', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.post('*/social/like/prompt_lib/:projectId/skill/:entityId', () => HttpResponse.json({ ok: true })),
    );

    const skill = renderHook(() =>
      useCardLike({ applicationId: '777', entityType: 'skill', projectId: 'pub-1', initialLiked: false, initialCount: 0 }),
    );
    await act(async () => {
      await skill.result.current.toggleLike();
    });
    expect(skill.result.current.isLiked).toBe(true);

    const agent = renderHook(() =>
      useCardLike({ applicationId: '777', projectId: 'pub-1', initialLiked: false, initialCount: 0 }),
    );
    expect(agent.result.current.isLiked).toBe(false);
    expect(agent.result.current.likeCount).toBe(0);
  });

  it('lets a later read with other server values win over the local entry', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.post('*/social/like/prompt_lib/:projectId/skill/:entityId', () => HttpResponse.json({ ok: true })),
    );
    const { result, rerender } = renderHook(
      ({ liked, count }: { liked: boolean; count: number }) =>
        useCardLike({ applicationId: '888', entityType: 'skill', projectId: 'pub-1', initialLiked: liked, initialCount: count }),
      { initialProps: { liked: false, count: 1 } },
    );
    await act(async () => {
      await result.current.toggleLike();
    });
    expect(result.current.likeCount).toBe(2);

    // The list is refetched: the server now counts our like and another one.
    rerender({ liked: true, count: 3 });
    expect(result.current.isLiked).toBe(true);
    expect(result.current.likeCount).toBe(3);
  });

  it('reverts and calls onError when the server refuses the like', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.post('*/social/like/prompt_lib/:projectId/skill/:entityId', () =>
        HttpResponse.json({ ok: false, error: 'no' }, { status: 403 }),
      ),
    );
    let refused = 0;
    const { result } = renderHook(() =>
      useCardLike({
        applicationId: '999',
        entityType: 'skill',
        projectId: 'pub-1',
        initialLiked: false,
        initialCount: 2,
        onError: () => {
          refused += 1;
        },
      }),
    );
    await act(async () => {
      await result.current.toggleLike();
    });
    expect(result.current.isLiked).toBe(false);
    expect(result.current.likeCount).toBe(2);
    expect(refused).toBe(1);
  });
});
