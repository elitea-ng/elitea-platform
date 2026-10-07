import * as widgetsMock from 'expo-widgets';


import { COUNTS_WIDGET } from '@/features/surfaces/CountsWidget';
import { buildSnapshot, signedOutSnapshot, type SurfaceSnapshot } from '@/features/surfaces/surfaceSnapshot';
import { renderWidgetSource, texts, widgetUrls, type WidgetNode } from '@/test/widgetRuntime';

/**
 * The widget layout as the extension runs it: the serialised 'widget'
 * function, evaluated with stub SwiftUI views (src/test/widgetRuntime.ts).
 */

// The jest mock (__mocks__/expo-widgets.ts) keeps each registered layout.
const { layouts } = widgetsMock as unknown as { layouts: Map<string, unknown> };

const NOW = Date.UTC(2026, 9, 6, 12);
const snap = (allowTitles: boolean, approvals = 2, unread = 3, running = 1): SurfaceSnapshot =>
  buildSnapshot({
    workspaceId: 'aaaaaaaaaaaa',
    origin: 'https://acme-legal.elitea.example',
    allowTitles,
    approvals: Array.from({ length: approvals }, (_, i) => ({ ref: String(40 + i), name: `Contract ${i + 1}` })),
    unread,
    running,
    now: NOW,
  });

const render = (props: SurfaceSnapshot, widgetFamily: string) =>
  renderWidgetSource<WidgetNode>(layouts.get(COUNTS_WIDGET), props, { widgetFamily, colorScheme: 'light' });

it('is registered with the name app.config.ts declares, with the signed-out snapshot as its first content', () => {
  expect(typeof layouts.get(COUNTS_WIDGET)).toBe('string');
});

it('small: the three counts, no title or host by default', () => {
  const tree = render(snap(false), 'systemSmall');
  expect(texts(tree)).toEqual(['Agent Zefir', '2', 'waiting for you', '3', 'unread', '1', 'running']);
  // A decision waiting wins the tap.
  expect(widgetUrls(tree)).toEqual(['agentzefir://surface/home?ws=aaaaaaaaaaaa']);
});

it('medium: names the waiting conversations and the host only when titles are allowed', () => {
  expect(texts(render(snap(false), 'systemMedium')).join(' ')).not.toMatch(/Contract|acme/);
  const titled = texts(render(snap(true), 'systemMedium'));
  expect(titled[0]).toBe('acme-legal.elitea.example');
  expect(titled).toEqual(expect.arrayContaining(['Contract 1', 'Contract 2']));
});

it('lock screen: inline and circular counts; a tap opens the inbox when nothing waits', () => {
  expect(texts(render(snap(false, 0, 120, 0), 'accessoryInline'))).toEqual(['99+ unread']);
  expect(texts(render(snap(false, 0, 0, 0), 'accessoryInline'))).toEqual(['All caught up']);
  const circular = render(snap(false, 0, 4, 0), 'accessoryCircular');
  expect(texts(circular)).toEqual(['4']);
  expect(widgetUrls(circular)).toEqual(['agentzefir://surface/notifications?ws=aaaaaaaaaaaa']);
  expect(texts(render(snap(false, 1, 0, 2), 'accessoryRectangular'))).toEqual(['1', 'waiting for you', '0', 'unread', '2', 'running']);
});

it('signed out (wiped, switching): no counts, asks to open the app', () => {
  for (const family of ['systemSmall', 'systemMedium', 'accessoryRectangular']) {
    expect(texts(render(signedOutSnapshot(NOW), family))).toEqual(['Agent Zefir', 'Open the app to sign in.']);
  }
  expect(texts(render(signedOutSnapshot(NOW), 'accessoryInline'))).toEqual(['Agent Zefir']);
  expect(widgetUrls(render(signedOutSnapshot(NOW), 'systemSmall'))).toEqual(['agentzefir://surface/home']);
});

it('draws before the app ever wrote a snapshot', () => {
  expect(() => render(undefined as unknown as SurfaceSnapshot, 'systemSmall')).not.toThrow();
});
