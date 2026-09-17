import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { IndexRow } from '../../model/indexesStore';

import { IndexListItem } from './IndexListItem';

describe('IndexListItem', () => {
  /* elitea_issues: #2975 — a freshly-created index row whose `metadata` has not arrived yet (or is empty) must render the placeholder, never crash with "Cannot read properties of undefined (reading 'length')". */
  it('does not crash when metadata is absent on a brand-new index row', () => {
    const index = { id: 'new-1' } as unknown as IndexRow;
    expect(() => renderWithTheme(<IndexListItem index={index} />)).not.toThrow();
  });

  it('renders a loading skeleton when useMock is set', () => {
    const { getAllByTestId } = renderWithTheme(
      <IndexListItem
        useMock
        index={{ id: 'skel', metadata: {} }}
      />,
    );
    expect(getAllByTestId('index-list-item-skeleton')).toHaveLength(2);
  });

  it('shows the collection name and total-indexed count', () => {
    const index: IndexRow = { id: '1', metadata: { collection: 'my-index', indexed: 42 } };
    const { getByText } = renderWithTheme(<IndexListItem index={index} />);
    expect(getByText('my-index')).toBeInTheDocument();
    expect(getByText('42')).toBeInTheDocument();
  });

  it('shows "reindexed / total indexed" once history has more than one entry', () => {
    const index: IndexRow = { id: '1', metadata: { collection: 'my-index', indexed: 42, updated: 5, history: [{}, {}] } };
    const { getByText } = renderWithTheme(<IndexListItem index={index} />);
    expect(getByText('5 / 42')).toBeInTheDocument();
  });

  /**
   * elitea_issues: #6066 — the index chip must show skipped-file info
   * consistently with the index details page: same count, same tooltip
   * ("total skipped during indexing", see this component's `Tooltip` above).
   * The badge counts everything the run LEFT OUT (`report.totals.leftOut`),
   * not one summary key off the raw blob. The old `skipped.total_skipped`
   * read reported 0 for every row whose indexer wrote a canonical `report`.
   */
  it('shows a skipped-count badge when documents were skipped', () => {
    const index: IndexRow = {
      id: '1',
      metadata: {
        collection: 'my-index',
        indexed: 10,
        skipped: {
          documents_skipped: { filtered_count: 2, filtered: ['x.md', 'y.md'] },
          files_skipped: { unsupported_extension_count: 1, unsupported_extension: ['a.bin'] },
        },
      },
    };
    const { getByText } = renderWithTheme(<IndexListItem index={index} />);
    expect(getByText('3')).toBeInTheDocument();
  });

  it('counts a canonical report\'s left-out total, which the old skipped-blob read could not see', () => {
    const index: IndexRow = {
      id: '1',
      metadata: {
        collection: 'my-index',
        indexed: 10,
        report: { status: 'partly_indexed', totals: { indexed: 10, skipped: 4, not_indexed: 1, failed: 2 } },
      },
    };
    const { getByText } = renderWithTheme(<IndexListItem index={index} />);
    expect(getByText('7')).toBeInTheDocument();
  });

  it('keeps the red error/stale styling even when the row is also selected (baseline: error wins over selected)', () => {
    const index: IndexRow = { id: '1', metadata: { collection: 'my-index', state: 'in_progress' }, stale: true };
    const { getByText } = renderWithTheme(
      <IndexListItem
        index={index}
        currentIndex={index}
      />,
    );
    const nameEl = getByText('my-index');
    const wrapperBox = nameEl.parentElement;
    if (!wrapperBox) throw new Error('wrapper box not found');
    const cls = Array.from(wrapperBox.classList).find((c) => c.startsWith('css-'));
    if (!cls) throw new Error('emotion class not found on wrapper box');

    // jsdom's CSS engine doesn't resolve `var(...)` inside shorthand
    // properties for `getComputedStyle` — same technique as `IndexChat.
    // test.tsx`'s `chatBodyContainerSx` assertion: read the generated
    // rule's own CSS text instead.
    let cssText = '';
    for (const sheet of Array.from(document.styleSheets)) {
      for (const rule of Array.from(sheet.cssRules)) {
        if ('selectorText' in rule && (rule as CSSStyleRule).selectorText?.includes(cls)) {
          cssText += (rule as CSSStyleRule).cssText;
        }
      }
    }
    // Pre-fix (`isSelected` checked first): this would be
    // `var(--el-palette-action-selected)` instead — the plain "selected"
    // style winning outright and the error/stale indicator disappearing.
    expect(cssText).toMatch(/border:\s*\.0625rem solid var\(--el-palette-error-main/);
    expect(cssText).toMatch(/background:\s*var\(--el-palette-error-light/);
  });

  it('calls onIndexClick with the index when clicked', async () => {
    const user = userEvent.setup();
    const onIndexClick = vi.fn();
    const index: IndexRow = { id: '1', metadata: { collection: 'my-index' } };
    const { getByText } = renderWithTheme(
      <IndexListItem
        index={index}
        onIndexClick={onIndexClick}
      />,
    );
    await user.click(getByText('my-index'));
    expect(onIndexClick).toHaveBeenCalledWith(index);
  });
});
