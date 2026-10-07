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
});
