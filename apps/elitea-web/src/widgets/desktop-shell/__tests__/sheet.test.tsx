import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogTitle from '@mui/material/DialogTitle';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';

import { DesktopThemeLayer } from '../ui/DesktopThemeLayer';

function Host(): React.JSX.Element {
  const [open, setOpen] = useState(true);
  return (
    <AppProviders>
      <DesktopThemeLayer>
        <Dialog open={open} onClose={() => setOpen(false)} aria-labelledby="t">
          <DialogTitle id="t">Send this thread</DialogTitle>
          <DialogActions>
            <Button onClick={() => setOpen(false)}>Cancel</Button>
            <Button variant="contained">Send</Button>
          </DialogActions>
        </Dialog>
      </DesktopThemeLayer>
    </AppProviders>
  );
}

describe('sheets', () => {
  it('cancel with ⌘. like Escape, and keep the default button last', async () => {
    const user = userEvent.setup();
    render(<Host />);
    const dialog = await screen.findByRole('dialog', { name: 'Send this thread' });
    const buttons = Array.from(dialog.querySelectorAll('button')).map((b) => b.textContent);
    expect(buttons).toEqual(['Cancel', 'Send']);
    await user.keyboard('{Meta>}.{/Meta}');
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });

  it('cancel with Escape', async () => {
    const user = userEvent.setup();
    render(<Host />);
    await screen.findByRole('dialog');
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });
});
