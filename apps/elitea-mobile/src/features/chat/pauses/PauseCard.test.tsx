import { fireEvent, render, screen, waitFor } from '@testing-library/react-native';

import { goldenData, nodeOf } from '@/features/chat/__fixtures__/clientFrames';
import { PauseCard, jsonObjectError } from '@/features/chat/pauses/PauseCard';
import { pauseOf, type TurnPause } from '@/features/chat/toolActivity';
import { resetPolicy, setPolicy, STRICT_DEFAULTS } from '@/security/policy';
import { copyableTexts } from '@/test/selectable';

const CONV = '0b6c1f8e-1d2a-4f3b-8c4d-5e6f7a8b9c0d';
const target = { projectId: 7, conversationUuid: CONV, messageId: 'resp-1', threadId: 'thread-1' };
const golden = (worker: 'python' | 'rust', name: string) => pauseOf(nodeOf(goldenData(worker, name))) as TurnPause;

async function renderCard(pause: TurnPause, extra: Partial<Parameters<typeof PauseCard>[0]> = {}) {
  const onContinue = jest.fn(async (..._args: unknown[]) => null as string | null);
  const result = await render(<PauseCard pause={pause} target={target} offline={false} onContinue={onContinue} onOpenWeb={null} {...extra} />);
  return { onContinue, result };
}

