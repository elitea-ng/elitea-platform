import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createFakeDoctorIpc } from '@/shared/desktop/doctorIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import TroubleshootPage from './TroubleshootPage';

describe('Settings › Troubleshoot', () => {
  it('lists every check, local and remote, and applies a repair only on a click', async () => {
    const doctor = createFakeDoctorIpc(
      [
        { id: 'dir.data', title: 'Data folder', status: 'warn', message: 'mode 755; it should be 0700.', fix_id: 'dir.tighten.data', fix_label: 'Restrict to you' },
        { id: 'local_work', title: 'Local work', status: 'warn', message: 'An administrator turns it on in Admin › Configuration › Native client policy.' },
      ],
      { 'dir.tighten.data': (checks) => checks.map((c) => (c.id === 'dir.data' ? { id: c.id, title: c.title, status: 'ok' as const, message: 'private' } : c)) },
    );
    const user = userEvent.setup();
    renderWithTheme(<TroubleshootPage ipc={doctor} />);

    expect(await screen.findByRole('heading', { name: 'Troubleshoot' })).toBeInTheDocument();
    expect(await screen.findByTestId('doctor-check-local_work')).toHaveTextContent('Native client policy');
    expect(doctor.calls.runs).toEqual(['all']);
    expect(doctor.calls.fixes).toEqual([]);
    expect(within(screen.getByTestId('doctor-check-local_work')).queryByRole('button')).toBeNull();

    await user.click(screen.getByRole('button', { name: 'Fix: Data folder' }));
    expect(await screen.findByText('Fixed.')).toBeInTheDocument();
    expect(doctor.calls.fixes).toEqual(['dir.tighten.data']);
    expect(await screen.findByText('private')).toBeInTheDocument();
  });
});
