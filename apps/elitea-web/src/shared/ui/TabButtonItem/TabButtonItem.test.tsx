import ToggleButtonGroup from '@mui/material/ToggleButtonGroup';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import type { TabGroupButtonItem } from '.';
import { TabButtonItem } from '.';

function renderItem(item: TabGroupButtonItem, disableTooltip?: boolean) {
  return renderWithTheme(
    <ToggleButtonGroup
      value={item.value}
      exclusive
    >
      <TabButtonItem
        item={item}
        {...(disableTooltip !== undefined ? { disableTooltip } : {})}
      />
    </ToggleButtonGroup>,
  );
}

describe('TabButtonItem', () => {
  it('renders the label as the accessible name when present', () => {
    const { getByRole } = renderItem({ value: 'list', label: 'List' });
    expect(getByRole('button', { name: 'List' })).toBeInTheDocument();
  });

  it('renders the icon', () => {
    const { getByTestId } = renderItem({
      value: 'grid',
      label: 'Grid',
      icon: <span data-testid="icon" />,
    });
    expect(getByTestId('icon')).toBeInTheDocument();
  });

  it('falls back to tooltip text for the accessible name of an icon-only button', () => {
    const { getByRole } = renderItem({ value: 'grid', tooltip: 'Grid view', icon: <span /> });
    expect(getByRole('button', { name: 'Grid view' })).toBeInTheDocument();
  });

  it('falls back to the label, then the value, when tooltip is not given', () => {
    const { getByRole, rerender } = renderItem({ value: 'grid', label: 'Grid', icon: <span /> });
    // A labeled button keeps its label as the accessible name (no aria-label override).
    expect(getByRole('button', { name: 'Grid' })).toBeInTheDocument();

    rerender(
      <ToggleButtonGroup
        value="grid"
        exclusive
      >
        <TabButtonItem item={{ value: 'grid', icon: <span /> }} />
      </ToggleButtonGroup>,
    );
    expect(getByRole('button', { name: 'grid' })).toBeInTheDocument();
  });

  it('marks the button disabled', () => {
    const { getByRole } = renderItem({ value: 'list', label: 'List', disabled: true });
    expect(getByRole('button', { name: 'List' })).toBeDisabled();
  });

  it('renders without a Tooltip wrapper when disableTooltip is set', () => {
    const { getByRole, queryByRole } = renderItem({ value: 'list', label: 'List' }, true);
    expect(getByRole('button', { name: 'List' })).toBeInTheDocument();
    // No tooltip popup is reachable without the wrapper being present at all.
    expect(queryByRole('tooltip')).not.toBeInTheDocument();
  });

  it('applies an ARRAY sx (object and function entries) together with fullWidth', () => {
    const { getByRole } = renderWithTheme(
      <ToggleButtonGroup
        value="list"
        exclusive
      >
        <TabButtonItem
          item={{ value: 'list', label: 'List' }}
          fullWidth
          disableTooltip
          sx={[{ height: '31px' }, () => ({ opacity: 0.5 })]}
        />
      </ToggleButtonGroup>,
    );
    const style = getComputedStyle(getByRole('button', { name: 'List' }));
    expect(style.height).toBe('31px');
    expect(style.opacity).toBe('0.5');
    // The equal-share sizing layered underneath the caller's styles is kept.
    expect(style.width).toBe('100%');
  });

  it('applies a function sx together with fullWidth', () => {
    const { getByRole } = renderWithTheme(
      <ToggleButtonGroup
        value="list"
        exclusive
      >
        <TabButtonItem
          item={{ value: 'list', label: 'List' }}
          fullWidth
          disableTooltip
          sx={() => ({ height: '29px' })}
        />
      </ToggleButtonGroup>,
    );
    const style = getComputedStyle(getByRole('button', { name: 'List' }));
    expect(style.height).toBe('29px');
    expect(style.width).toBe('100%');
  });
});