describe('HITL card', () => {
  it('offers the pause actions and approves with the decision body', async () => {
    const { onContinue } = await renderCard(golden('python', 'hitl_interrupt'));
    await screen.findByText('github · delete_branch');
    expect(screen.getByRole('button', { name: 'Reject' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Edit' })).toBeTruthy();
    await fireEvent.press(screen.getByRole('button', { name: 'Approve' }));
    await waitFor(() => expect(onContinue).toHaveBeenCalled());
    expect(onContinue).toHaveBeenCalledWith('agent.continue.hitl.v1', expect.objectContaining({
      hitl_decisions: [{ interrupt_id: 'interrupt-1', tool_call_id: 'call-1', action: 'approve', value: '' }],
    }));
  });

  it('masks the tool arguments until a value is tapped', async () => {
    await renderCard(golden('python', 'hitl_interrupt'));
    await fireEvent.press(await screen.findByText('Arguments (1)'));
    expect(screen.getByLabelText('branch, hidden. Tap to show.')).toBeTruthy();
    await fireEvent.press(screen.getByLabelText('branch, hidden. Tap to show.'));
    expect(screen.getByLabelText('branch: old')).toBeTruthy();
  });

  it('edits the arguments as JSON and sends them as text', async () => {
    const { onContinue } = await renderCard(golden('python', 'hitl_interrupt'));
    await fireEvent.press(await screen.findByRole('button', { name: 'Edit' }));
    const editor = await screen.findByLabelText('Edited value');
    await fireEvent.changeText(editor, '{"branch": ');
    expect(screen.getByText('This is not valid JSON.')).toBeTruthy();
    await fireEvent.changeText(editor, '{"branch":"main"}');
    await fireEvent.press(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(onContinue).toHaveBeenCalled());
    expect(onContinue.mock.calls[0][1]).toMatchObject({
      hitl_decisions: [{ action: 'edit', value: '{"branch":"main"}' }],
    });
  });

  it('allow_share_out off: the Edit editor still edits, but its prefilled tool arguments cannot be copied or shared', async () => {
    setPolicy({ ...STRICT_DEFAULTS, allow_share_out: false }, 'server');
    try {
      await renderCard(golden('python', 'hitl_interrupt'));
      await fireEvent.press(await screen.findByRole('button', { name: 'Edit' }));
      const editor = await screen.findByLabelText('Edited value');
      expect(editor.props.value).toContain('"branch"');
      expect(editor.props.contextMenuHidden).toBe(true);
      expect(copyableTexts(screen.toJSON())).toEqual([]);
    } finally {
      resetPolicy();
    }
  });

  it('allow_share_out on: the Edit editor keeps the system menu (copy, paste)', async () => {
    setPolicy({ ...STRICT_DEFAULTS, allow_share_out: true }, 'server');
    try {
      await renderCard(golden('python', 'hitl_interrupt'));
      await fireEvent.press(await screen.findByRole('button', { name: 'Edit' }));
      expect((await screen.findByLabelText('Edited value')).props.contextMenuHidden).toBe(false);
    } finally {
      resetPolicy();
    }
  });

  it('cannot be answered offline', async () => {
    const { onContinue } = await renderCard(golden('python', 'hitl_interrupt'), { offline: true });
    expect(await screen.findByText("You're offline.")).toBeTruthy();
    await fireEvent.press(screen.getByRole('button', { name: 'Approve' }));
    expect(onContinue).not.toHaveBeenCalled();
  });

  it('shows the server problem (e.g. answered elsewhere) under the card', async () => {
    const onContinue = jest.fn(async () => 'Answered elsewhere.');
    await render(<PauseCard pause={golden('python', 'hitl_interrupt')} target={target} offline={false} onContinue={onContinue} onOpenWeb={null} />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Approve' }));
    expect(await screen.findByText('Answered elsewhere.')).toBeTruthy();
  });
});

describe('ask_user card', () => {
  const pause: TurnPause = {
    kind: 'hitl',
    shape: 'single',
    threadId: 'thread-1',
    messageId: 'resp-1',
    interrupts: [
      {
        interruptId: 'int-ask-1',
        toolCallId: 'call-ask',
        message: null,
        availableActions: ['answer'],
        guardrailType: 'clarifying_question',
        actionLabel: null,
        policyMessage: null,
        toolName: 'ask_user',
        toolkitName: null,
        toolArgs: null,
        questions: [{ id: 'environment', question: 'Which environment?', options: [{ label: 'Staging' }, { label: 'Prod' }] }],
        parentAgentName: null,
        childThreadId: null,
      },
    ],
  };

  it('submits only once every required question is answered', async () => {
    const { onContinue } = await renderCard(pause);
    const submit = await screen.findByRole('button', { name: 'Submit' });
    expect(submit.props.accessibilityState).toMatchObject({ disabled: true });
    await fireEvent.press(screen.getByRole('radio', { name: 'Staging' }));
    await fireEvent.press(screen.getByRole('button', { name: 'Submit' }));
    await waitFor(() => expect(onContinue).toHaveBeenCalled());
    expect(onContinue.mock.calls[0][1]).toMatchObject({
      hitl_decisions: [{ interrupt_id: 'int-ask-1', action: 'answer', value: { environment: 'Staging' } }],
    });
  });
});

describe('sign-in and output-limit cards', () => {
  it('Skip continues without the tool; Open in web is offered', async () => {
    const onOpenWeb = jest.fn();
    const { onContinue } = await renderCard(golden('python', 'mcp_authorization_terminal'), { onOpenWeb });
    expect(await screen.findByText('example needs sign-in')).toBeTruthy();
    await fireEvent.press(screen.getByRole('button', { name: 'Open in web' }));
    expect(onOpenWeb).toHaveBeenCalled();
    await fireEvent.press(screen.getByRole('button', { name: 'Skip' }));
    await waitFor(() =>
      expect(onContinue).toHaveBeenCalledWith('agent.continue.authorization.v1', expect.objectContaining({ authorization_request_id: 'auth-1', authorization_action: 'skip' })),
    );
  });

  it('Continue after the output limit', async () => {
    const { onContinue } = await renderCard(golden('rust', 'requires_confirmation'));
    await fireEvent.press(await screen.findByRole('button', { name: 'Continue' }));
    await waitFor(() => expect(onContinue).toHaveBeenCalledWith('agent.continue.output-limit.v1', expect.objectContaining({ message_id: 'resp-1' })));
  });

  it('says when the pause cannot be addressed from mobile', async () => {
    await renderCard(golden('rust', 'requires_confirmation'), { target: null });
    expect(await screen.findByText('This pause can only be answered in the web app.')).toBeTruthy();
  });
});

describe('jsonObjectError', () => {
  it('accepts only a JSON object', () => {
    expect(jsonObjectError('{"a":1}')).toBeNull();
    expect(jsonObjectError('[1]')).toBe('The arguments must be a JSON object.');
    expect(jsonObjectError('nope')).toBe('This is not valid JSON.');
  });
});

describe('HITL card (captured, python worker sensitive tool)', () => {
  // eslint-disable-next-line @typescript-eslint/no-require-imports
  const captured = require('../__fixtures__/captured/python-sensitive-tool-pause.json') as { frames: Record<string, unknown>[] };
  const pause = pauseOf(nodeOf(captured.frames[2])) as TurnPause;
  const POLICY = "Your organization requires approval before running the sensitive action 'MockTools.mock_tool_status'.";

  it('says the policy once when the message repeats it', async () => {
    await renderCard(pause);
    await screen.findByRole('button', { name: 'Approve' });
    expect(screen.getAllByText(POLICY)).toHaveLength(1);
  });
});
