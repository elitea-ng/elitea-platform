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
    projectId: 7,
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

describe('the design system on the home screen (redesign)', () => {
  const modifiersOf = (node: WidgetNode | string): { modifier: string; args: unknown[] }[] => {
    if (typeof node === 'string') return [];
    return [...((node.props.modifiers ?? []) as { modifier: string; args: unknown[] }[]), ...node.children.flatMap(modifiersOf)];
  };
  const tree = (env: Record<string, unknown>) =>
    renderWidgetSource<WidgetNode>(layouts.get(COUNTS_WIDGET), snap(true), { widgetFamily: 'systemMedium', ...env });

  it('wears the pastel gradient and Zefir rose in full colour, per scheme (the tokens)', () => {
    const { colors: color } = jest.requireActual('@/design/tokens') as typeof import('@/design/tokens');
    for (const scheme of ['light', 'dark'] as const) {
      const mods = modifiersOf(tree({ colorScheme: scheme, widgetRenderingMode: 'fullColor' }));
      const ground = mods.find((m) => m.modifier === 'containerBackground')!.args[0] as { type: string; colors: string[] };
      expect(ground.type).toBe('linearGradient');
      expect(ground.colors).toEqual([...color[scheme].backgroundGradient.stops]);
      expect(mods.filter((m) => m.modifier === 'foregroundStyle').map((m) => m.args[0])).toContain(color[scheme].accent);
    }
  });

  it('on a tinted or clear home screen the system draws the ground and colour', () => {
    const mods = modifiersOf(tree({ colorScheme: 'light', widgetRenderingMode: 'accented' }));
    expect(mods.find((m) => m.modifier === 'containerBackground')!.args[0]).toEqual({ type: 'hierarchical', style: 'quinary' });
    expect(mods.filter((m) => m.modifier === 'foregroundStyle').map((m) => m.args[0])).not.toContain('#B4306A');
  });

  it('every text follows Dynamic Type: a text style, never a fixed size', () => {
    for (const family of ['systemSmall', 'systemMedium', 'accessoryCircular', 'accessoryRectangular']) {
      const fonts = modifiersOf(render(snap(true), family)).filter((m) => m.modifier === 'font');
      expect(fonts.length).toBeGreaterThan(0);
      for (const f of fonts) expect(f.args[0]).toEqual(expect.objectContaining({ textStyle: expect.any(String) }));
      for (const f of fonts) expect((f.args[0] as { size?: number }).size).toBeUndefined();
    }
  });

  it('the small widget fits at large text sizes: capped at xLarge, the hero on one line, unread and running on one line (F6)', () => {
    const small = render(snap(false), 'systemSmall');
    expect(small.props.modifiers).toEqual(expect.arrayContaining([{ modifier: 'dynamicTypeSize', args: [{ max: 'xLarge' }] }]));
    const hero = (function find(node: WidgetNode | string): WidgetNode | null {
      if (typeof node === 'string') return null;
      if (node.type === 'Text' && node.children[0] === '2') return node;
      for (const child of node.children) {
        const hit = find(child);
        if (hit) return hit;
      }
      return null;
    })(small);
    expect(hero?.props.modifiers).toEqual(
      expect.arrayContaining([
        { modifier: 'lineLimit', args: [1] },
        { modifier: 'minimumScaleFactor', args: [0.6] },
      ]),
    );
    // The counts row: one HStack holding both the unread and the running counts.
    const countsRow = small.children.find(
      (c): c is WidgetNode => typeof c !== 'string' && c.type === 'HStack' && texts(c).includes('unread') && texts(c).includes('running'),
    );
    expect(countsRow).toBeDefined();
    // The medium widget keeps them as two lines, with the same cap.
    const medium = render(snap(false), 'systemMedium');
    expect(medium.props.modifiers).toEqual(expect.arrayContaining([{ modifier: 'dynamicTypeSize', args: [{ max: 'xLarge' }] }]));
  });
});
