import type { ReactTestRendererJSON } from 'react-test-renderer';

type Rendered = ReactTestRendererJSON | ReactTestRendererJSON[] | null;

/** The text of every host element that is `selectable` (the system Copy/Share menu can take it). */
export function selectableTexts(tree: Rendered): string[] {
  const out: string[] = [];
  const textOf = (node: ReactTestRendererJSON | string): string =>
    typeof node === 'string' ? node : (node.children ?? []).map(textOf).join('');
  const walk = (node: ReactTestRendererJSON | string | null) => {
    if (node === null || typeof node === 'string') return;
    if (node.props.selectable === true) out.push(textOf(node));
    for (const child of node.children ?? []) walk(child);
  };
  for (const node of Array.isArray(tree) ? tree : [tree]) walk(node);
  return out;
}

/**
 * Everything the system Copy/Share menu could take: selectable text, plus the
 * value of every text field that shows one with its context menu still on
 * (a TextInput always offers Select All / Copy / Share unless
 * `contextMenuHidden`). With `allow_share_out` off this must be empty.
 */
export function copyableTexts(tree: Rendered): string[] {
  const out = selectableTexts(tree);
  const walk = (node: ReactTestRendererJSON | string | null) => {
    if (node === null || typeof node === 'string') return;
    const value: unknown = node.props.value ?? node.props.defaultValue;
    if (node.type === 'TextInput' && typeof value === 'string' && value !== '' && node.props.contextMenuHidden !== true) out.push(value);
    for (const child of node.children ?? []) walk(child);
  };
  for (const node of Array.isArray(tree) ? tree : [tree]) walk(node);
  return out;
}
