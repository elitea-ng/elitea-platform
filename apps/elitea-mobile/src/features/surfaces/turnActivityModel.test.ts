import { INITIAL_TURN, type TurnState } from '@/features/chat/turnReducer';
import { activityPhase, activityProps, activityTitlePreview, GENERIC_TITLE, genericTitle } from '@/features/surfaces/turnActivityModel';

const at = (turn: Partial<TurnState>, queued = false) => activityPhase({ turn: { ...INITIAL_TURN, ...turn }, queued });

it('maps a live turn to what the activity says', () => {
  expect(at({ phase: 'sending' })).toBe('working');
  expect(at({ phase: 'streaming' })).toBe('working');
  expect(at({ phase: 'streaming', content: 'Partial' })).toBe('writing');
  expect(at({ phase: 'done' })).toBe('done');
  expect(at({ phase: 'done', stopped: true })).toBe('stopped');
  expect(at({ phase: 'failed', stopped: true })).toBe('stopped');
  expect(at({ phase: 'failed' })).toBe('failed');
  expect(at({ phase: 'paused', pause: { kind: 'output_limit', threadId: null, messageId: 'm', action: 'Continue' } })).toBe('done');
  expect(at({ phase: 'sending' }, true)).toBeNull();
});

it('a title only with notification_preview "title", one bounded line; a final state is never approximate', () => {
  const base = { phase: 'done' as const, conversationTitle: `  Long\n${'x'.repeat(60)}`, approximate: true, startedAt: 0 };
  expect(activityProps({ ...base, preview: 'none' }).title).toBe(genericTitle('done'));
  const titled = activityProps({ ...base, preview: 'title' });
  expect(titled.title).toHaveLength(48);
  expect(titled.title.startsWith('Long x')).toBe(true);
  expect(titled).toMatchObject({ approximate: false, detail: 'Answer ready' });
  expect(activityProps({ ...base, preview: 'title', conversationTitle: '  ' }).title).toBe(genericTitle('done'));
});

it('the title needs BOTH notification_preview "title" and allow_system_surfaces (a lock-screen surface; counts-only by default)', () => {
  expect(activityTitlePreview({ notification_preview: 'title', allow_system_surfaces: false })).toBe('none');
  expect(activityTitlePreview({ notification_preview: 'none', allow_system_surfaces: true })).toBe('none');
  expect(activityTitlePreview({ notification_preview: 'title', allow_system_surfaces: true })).toBe('title');
});

it('the generic headline matches the phase: a finished or paused answer never says "is answering" (seen on the simulator lock screen)', () => {
  const props = (phase: Parameters<typeof activityProps>[0]['phase']) =>
    activityProps({ phase, preview: 'none', conversationTitle: null, approximate: false, startedAt: 0 });
  expect(props('working').title).toBe(GENERIC_TITLE);
  expect(props('tool').title).toBe(GENERIC_TITLE);
  expect(props('writing').title).toBe(GENERIC_TITLE);
  for (const phase of ['done', 'failed', 'stopped', 'waiting'] as const) {
    expect(props(phase).title).not.toMatch(/is answering/);
  }
});
