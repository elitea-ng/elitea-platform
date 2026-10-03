/**
 * #6687: every divider in the Artifacts views takes `border.artifactDivider`
 * (Gray40 #262B34 dark, Light40 #E1E5E9 light), and a selected row keeps its
 * 1px rule (in transparent) so the rows do not move by a pixel.
 */
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { ThemeProvider } from '@mui/material/styles';
import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { Bucket } from '@/entities/bucket';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import { BucketList } from './BucketList';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

/** The Artifacts files that draw a divider, relative to `src/`. */
const DIVIDER_FILES = [
  'features/artifacts/ui/ArtifactGridRow.tsx',
  'features/artifacts/ui/ArtifactPagination.tsx',
  'features/artifacts/ui/BucketList.tsx',
  'features/artifacts/ui/BucketPanelHeader.tsx',
  'features/artifacts/ui/BucketStorageSelector.tsx',
  'features/artifacts/ui/BucketTreeItem.tsx',
  'features/artifacts/ui/FilePreviewCanvas.tsx',
  'pages/artifacts/Artifacts.tsx',
] as const;

const SRC = resolve(__dirname, '../../..');

function bucket(id: string): Bucket {
  return { id, name: id, isPinned: false, createdAt: '2026-01-01T00:00:00Z', retentionDays: null, sizeBytes: 0 };
}

/** The nearest ancestor of `element` that draws a bottom rule. */
function ruledAncestor(element: HTMLElement): HTMLElement {
  for (let node: HTMLElement | null = element; node; node = node.parentElement) {
    const style = getComputedStyle(node);
    if (style.borderBottomStyle === 'solid' && style.borderBottomWidth !== '0px') return node;
  }
  throw new Error('no ancestor draws a bottom rule');
}

/** The red, green and blue channels of a six-digit hex colour. */
function channels(hex: string | undefined): number[] {
  const digits = (hex ?? '').replace(/^#/, '');
  return [0, 2, 4].map((offset) => Number.parseInt(digits.slice(offset, offset + 2), 16));
}

/** The ancestor of `label` that sits as far above it as `row` sits above `reference`. */
function rowAtSameDepth(label: HTMLElement, reference: HTMLElement, row: HTMLElement): HTMLElement {
  let depth = 0;
  for (let node: HTMLElement | null = reference; node && node !== row; node = node.parentElement) depth += 1;
  let node: HTMLElement = label;
  for (let step = 0; step < depth && node.parentElement; step += 1) node = node.parentElement;
  return node;
}

/** The CSS text emotion injected for `element`'s classes. */
function cssFor(element: HTMLElement): string {
  const css = Array.from(document.querySelectorAll('style'), (style) => style.textContent ?? '').join('\n');
  return Array.from(element.classList)
    .flatMap((name) => css.split('}').filter((rule) => rule.includes(`.${name}`)))
    .join('}');
}

describe('Artifacts dividers (#6687)', () => {
  it('the default pack states the design colours for border.artifactDivider', () => {
    // Gray40 (dark) and Light40 (light) in the design, as RGB channels.
    expect(channels(DEFAULT_BRAND_PACK.schemes.dark['border.artifactDivider'])).toEqual([38, 43, 52]);
    expect(channels(DEFAULT_BRAND_PACK.schemes.light['border.artifactDivider'])).toEqual([225, 229, 233]);
  });

  it.each(DIVIDER_FILES)('%s draws its dividers with border.artifactDivider', (file) => {
    const source = readFileSync(resolve(SRC, file), 'utf8');
    expect(source).toContain('palette.border.artifactDivider');
    expect(source).not.toMatch(/border\.(lines|table|conversationItemDivider)\}`/);
  });

  it('a selected bucket row keeps a 1px transparent rule, and the other rows use the divider token', () => {
    render(
      <ThemeProvider
        theme={theme}
        defaultMode={DEFAULT_COLOR_SCHEME}
      >
        <BucketList
          buckets={[bucket('selected-bucket'), bucket('other-bucket')]}
          selectedBucket="selected-bucket"
          tree={[]}
          expandedPaths={[]}
          onSelect={vi.fn()}
          onEdit={vi.fn()}
          onManageAccess={vi.fn()}
          onPin={vi.fn()}
          onDelete={vi.fn()}
          onSelectFile={vi.fn()}
          onSelectFolder={vi.fn()}
          isTeamProject
        />
      </ThemeProvider>,
    );
    const selectedRow = ruledAncestor(screen.getByText('selected-bucket'));
    const selected = getComputedStyle(selectedRow);
    // jsdom reports `transparent` as its computed value.
    expect(selected.borderBottomColor).toMatch(/^rgba\(0, 0, 0, 0\)$/);
    expect(selected.borderBottomWidth).toBe('0.0625rem');

    // jsdom cannot compute a colour from a CSS variable, so read the rule the
    // other row's class carries instead.
    const otherRow = rowAtSameDepth(screen.getByText('other-bucket'), screen.getByText('selected-bucket'), selectedRow);
    expect(cssFor(otherRow)).toMatch(/border-bottom:0\.0625rem solid var\(--[^)]*artifactDivider\)/);
  });
});
