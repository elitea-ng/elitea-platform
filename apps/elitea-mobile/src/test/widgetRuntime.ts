/**
 * jest: a stand-in for the expo-widgets JS runtime of the widget extension.
 * babel-preset-expo serialises a 'widget' function to its source; the
 * extension evaluates that source with the SwiftUI views and modifiers as
 * globals. This evaluates it the same way, with views that build a plain
 * tree, so a test can read what a widget or Live Activity would draw.
 */

export interface WidgetNode {
  type: string;
  props: Record<string, unknown>;
  children: (WidgetNode | string)[];
}

export interface Modifier {
  modifier: string;
  args: unknown[];
}

const VIEWS = ['Text', 'VStack', 'HStack', 'ZStack', 'Image', 'Spacer', 'Label', 'ProgressView', 'Gauge', 'Group', 'Button', 'Link'];
const MODIFIERS = [
  'font',
  'foregroundStyle',
  'foregroundColor',
  'padding',
  'frame',
  'lineLimit',
  'widgetURL',
  'containerBackground',
  'activityBackgroundTint',
  'monospacedDigit',
  'bold',
  'opacity',
  'multilineTextAlignment',
];

function normaliseChildren(children: unknown): (WidgetNode | string)[] {
  const list = Array.isArray(children) ? children.flat(Infinity) : [children];
  return list
    .filter((c) => c !== null && c !== undefined && c !== false && c !== true)
    .map((c) => (typeof c === 'object' ? (c as WidgetNode) : String(c)));
}

function jsx(type: string, props: Record<string, unknown> | null): WidgetNode {
  const { children, ...rest } = props ?? {};
  return { type, props: rest, children: normaliseChildren(children) };
}

/** Evaluates a serialised 'widget' function and calls it. */
export function renderWidgetSource<T>(source: unknown, props: unknown, environment: Record<string, unknown>): T {
  if (typeof source !== 'string') throw new Error('expected the serialised widget source (babel widgets plugin)');
  const globals: Record<string, unknown> = { _jsx: jsx, _jsxs: jsx, _Fragment: 'Fragment' };
  for (const view of VIEWS) globals[view] = view;
  for (const name of MODIFIERS) globals[name] = (...args: unknown[]): Modifier => ({ modifier: name, args });
  const names = Object.keys(globals);
  // The widget runtime evaluates the layout source the same way.
  const fn = new Function(...names, `return (${source});`)(...names.map((n) => globals[n])) as (p: unknown, e: unknown) => T;
  return fn(props, environment);
}

/** Every text drawn, in order. */
export function texts(node: WidgetNode | string | null | undefined): string[] {
  if (node === null || node === undefined) return [];
  if (typeof node === 'string') return [node];
  const own = node.type === 'Text' ? [node.children.filter((c) => typeof c === 'string').join('')] : [];
  return [...own, ...node.children.filter((c): c is WidgetNode => typeof c !== 'string').flatMap(texts)];
}

/** The widgetURL modifiers' URLs anywhere in the tree. */
export function widgetUrls(node: WidgetNode | string | null | undefined): string[] {
  if (node === null || node === undefined || typeof node === 'string') return [];
  const mods = (Array.isArray(node.props.modifiers) ? node.props.modifiers : []) as Modifier[];
  const own = mods.filter((m) => m.modifier === 'widgetURL').map((m) => String(m.args[0]));
  return [...own, ...node.children.flatMap(widgetUrls)];
}

/** The start dates (ms) of every system-driven timer Text (`dateStyle: 'timer'`) in the tree. */
export function timers(node: WidgetNode | string | null | undefined): number[] {
  if (node === null || node === undefined || typeof node === 'string') return [];
  const date = node.props.date;
  const own = node.type === 'Text' && node.props.dateStyle === 'timer' && date instanceof Date ? [date.getTime()] : [];
  return [...own, ...node.children.flatMap(timers)];
}
