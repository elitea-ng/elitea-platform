import { ThemeProvider } from '@mui/material/styles';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import type { ArtifactListItem } from '../model/types';
import { ArtifactGridRow } from './ArtifactGridRow';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const LONG_NAME = 'a-file-name-long-enough-to-be-clipped-by-the-fixed-width-name-column.txt';

function item(overrides: Partial<ArtifactListItem> = {}): ArtifactListItem {
  return {
    id: LONG_NAME,
    key: LONG_NAME,
    name: LONG_NAME,
    kind: 'file',
    size: 0,
    ...overrides,
  };
}

const NOOP = {
  onToggle: vi.fn(),
  onOpen: vi.fn(),
  onPreview: vi.fn(),
  onDownload: vi.fn(),
  onDelete: vi.fn(),
};

/* onetest: elitea_issues: #4447, #3758 — the fixed-width name column that clips a long file/folder
 * name (no resize handle) still exposes the full name via a hover/focus Tooltip. */
describe('ArtifactGridRow — long name tooltip (issues 4447, 3758)', () => {
  it('shows the full file name in a tooltip on hover, when the name cell is truncated', async () => {
    const user = userEvent.setup();
    render(
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <table>
          <tbody>
            <ArtifactGridRow
              item={item()}
              columns={[]}
              gridTemplateColumns="3rem 1fr 7rem"
              values={{}}
              selected={false}
              {...NOOP}
            />
          </tbody>
        </table>
      </ThemeProvider>,
    );

    const label = screen.getByText(LONG_NAME);
    await user.hover(label);

    expect(await screen.findByRole('tooltip')).toHaveTextContent(LONG_NAME);
  });
});
