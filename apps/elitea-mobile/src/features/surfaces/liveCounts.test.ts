import { INITIAL_TURN, type TurnState } from '@/features/chat/turnReducer';
import { liveCounts, mergePending } from '@/features/surfaces/useWidgetSnapshot';

const live = (phase: TurnState['phase'], extra: Partial<TurnState> = {}, queued = false) => ({
  turn: { ...INITIAL_TURN, phase, ...extra },
  queued,
  settled: false,
});

it('counts the running and paused live turns of the selected project only', () => {
  const pause = { kind: 'hitl', shape: 'single', interrupts: [], threadId: null, messageId: 'm' } as unknown as TurnState['pause'];
  const counts = liveCounts(
    [
      ['7:conv-a', live('streaming')],
      ['7:conv-b', live('sending')],
      ['7:conv-c', live('sending', {}, true)],
      ['7:conv-d', live('paused', { pause })],
      ['7:conv-e', live('paused', { pause: { kind: 'output_limit', threadId: null, messageId: 'm', action: 'Continue' } })],
      ['7:conv-f', live('done')],
      ['8:conv-g', live('streaming')],
    ],
    7,
  );
  expect(counts).toEqual({ running: 2, paused: ['conv-d'] });
});

it('adds the live pauses the store does not know yet, without duplicates', () => {
  expect(mergePending([{ ref: 'AAA', name: 'Known' }], ['aaa', 'bbb'])).toEqual([
    { ref: 'bbb', name: '' },
    { ref: 'AAA', name: 'Known' },
  ]);
});
