import * as widgetsMock from 'expo-widgets';

import { TURN_ACTIVITY } from '@/features/surfaces/TurnActivity';
import { activityProps } from '@/features/surfaces/turnActivityModel';
import { renderWidgetSource, texts, timers, type WidgetNode } from '@/test/widgetRuntime';

const { layouts } = widgetsMock as unknown as { layouts: Map<string, unknown> };

const render = (props: ReturnType<typeof activityProps>) =>
  renderWidgetSource<Record<string, WidgetNode | null>>(layouts.get(TURN_ACTIVITY), props, { colorScheme: 'light', isLuminanceReduced: false });

it('lock screen banner and Dynamic Island: generic headline and phase', () => {
  const layout = render(activityProps({ phase: 'tool', preview: 'none', conversationTitle: 'Contract review', approximate: false, startedAt: Date.now() - 3 * 60_000 }));
  expect(texts(layout.banner).filter(Boolean)).toEqual(['Your agent is answering', 'Using a tool…']);
  expect(texts(layout.expandedCenter)).toEqual(['Your agent is answering']);
  expect(layout.minimal?.type).toBe('Image');
});

it('the elapsed time is a system timer counting up from the start, not a string frozen at the last update', () => {
  const startedAt = Date.now() - 3 * 60_000;
  const layout = render(activityProps({ phase: 'tool', preview: 'none', conversationTitle: null, approximate: false, startedAt }));
  expect(timers(layout.banner)).toEqual([startedAt]);
  expect(timers(layout.compactTrailing)).toEqual([startedAt]);
  expect(timers(layout.expandedTrailing)).toEqual([startedAt]);
  // Rendered 5 s in: still a running timer (a computed "N min" would be empty and stay empty).
  const early = render(activityProps({ phase: 'working', preview: 'none', conversationTitle: null, approximate: false, startedAt: Date.now() - 5000 }));
  expect(timers(early.banner)).toHaveLength(1);
});

it('a finished or paused activity shows no running timer', () => {
  for (const phase of ['done', 'failed', 'waiting', 'stopped'] as const) {
    const layout = render(activityProps({ phase, preview: 'none', conversationTitle: null, approximate: false, startedAt: Date.now() - 60_000 }));
    expect([...timers(layout.banner), ...timers(layout.compactTrailing), ...timers(layout.expandedTrailing)]).toEqual([]);
  }
});

it('approximate after a suspend', () => {
  const layout = render(activityProps({ phase: 'writing', preview: 'title', conversationTitle: 'Contract review', approximate: true, startedAt: Date.now() }));
  expect(texts(layout.banner).filter(Boolean)).toEqual(['Contract review', 'Open the app for the latest']);
  expect(texts(layout.compactTrailing)).toEqual(['…']);
});
