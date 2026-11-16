import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../../test/setup';

import { ListingLike } from './ListingLike';

describe('ListingLike', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('shows the count and the caller\'s own like of a skill, and toggles it through the skill entity', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const paths: string[] = [];
    server.use(
      http.post('*/social/like/prompt_lib/:projectId/skill/:entityId', ({ request }) => {
        paths.push(new URL(request.url).pathname);
        return HttpResponse.json({ ok: true });
      }),
    );
    renderWithTheme(<ListingLike kind="skills" entityId="601" likes={4} isLiked={false} name="Triage" />);

    const button = screen.getByRole('button', { name: 'Like Triage' });
    expect(button).toHaveAttribute('aria-pressed', 'false');
    expect(screen.getByTestId('marketplace-like-count')).toHaveTextContent('4');

    await userEvent.click(button);
    await waitFor(() => expect(button).toHaveAttribute('aria-pressed', 'true'));
    expect(screen.getByTestId('marketplace-like-count')).toHaveTextContent('5');
    expect(paths).toHaveLength(1);
    expect(paths[0]).toMatch(/\/social\/like\/prompt_lib\/[^/]+\/skill\/601$/);
  });

  it('likes an agent as an application', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const paths: string[] = [];
    server.use(
      http.post('*/social/like/prompt_lib/:projectId/application/:entityId', ({ request }) => {
        paths.push(new URL(request.url).pathname);
        return HttpResponse.json({ ok: true });
      }),
    );
    renderWithTheme(<ListingLike kind="agents" entityId="602" likes={0} isLiked={false} name="Helper" />);
    await userEvent.click(screen.getByRole('button', { name: 'Like Helper' }));
    await waitFor(() => expect(paths).toHaveLength(1));
    expect(paths[0]).toMatch(/\/application\/602$/);
  });

  it('draws no heart for a listing whose public twin is gone', () => {
    renderWithTheme(
      <>
        <ListingLike kind="skills" entityId="" likes={1} isLiked={false} name="Gone" />
        <ListingLike kind="agents" entityId="0" likes={1} isLiked={false} name="Gone too" />
      </>,
    );
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
  });

  it('says so when the like is refused, instead of snapping back silently', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.post('*/social/like/prompt_lib/:projectId/skill/:entityId', () =>
        HttpResponse.json({ ok: false, error: 'no' }, { status: 500 }),
      ),
    );
    renderWithTheme(<ListingLike kind="skills" entityId="603" likes={2} isLiked={false} name="Triage" />);
    await userEvent.click(screen.getByRole('button', { name: 'Like Triage' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Your like could not be saved.');
    expect(screen.getByRole('button', { name: 'Like Triage' })).toHaveAttribute('aria-pressed', 'false');
    expect(screen.getByTestId('marketplace-like-count')).toHaveTextContent('2');
  });
});
