/**
 * WikiList: the two empty states are different, and that is the whole test.
 *
 * "This project has generated no wikis" and "this bucket has wikis and none is
 * yours" are the same blank screen to a user and completely different problems.
 * The second is usually a repository or branch that does not match what was
 * generated — something the user can fix — so the screen has to say so.
 *
 * Collapsing them is the easy refactor and the one that makes the feature
 * undiagnosable, which is why it is asserted rather than left to review.
 */
import { screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { WikiManifest } from '@/entities/wiki';

import { renderWithProviders } from '../__tests__/testUtils';
import { WikiList } from './WikiList';

const WIKI: WikiManifest = {
  wiki_id: 'acme--notes-service--main',
  wiki_title: 'notes-service',
  repository: 'acme/notes-service',
  branch: 'main',
};

describe('WikiList', () => {
  it('lists the wikis it is given', () => {
    renderWithProviders(<WikiList wikis={[WIKI]} allWikis={[WIKI]} onSelect={vi.fn()} />);
    expect(screen.getByText('notes-service')).toBeInTheDocument();
    expect(screen.getByText('acme/notes-service · main')).toBeInTheDocument();
  });

  it('says the project has generated nothing when the bucket is empty', () => {
    renderWithProviders(<WikiList wikis={[]} allWikis={[]} onSelect={vi.fn()} />);
    expect(screen.getByText(/no wiki has been generated for this project/i)).toBeInTheDocument();
  });

  it('says the repository does not match when the bucket holds other wikis', () => {
    // The discriminating case: same empty list, different cause, different text.
    renderWithProviders(<WikiList wikis={[]} allWikis={[WIKI]} onSelect={vi.fn()} />);
    expect(screen.getByText(/check the repository and branch/i)).toBeInTheDocument();
  });

  it('marks the selected wiki, and only it, as selected', () => {
    // Asserted HERE because the visual suite cannot assert it. `.Mui-selected`
    // on a `ListItemButton` paints `alpha(primary.main, selectedOpacity)` —
    // 8% magenta in the light scheme, which scores 81.9 on pixelmatch's YIQ
    // metric against the 88.0 ceiling that `SNAPSHOT_TOLERANCE`'s
    // `threshold: 0.05` sets. Zero pixels count as different, so
    // `deepwiki-browser-light` passes whether the highlight is drawn or not
    // (see the `threshold` section of e2e/visual/lib/settle.ts). A class
    // assertion is the only cheap thing that fails when the highlight goes.
    const OTHER: WikiManifest = { ...WIKI, wiki_id: 'acme--other--main', wiki_title: 'other' };
    renderWithProviders(
      <WikiList wikis={[WIKI, OTHER]} allWikis={[WIKI, OTHER]} selectedWikiId={WIKI.wiki_id} onSelect={vi.fn()} />,
    );
    const items = screen.getAllByRole('button');
    expect(items[0]).toHaveClass('Mui-selected');
    expect(items[1]).not.toHaveClass('Mui-selected');
  });

  it('reports the selected wiki to its caller', async () => {
    const onSelect = vi.fn();
    const { default: userEvent } = await import('@testing-library/user-event');
    renderWithProviders(<WikiList wikis={[WIKI]} allWikis={[WIKI]} onSelect={onSelect} />);
    await userEvent.setup().click(screen.getByText('notes-service'));
    expect(onSelect).toHaveBeenCalledWith(WIKI);
  });
});
