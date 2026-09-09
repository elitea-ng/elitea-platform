/**
 * Content structural checks (PREAMBLE decision 3/7). Deliberately does NOT
 * compile `.mdx` through `@mdx-js/rollup` — `vitest.config.ts` (spec §6.3,
 * kept verbatim outside its own marked `[F2]` deviations) registers no MDX
 * plugin, and adding one there to satisfy this one test file would change a
 * config every other test in the app also runs under. Every check below is
 * therefore a REGEX/structural read of the raw `.mdx` source, the same
 * class of check the PREAMBLE allows outright for mermaid ("if mermaid can't
 * run in vitest, do a structural check and say so") applied to the whole
 * file. `nav.ts`, `shots.manifest.ts` and `components/index.ts` are real
 * imports (plain `.ts`/`.tsx`, no MDX involved) — only the content files
 * themselves are read as text.
 */
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

import GithubSlugger from 'github-slugger';
import { describe, expect, it } from 'vitest';

import { docsComponents } from './components';
import { flattenNav } from './nav';
import { shots } from './shots.manifest';

const DOCS_DIR = dirname(fileURLToPath(import.meta.url));
const CONTENT_DIR = join(DOCS_DIR, 'content');
const IMG_DIR = join(CONTENT_DIR, 'img');
const MAX_IMAGE_BYTES = 250 * 1024;

function walkMdxFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir)) {
    if (entry === 'img') continue;
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      out.push(...walkMdxFiles(full));
    } else if (entry.endsWith('.mdx')) {
      out.push(full);
    }
  }
  return out;
}

/** Must agree with `content-registry.ts`'s `slugFromPath` — that module
 * builds the same slug from the same file, just through `import.meta.glob`
 * instead of `fs`, for the reason this file's header gives. */
function slugFor(filePath: string): string {
  const rel = relative(CONTENT_DIR, filePath).replace(/\\/g, '/').replace(/\.mdx$/, '');
  return rel === 'index' ? '' : rel;
}

const FRONTMATTER_RE = /^---\n([\s\S]*?)\n---\n?/;

interface Frontmatter {
  readonly title: string | undefined;
  readonly description: string | undefined;
}

