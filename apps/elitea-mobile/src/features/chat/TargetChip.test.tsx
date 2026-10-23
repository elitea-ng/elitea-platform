import { fireEvent, render, screen } from '@testing-library/react-native';

import { isRemovable } from '@/features/chat/ParticipantsSheet';
import { TargetChip } from '@/features/chat/TargetChip';

const option = (participantId: number, name: string) => ({ participantId, name, agentId: participantId * 10, agentProjectId: 7, versionId: 1 });

describe('TargetChip', () => {
  it('names the addressed agent and switches to another one', async () => {
    const onChoose = jest.fn();
    const choices = [option(2, 'Research'), option(5, 'Approver')];
    await render(<TargetChip current={choices[0]} choices={choices} onChoose={onChoose} />);
    await fireEvent.press(screen.getByLabelText('To: Research'));
    await fireEvent.press(screen.getByRole('radio', { name: 'Approver' }));
    expect(onChoose).toHaveBeenCalledWith(5);
  });
});

describe('isRemovable', () => {
  it('lets mobile remove people (not me) and agents, never toolkits or models', () => {
    expect(isRemovable({ id: 1, entity_name: 'user', entity_meta: { id: 9 } }, 9)).toBe(false);
    expect(isRemovable({ id: 4, entity_name: 'user', entity_meta: { id: 42 } }, 9)).toBe(true);
    expect(isRemovable({ id: 2, entity_name: 'application' }, 9)).toBe(true);
    expect(isRemovable({ id: 3, entity_name: 'pipeline' }, 9)).toBe(true);
    expect(isRemovable({ id: 8, entity_name: 'toolkit' }, 9)).toBe(false);
    expect(isRemovable({ id: 7, entity_name: 'dummy' }, 9)).toBe(false);
  });
});
