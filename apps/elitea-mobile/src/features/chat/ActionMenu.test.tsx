import { fireEvent, render, screen } from '@testing-library/react-native';

import { ActionMenu } from '@/features/chat/ActionMenu';
import { TargetChip } from '@/features/chat/TargetChip';

const items = [{ key: 'copy', label: 'Copy', icon: 'copy' as const, onPress: jest.fn() }];

describe('ActionMenu closes from inside the VoiceOver-modal sheet', () => {
  it('has a Cancel row inside the sheet (iOS), which closes it', async () => {
    const onClose = jest.fn();
    await render(<ActionMenu visible title="Message actions" items={items} onClose={onClose} />);
    const cancel = screen.getByRole('button', { name: 'Cancel' });
    // It sits inside the accessibilityViewIsModal view, where VoiceOver can reach it.
    let inModal = false;
    for (let n = cancel.parent; n; n = n.parent) if (n.props.accessibilityViewIsModal) inModal = true;
    expect(inModal).toBe(true);
    await fireEvent.press(cancel);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('the VoiceOver escape scrub closes it', async () => {
    const onClose = jest.fn();
    await render(<ActionMenu visible title="Message actions" items={items} onClose={onClose} />);
    let modal = screen.getByRole('button', { name: 'Copy' }).parent;
    while (modal && !modal.props.accessibilityViewIsModal) modal = modal.parent;
    modal!.props.onAccessibilityEscape();
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('the Send-to sheet can be left without picking an agent again', async () => {
    const onChoose = jest.fn();
    const choices = [
      { participantId: 2, name: 'Research', agentId: 20, agentProjectId: 7, versionId: 1 },
      { participantId: 5, name: 'Approver', agentId: 50, agentProjectId: 7, versionId: 1 },
    ];
    await render(<TargetChip current={choices[0]} choices={choices} onChoose={onChoose} />);
    await fireEvent.press(screen.getByLabelText('To: Research'));
    await fireEvent.press(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('radio', { name: 'Approver' })).toBeNull();
    expect(onChoose).not.toHaveBeenCalled();
  });
});
