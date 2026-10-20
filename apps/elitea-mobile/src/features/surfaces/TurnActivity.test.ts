import * as widgetsMock from 'expo-widgets';

import { TURN_ACTIVITY } from '@/features/surfaces/TurnActivity';
import { activityProps } from '@/features/surfaces/turnActivityModel';
import { renderWidgetSource, texts, timers, type WidgetNode } from '@/test/widgetRuntime';

const { layouts } = widgetsMock as unknown as { layouts: Map<string, unknown> };

const render = (props: ReturnType<typeof activityProps>) =>
  renderWidgetSource<Record<string, WidgetNode | null>>(layouts.get(TURN_ACTIVITY), props, { colorScheme: 'light', isLuminanceReduced: false });

it('lock screen banner and Dynamic Island: generic headline and phase', () => {
  const layout = render(
    activityProps({ phase: 'tool', preview: 'none', conversationTitle: 'Contract review', approximate: false, startedAt: Date.now() - 3 * 60_000 }),
  );
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

it('the timer keeps to a fixed trailing frame, so the title gets the banner width (#41)', () => {
  const layout = render(activityProps({ phase: 'tool', preview: 'none', conversationTitle: null, approximate: false, startedAt: Date.now() - 60_000 }));
  const timerOf = (node: WidgetNode | null | undefined): WidgetNode | null => {
    if (!node) return null;
    if (node.type === 'Text' && node.props.dateStyle === 'timer') return node;
    for (const child of node.children) {
      if (typeof child !== 'string') {
        const found = timerOf(child);
        if (found) return found;
      }
    }
    return null;
  };
  for (const slot of [layout.banner, layout.compactTrailing, layout.expandedTrailing]) {
    const mods = (timerOf(slot)?.props.modifiers ?? []) as { modifier: string; args: unknown[] }[];
    expect(mods.find((m) => m.modifier === 'frame')?.args[0]).toMatchObject({ alignment: 'trailing', width: expect.any(Number) });
  }
});

it('takes Zefir rose and the app ground per scheme, white on the dimmed lock screen', () => {
  const props = activityProps({ phase: 'working', preview: 'none', conversationTitle: null, approximate: false, startedAt: Date.now() });
  const draw = (env: Record<string, unknown>) => renderWidgetSource<Record<string, WidgetNode | null>>(layouts.get(TURN_ACTIVITY), props, env);
  const tint = (layout: Record<string, WidgetNode | null>) =>
    ((layout.banner!.props.modifiers ?? []) as { modifier: string; args: unknown[] }[]).find((m) => m.modifier === 'activityBackgroundTint')!.args[0];
  expect(draw({ colorScheme: 'light' }).minimal!.props.color).toBe('#B4306A');
  expect(draw({ colorScheme: 'dark' }).minimal!.props.color).toBe('#FF8CB8');
  expect(draw({ colorScheme: 'dark', isLuminanceReduced: true }).minimal!.props.color).toBe('#FFFFFF');
  expect(tint(draw({ colorScheme: 'light' }))).toBe('#FBF7F9');
  expect(tint(draw({ colorScheme: 'dark' }))).toBe('#1D1A22');
});
