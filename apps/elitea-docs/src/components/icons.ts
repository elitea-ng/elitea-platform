/**
 * Icon names in the docs are lucide names (`rocket`, `shield-check`), plus a
 * few older Font Awesome-style names the pages were written with, aliased
 * below. Rendered at build time to an inline SVG; an unknown name fails the
 * build, so a page can never ship an icon that silently draws nothing.
 */
import { icons, type IconNode } from 'lucide';

const ALIASES: Record<string, string> = {
  bolt: 'zap',
  book: 'book-open',
  'box-archive': 'archive',
  comments: 'messages-square',
  'diagram-project': 'workflow',
  'layer-group': 'layers',
  layout: 'layout-dashboard',
  'microchip-ai': 'cpu',
  microphone: 'mic',
  'microphone-lines': 'mic',
  robot: 'bot',
  'shield-halved': 'shield-half',
  sliders: 'sliders-horizontal',
  'volume-high': 'volume-2',
  'waveform-lines': 'audio-waveform',
};

function pascal(name: string): string {
  return name
    .split('-')
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join('');
}

function escapeAttribute(value: string | number): string {
  return String(value).replaceAll('&', '&amp;').replaceAll('"', '&quot;').replaceAll('<', '&lt;');
}

export function iconSvg(name: string, size = 20): string {
  const node: IconNode | undefined = icons[pascal(ALIASES[name] ?? name) as keyof typeof icons];
  if (node === undefined) throw new Error(`docs icon "${name}" is not a lucide icon or a known alias`);
  const children = node
    .map(([tag, attributes]) => {
      const rendered = Object.entries(attributes)
        .map(([key, value]) => `${key}="${escapeAttribute(value as string | number)}"`)
        .join(' ');
      return `<${tag} ${rendered}/>`;
    })
    .join('');
  return `<svg xmlns="http://www.w3.org/2000/svg" width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${children}</svg>`;
}