function parseFrontmatter(source: string): { frontmatter: Frontmatter; body: string } {
  const match = FRONTMATTER_RE.exec(source);
  if (match === null) return { frontmatter: { title: undefined, description: undefined }, body: source };
  const block = match[1] ?? '';
  const readField = (name: string): string | undefined => {
    const fieldMatch = new RegExp(`^${name}:\\s*(.+)$`, 'm').exec(block);
    return fieldMatch?.[1]?.trim().replace(/^["']|["']$/g, '');
  };
  return {
    frontmatter: { title: readField('title'), description: readField('description') },
    body: source.slice(match[0].length),
  };
}

/** Removes fenced code blocks (` ``` `) before any regex below scans for
 * JSX tags or links, so an example `<Card>` or `[link](url)` shown as
 * documentation TEXT inside a code fence is never mistaken for real content
 * markup. */
function withoutCodeFences(body: string): string {
  return body.replace(/```[\s\S]*?```/g, '');
}

/** Strips the inline markdown/JSX decoration off a heading line so what is
 * left approximates the plain text `rehype-slug` actually slugs (it slugs
 * the heading's rendered TEXT, via `hast-util-to-string`, not its markdown
 * source). */
function headingPlainText(raw: string): string {
  return raw
    .replace(/<[^>]+>/g, '')
    .replace(/`([^`]+)`/g, '$1')
    .replace(/\*\*([^*]+)\*\*/g, '$1')
    .replace(/\*([^*]+)\*/g, '$1')
    .replace(/\[([^\]]+)\]\([^)]*\)/g, '$1')
    .trim();
}

interface ParsedPage {
  readonly slug: string;
  readonly relPath: string;
  readonly frontmatter: Frontmatter;
  readonly headingIds: ReadonlySet<string>;
  readonly h1Lines: readonly string[];
  readonly internalLinks: ReadonlyArray<{ target: string; anchor: string | undefined }>;
  readonly screenshotIds: readonly string[];
  readonly mermaidBlocks: readonly string[];
  readonly jsxComponentNames: ReadonlySet<string>;
}

function extractHeadingIds(body: string): Set<string> {
  const slugger = new GithubSlugger();
  const headingIds = new Set<string>();
  for (const match of body.matchAll(/^#{1,6}\s+(.+)$/gm)) {
    const text = headingPlainText(match[1] ?? '');
    if (text !== '') headingIds.add(slugger.slug(text));
  }
  return headingIds;
}

function extractInternalLinks(prose: string): Array<{ target: string; anchor: string | undefined }> {
  const internalLinks: Array<{ target: string; anchor: string | undefined }> = [];
  for (const match of prose.matchAll(/\]\((\/[^)\s]*)\)/g)) {
    const href = match[1] ?? '';
    const [target, anchor] = href.slice(1).split('#');
    internalLinks.push({ target: target ?? '', anchor });
  }
  return internalLinks;
}

function extractScreenshotIds(prose: string): string[] {
  const screenshotIds: string[] = [];
  for (const match of prose.matchAll(/<Screenshot\s+[^>]*\bid=["']([^"']+)["']/g)) {
    if (match[1] !== undefined) screenshotIds.push(match[1]);
  }
  return screenshotIds;
}

/** `# ` (level-1) headings only — MDX content must not declare its own H1:
 * `App.tsx` renders one, from `frontmatter.title`, above every page's
 * compiled content, so a page's own `# ` would produce two H1s on the same
 * page. Content pages are expected to start at `## ` (H2). */
function extractH1Lines(body: string): string[] {
  const h1Lines: string[] = [];
  for (const match of body.matchAll(/^#(?!#)\s+(.+)$/gm)) {
    if (match[1] !== undefined) h1Lines.push(match[1]);
  }
  return h1Lines;
}

function extractMermaidBlocks(body: string): string[] {
  const mermaidBlocks: string[] = [];
  for (const match of body.matchAll(/```mermaid\n([\s\S]*?)```/g)) {
    if (match[1] !== undefined) mermaidBlocks.push(match[1]);
  }
  return mermaidBlocks;
}

function extractJsxComponentNames(prose: string): Set<string> {
  const jsxComponentNames = new Set<string>();
  for (const match of prose.matchAll(/<\/?([A-Z][A-Za-z0-9]*)\b/g)) {
    if (match[1] !== undefined) jsxComponentNames.add(match[1]);
  }
  return jsxComponentNames;
}

function parsePage(filePath: string): ParsedPage {
  const source = readFileSync(filePath, 'utf8');
  const { frontmatter, body } = parseFrontmatter(source);
  const prose = withoutCodeFences(body);

  return {
    slug: slugFor(filePath),
    relPath: relative(CONTENT_DIR, filePath),
    frontmatter,
    headingIds: extractHeadingIds(body),
    h1Lines: extractH1Lines(body),
    internalLinks: extractInternalLinks(prose),
    screenshotIds: extractScreenshotIds(prose),
    mermaidBlocks: extractMermaidBlocks(body),
    jsxComponentNames: extractJsxComponentNames(prose),
  };
}

const pages = walkMdxFiles(CONTENT_DIR).map(parsePage);

describe('docs content', () => {
  it('has at least one page to check (floor on the walk itself)', () => {
    expect(pages.length).toBeGreaterThan(0);
  });

  it('nav and content files are a bijection', () => {
    const navSlugs = flattenNav().map((page) => page.slug);
    const fileSlugs = pages.map((page) => page.slug);

    expect(new Set(navSlugs).size, 'nav.ts declares the same slug twice').toBe(navSlugs.length);
    expect(new Set(fileSlugs).size, 'two content files resolve to the same slug').toBe(fileSlugs.length);

    const missingFiles = navSlugs.filter((slug) => !fileSlugs.includes(slug));
    const orphanFiles = fileSlugs.filter((slug) => !navSlugs.includes(slug));
    expect(missingFiles, 'nav.ts slugs with no content/*.mdx file').toEqual([]);
    expect(orphanFiles, 'content/*.mdx files not reachable from nav.ts').toEqual([]);
  });

  it('every internal link resolves to a nav slug, and every anchor to a real heading', () => {
    const navSlugSet = new Set(flattenNav().map((page) => page.slug));
    const bySlug = new Map(pages.map((page) => [page.slug, page]));
    const problems: string[] = [];

    for (const page of pages) {
      for (const link of page.internalLinks) {
        if (!navSlugSet.has(link.target)) {
          problems.push(`${page.relPath}: link to "/${link.target}" has no matching nav slug`);
          continue;
        }
        if (link.anchor !== undefined) {
          const targetPage = bySlug.get(link.target);
          if (targetPage === undefined || !targetPage.headingIds.has(link.anchor)) {
            problems.push(`${page.relPath}: link to "/${link.target}#${link.anchor}" has no matching heading`);
          }
        }
      }
    }
    expect(problems).toEqual([]);
  });

  it('every <Screenshot id> has a shots.manifest.ts entry, and a committed webp is ≤250 KB', () => {
    const manifestIds = new Set(shots.map((shot) => shot.id));
    const problems: string[] = [];

    for (const page of pages) {
      for (const id of page.screenshotIds) {
        if (!manifestIds.has(id)) {
          problems.push(`${page.relPath}: <Screenshot id="${id}"> has no shots.manifest.ts entry`);
          continue;
        }
        const imagePath = join(IMG_DIR, `${id}.webp`);
        if (existsSync(imagePath)) {
          const size = statSync(imagePath).size;
          if (size > MAX_IMAGE_BYTES) {
            problems.push(`${page.relPath}: content/img/${id}.webp is ${size} bytes, over the ${MAX_IMAGE_BYTES}-byte ceiling`);
          }
        }
      }
    }
    expect(problems).toEqual([]);
  });

  it('every committed content/img/*.webp is referenced by some <Screenshot id>', () => {
    if (!existsSync(IMG_DIR)) return;
    const referencedIds = new Set(pages.flatMap((page) => page.screenshotIds));
    const files = readdirSync(IMG_DIR).filter((file) => file.endsWith('.webp'));
    const unreferenced = files.filter((file) => !referencedIds.has(file.replace(/\.webp$/, '')));
    expect(unreferenced, 'unreferenced committed screenshot(s)').toEqual([]);
  });

  it('no page declares its own H1 — App.tsx renders one from frontmatter.title', () => {
    const problems = pages
      .filter((page) => page.h1Lines.length > 0)
      .map((page) => `${page.relPath}: "# ${page.h1Lines[0]}"`);
    expect(problems).toEqual([]);
  });

  it('every page declares frontmatter title and description', () => {
    const problems = pages
      .filter((page) => !page.frontmatter.title || !page.frontmatter.description)
      .map((page) => page.relPath);
    expect(problems).toEqual([]);
  });

  it('every JSX component used in content is part of the documented contract', () => {
    const known = new Set(Object.keys(docsComponents));
    const problems: string[] = [];
    for (const page of pages) {
      for (const name of page.jsxComponentNames) {
        if (!known.has(name)) problems.push(`${page.relPath}: <${name}> is not in components/index.ts's docsComponents map`);
      }
    }
    expect(problems).toEqual([]);
  });

  it('every ```mermaid block is syntactically valid', async () => {
    const allBlocks = pages.flatMap((page) => page.mermaidBlocks.map((chart) => ({ page: page.relPath, chart })));
    if (allBlocks.length === 0) return;

    let mermaidParse: ((chart: string) => Promise<unknown>) | undefined;
    try {
      const mermaidModule = await import('mermaid');
      mermaidModule.default.initialize({ startOnLoad: false });
      mermaidParse = (chart: string) => mermaidModule.default.parse(chart);
    } catch (importError) {
      // mermaid needs a browser-shaped environment to even initialize in
      // some sandboxes; jsdom (this project's `node` vitest project) usually
      // provides enough of one, but if it does not, fall back to a
      // structural check rather than silently skipping the assertion.
      console.warn(`docs-content.test.ts: mermaid unavailable in this test environment (${String(importError)}); falling back to a structural check.`);
    }

    const problems: string[] = [];
    for (const { page, chart } of allBlocks) {
      if (mermaidParse !== undefined) {
        try {
          await mermaidParse(chart);
          continue;
        } catch (parseError) {
          problems.push(`${page}: mermaid.parse failed — ${String(parseError)}`);
          continue;
        }
      }
      // Structural fallback: every mermaid diagram type keyword recognized
      // by mermaid 11's own grammar dispatch.
      const firstLine = chart.trim().split('\n')[0]?.trim() ?? '';
      const knownStart = /^(flowchart|graph|sequenceDiagram|classDiagram|stateDiagram|stateDiagram-v2|erDiagram|journey|gantt|pie|quadrantChart|requirementDiagram|gitGraph|mindmap|timeline|sankey|block-beta|xychart-beta|C4Context)\b/;
      if (!knownStart.test(firstLine)) {
        problems.push(`${page}: mermaid block does not start with a recognized diagram type ("${firstLine}")`);
      }
    }
    expect(problems).toEqual([]);
  });
});
