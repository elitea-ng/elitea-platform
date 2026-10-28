/**
 * The hover submenu: it opens beside its row on hover, on a click and from
 * the keyboard, stays open while the pointer travels into it, and its rows
 * act without the parent menu taking their keys.
 */
import { useState } from 'react';

import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import Menu from '@mui/material/Menu';

import { renderWithTheme as render } from '../lib/testTheme';

import { MenuRow, MenuRowText } from './nativeMenu';
import { NativeSubmenu } from './NativeSubmenu';

function Harness({ onPick }: { onPick: (value: string) => void }): React.JSX.Element {
  const [open, setOpen] = useState(true);
  return (
    <Menu open={open} anchorEl={document.body} onClose={() => setOpen(false)}>
      <MenuRow data-testid="plain">
        <MenuRowText>Plain</MenuRowText>
      </MenuRow>
      <NativeSubmenu label="Language" hint="English" testId="language">
        {(close) =>
          ['English', 'Español'].map((value) => (
            <MenuRow
              key={value}
              onClick={() => {
                close();
                setOpen(false);
                onPick(value);
              }}
            >
              <MenuRowText>{value}</MenuRowText>
            </MenuRow>
          ))
        }
      </NativeSubmenu>
    </Menu>
  );
}

describe('NativeSubmenu', () => {
  it('opens on hover and closes once the pointer settles elsewhere', async () => {
    const user = userEvent.setup();
    render(<Harness onPick={vi.fn()} />);
    const row = screen.getByTestId('language');
    expect(row).toHaveAttribute('aria-expanded', 'false');
    await user.hover(row);
    expect(await screen.findByRole('menuitem', { name: 'Español' })).toBeInTheDocument();
    expect(row).toHaveAttribute('aria-expanded', 'true');
    // The pointer crosses into the submenu: it stays.
    await user.hover(screen.getByRole('menuitem', { name: 'Español' }));
    await new Promise((resolve) => setTimeout(resolve, 300));
    expect(row).toHaveAttribute('aria-expanded', 'true');
    // And settles on another row of the parent: it goes.
    await user.hover(screen.getByTestId('plain'));
    await waitFor(() => expect(row).toHaveAttribute('aria-expanded', 'false'));
  });

  it('opens from the keyboard on its first row, goes back with the arrow, and its row acts', async () => {
    const user = userEvent.setup();
    const onPick = vi.fn<(value: string) => void>();
    render(<Harness onPick={onPick} />);
    screen.getByTestId('language').focus();
    await user.keyboard('{ArrowRight}');
    await waitFor(() => expect(screen.getByRole('menuitem', { name: 'English' })).toHaveFocus());
    await user.keyboard('{ArrowLeft}');
    await waitFor(() => expect(screen.getByTestId('language')).toHaveFocus());
    expect(screen.getByTestId('language')).toHaveAttribute('aria-expanded', 'false');
    await user.click(screen.getByTestId('language'));
    await user.click(await screen.findByRole('menuitem', { name: 'Español' }));
    expect(onPick).toHaveBeenCalledWith('Español');
  });
});
