/**
 * `PlusChatSubmenu` / `DropdownFooter`: MUI 9.2 `MenuItem` outside a menu list.
 *
 * Both components render `MenuItem` rows under a bare `Popper`/`Paper`, not
 * under a `Menu`. In MUI 9.2 `MenuItem` calls `useMenuListContext()`
 * unconditionally, and that hook THROWS
 * `"MUI: MenuListContext is missing. MenuItems must be placed within Menu or
 * MenuList."` when no provider is above it. So every "+"-menu submenu
 * (Modules/Agents/Pipelines/Toolkits/MCPs) and every open of the participants
 * dropdown blew up the surrounding error boundary instead of showing rows.
 *
 * These render the components exactly as their real callers do — standalone,
 * with NO `Menu`/`MenuList` supplied by the test — so a regression that drops
 * the provider fails here rather than only in the browser.
 */
import { ThemeProvider } from '@mui/material/styles';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ReactNode } from 'react';
import { describe, expect, it, vi } from 'vitest';

import DropdownFooter from '@/features/chat-participants/ui/UsersParticipantDropdown/DropdownFooter';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { remToPx } from '@/shared/ui/lib/testTheme';

import { PlusChatSubmenu } from './PlusChatSubmenu';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function Harness({ children }: { children: ReactNode }) {
  return (
    <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
      {children}
    </ThemeProvider>
  );
}

describe('PlusChatSubmenu renders outside a Menu', () => {
  it('renders its item rows without a Menu/MenuList ancestor', async () => {
    const onClick = vi.fn();
    render(
      <Harness>
        <PlusChatSubmenu items={[{ key: 'a', label: 'Agent One', onClick }]} />
      </Harness>,
    );

    const row = screen.getByText('Agent One');
    await userEvent.click(row);
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it('renders the create-new row and the empty state without a Menu ancestor', () => {
    render(
      <Harness>
        <PlusChatSubmenu items={[]} showCreateNew onCreateNew={vi.fn()} createNewLabel="Create new" emptyMessage="Nothing available" />
      </Harness>,
    );

    expect(screen.getByText('Create new')).toBeInTheDocument();
    expect(screen.getByText('Nothing available')).toBeInTheDocument();
  });

  it('renders checked toggles and blocks pending rows', () => {
    const onClick = vi.fn();
    render(
      <Harness>
        <PlusChatSubmenu items={[{ key: 'tool', label: 'Toolkit', onClick, checked: true, pending: true }]} />
      </Harness>,
    );

    expect(screen.getByRole('switch')).toBeChecked();
    expect(screen.getByRole('menuitem', { name: 'Toolkit' })).toHaveAttribute('aria-disabled', 'true');
    expect(onClick).not.toHaveBeenCalled();
  });
});

describe('PlusChatSubmenu header and size (#6629)', () => {
  it('keeps the create row in the header with the search, outside the scrolling list', async () => {
    const onCreateNew = vi.fn();
    render(
      <Harness>
        <PlusChatSubmenu
          items={Array.from({ length: 40 }, (_, index) => ({ key: `a-${String(index)}`, label: `Agent ${String(index)}` }))}
          showCreateNew
          onCreateNew={onCreateNew}
          createNewLabel="Create Agent"
        />
      </Harness>,
    );
    const header = screen.getByTestId('plus-submenu-header');
    const list = screen.getByTestId('plus-submenu-list');
    const create = screen.getByTestId('plus-submenu-create-new');
    expect(header).toContainElement(create);
    expect(header).toContainElement(screen.getByRole('textbox'));
    expect(list).not.toContainElement(create);
    expect(create).toHaveTextContent('Create Agent');
    await userEvent.click(create);
    expect(onCreateNew).toHaveBeenCalledTimes(1);
  });

  it('gives every list one fixed height, full or empty', () => {
    render(
      <Harness>
        <PlusChatSubmenu items={[]} />
      </Harness>,
    );
    expect(screen.getByTestId('plus-submenu-list')).toHaveStyle({ height: remToPx('20.3125rem') });
  });

  it('draws the search and plus glyphs at 16px', () => {
    render(
      <Harness>
        <PlusChatSubmenu
          items={[]}
          showCreateNew
          onCreateNew={vi.fn()}
        />
      </Harness>,
    );
    expect(screen.getByTestId('plus-submenu-search-icon')).toHaveStyle({ width: remToPx('1rem'), height: remToPx('1rem') });
    expect(screen.getByTestId('plus-submenu-create-icon')).toHaveStyle({ width: remToPx('1rem'), height: remToPx('1rem') });
  });
});

describe('UsersParticipantDropdown footer renders outside a Menu', () => {
  it('renders the "All users" row under a bare Paper', async () => {
    const onSelectAll = vi.fn();
    render(
      <Harness>
        <DropdownFooter usersCount={3} onSelectAll={onSelectAll} />
      </Harness>,
    );

    await userEvent.click(screen.getByText('All users'));
    expect(onSelectAll).toHaveBeenCalledTimes(1);
  });
});
