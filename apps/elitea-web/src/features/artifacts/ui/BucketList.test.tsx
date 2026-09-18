import { ThemeProvider } from '@mui/material/styles';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { Bucket } from '@/entities/bucket';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import { BucketList } from './BucketList';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function bucket(overrides: Partial<Bucket> = {}): Bucket {
  return {
    id: 'a-very-long-bucket-name-that-does-not-fit-the-row',
    name: 'a-very-long-bucket-name-that-does-not-fit-the-row',
    isPinned: false,
    createdAt: '2026-01-01T00:00:00Z',
    retentionDays: null,
    sizeBytes: 0,
    ...overrides,
  };
}

const NOOP = {
  onSelect: vi.fn(),
  onEdit: vi.fn(),
  onManageAccess: vi.fn(),
  onPin: vi.fn(),
  onDelete: vi.fn(),
  onSelectFile: vi.fn(),
  onSelectFolder: vi.fn(),
};

/* onetest: elitea_issues: #2723 — a bucket name long enough to ellipsize in the row is still
 * reachable in full via a hover/focus Tooltip, matching the row's other (action-icon) tooltips. */
describe('BucketList — long bucket name tooltip (#2723)', () => {
  it('shows the full bucket name in a tooltip on hover, when the row label is truncated', async () => {
    const user = userEvent.setup();
    render(
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <BucketList
          buckets={[bucket()]}
          tree={[]}
          expandedPaths={[]}
          {...NOOP}
        />
      </ThemeProvider>,
    );

    const label = screen.getByText('a-very-long-bucket-name-that-does-not-fit-the-row');
    await user.hover(label);

    expect(await screen.findByRole('tooltip')).toHaveTextContent('a-very-long-bucket-name-that-does-not-fit-the-row');
  });
});
