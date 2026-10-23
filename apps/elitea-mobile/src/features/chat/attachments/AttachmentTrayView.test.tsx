import { fireEvent, render, screen } from '@testing-library/react-native';

import { AttachmentTrayView } from '@/features/chat/attachments/AttachmentTrayView';
import { SAMPLE_TRAY } from '@/features/designGallery/chatFixtures';

describe('AttachmentTrayView', () => {
  it('an uploading file keeps its × and it cancels the upload', async () => {
    const onRemove = jest.fn();
    await render(<AttachmentTrayView items={SAMPLE_TRAY} onRemove={onRemove} onRetry={jest.fn()} />);
    await fireEvent.press(screen.getByRole('button', { name: 'Cancel upload of budget.xlsx' }));
    expect(onRemove).toHaveBeenCalledWith('f2');
    expect(screen.getByRole('button', { name: 'Remove launch-plan.pdf' })).toBeTruthy();
  });

  it("a busy document's type label is not veiled, and names truncate in the middle (the extension stays)", async () => {
    await render(<AttachmentTrayView items={SAMPLE_TRAY} onRemove={jest.fn()} onRetry={jest.fn()} />);
    expect(screen.getByTestId('tray-busy')).toBeTruthy();
    // No scrim veil is drawn on a document tile.
    expect(JSON.stringify(screen.toJSON())).not.toContain('"opacity":0.85');
    expect(screen.getByText('XLSX')).toBeTruthy();
    expect(screen.getByText('budget.xlsx').props.ellipsizeMode).toBe('middle');
  });

  it('a scan shows its short title with the date on the second line, so the date is never truncated away (F4)', async () => {
    const scan = { ...SAMPLE_TRAY[0], id: 's1', name: 'Scan 2026-10-08 14.05.pdf', size: 1300, state: 'ready' as const, display: { title: 'Scan 14.05', detail: 'Oct 8' } };
    await render(<AttachmentTrayView items={[scan]} onRemove={jest.fn()} onRetry={jest.fn()} />);
    expect(screen.getByText('Scan 14.05')).toBeTruthy();
    expect(screen.getByText('Oct 8 · 1 KB')).toBeTruthy();
    // VoiceOver and the remove button still name the file in full.
    expect(screen.getByLabelText('Scan 2026-10-08 14.05.pdf, ready')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Remove Scan 2026-10-08 14.05.pdf' })).toBeTruthy();
  });
});
