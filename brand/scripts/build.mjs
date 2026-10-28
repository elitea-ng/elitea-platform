#!/usr/bin/env node
/**
 * Builds the Elitea AI brand guide: a static site for people and agents.
 *
 *   node brand/scripts/build.mjs [--out <dir>]     default brand/dist
 *
 * Sources (nothing else is read):
 *   brand/BRAND.md                                  the prose: the one place a rule is written
 *   libs/ts/client-core/src/brand/tokens.json       every value (colour, type, shape, contrast pairs)
 *   libs/ts/client-core/src/brand/logo-*.svg        the mark and the lockup
 *   apps/elitea-web/public/brand/fonts/*.woff2      the self-hosted typefaces
 *   brand/template.html, brand/styles.css, brand/brand.js, brand/assets/
 *
 * Written: index.html (BRAND.md rendered, with the visual blocks), BRAND.md
 * (placeholders expanded), llms.txt, tokens.css, tokens.json (W3C design
 * tokens), brand.json (a manifest of every file), assets/, fonts/.
 *
 * Every URL the site emits is relative, so the same build works at
 * https://elitea-ng.github.io/elitea-platform/brand/, under any other path
 * and from the file system. Zero dependencies: Node built-ins only.
 */
import { cpSync, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const BRAND_DIR = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const REPO_DIR = resolve(BRAND_DIR, '..');
const CORE_BRAND = join(REPO_DIR, 'libs/ts/client-core/src/brand');
const FONTS_DIR = join(REPO_DIR, 'apps/elitea-web/public/brand/fonts');
const SITE_URL = 'https://elitea-ng.github.io/elitea-platform/brand/';

// ---------------------------------------------------------------- colour maths

/** WCAG 2.2 relative luminance of an opaque `#rrggbb`. */
export function luminance(hex) {
  const c = [1, 3, 5].map((i) => Number.parseInt(hex.slice(i, i + 2), 16) / 255);
  const [r, g, b] = c.map((v) => (v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4));
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

export function contrast(a, b) {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

const verdict = (ratio, min) => {
  if (ratio < min) return 'FAIL';
  if (min < 4.5) return 'Non-text';
  return ratio >= 7 ? 'AAA' : 'AA';
};

// ---------------------------------------------------------------- tokens

const kebab = (name) => name.replace(/[A-Z]/g, (c) => `-${c.toLowerCase()}`);

/** `tokens.css`: `--elitea-*` custom properties, light by default, dark by attribute or system preference. */
export function tokensCss(t) {
  const decl = (obj, indent = '  ') => Object.entries(obj).map(([k, v]) => `${indent}--elitea-${kebab(k)}: ${v};`).join('\n');
  const brand = Object.fromEntries(Object.entries(t.palette).map(([k, v]) => [k, v.value]));
  const shared = {
    ...brand,
    fontDisplay: t.typography.display.stack,
    fontUi: t.typography.ui.stack,
    fontMono: t.typography.mono.stack,
    radiusSm: `${t.shape.radiusSm}px`,
    radiusMd: `${t.shape.radiusMd}px`,
    radiusLg: `${t.shape.radiusLg}px`,
    radiusPill: `${t.shape.radiusPill}px`,
    focusWidth: t.focus.width,
    focusOffset: t.focus.offset,
    motionFast: t.motion.fast,
    motionBase: t.motion.base,
    motionEasing: t.motion.easing,
  };
  return `/* Elitea AI design tokens v${t.version}. Generated from tokens.json; ${SITE_URL} */
:root {
${decl(shared)}
}
:root,
[data-theme="light"] {
  color-scheme: light;
${decl(t.schemes.light)}
  --elitea-shadow: ${t.elevation.light};
}
[data-theme="dark"] {
  color-scheme: dark;
${decl(t.schemes.dark)}
  --elitea-shadow: none;
}
@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) {
    color-scheme: dark;
${decl(t.schemes.dark, '    ')}
    --elitea-shadow: none;
  }
}
`;
}

/** `tokens.json`: the W3C Design Tokens Community Group format. */
export function tokensDtcg(t) {
  const colour = (value, description) => ({ $type: 'color', $value: value, ...(description ? { $description: description } : {}) });
  const scheme = (s) => Object.fromEntries(Object.entries(s).map(([k, v]) => [k, colour(v)]));
  return {
    $description: `Elitea AI design tokens v${t.version}. ${SITE_URL}`,
    palette: Object.fromEntries(Object.entries(t.palette).map(([k, v]) => [k, colour(v.value, `${v.name}. ${v.job}`)])),
    light: scheme(t.schemes.light),
    dark: scheme(t.schemes.dark),
    font: Object.fromEntries(
      ['display', 'ui', 'mono'].map((k) => [k, { $type: 'fontFamily', $value: t.typography[k].stack, $description: `${t.typography[k].family}, weights ${t.typography[k].weights.join(' and ')}` }]),
    ),
    radius: Object.fromEntries(Object.entries(t.shape).map(([k, v]) => [k, { $type: 'dimension', $value: `${v}px` }])),
    chart: { light: t.chart.light.map((v) => colour(v)), dark: t.chart.dark.map((v) => colour(v)), $description: t.chart.rule },
    focus: { $type: 'border', $value: { width: t.focus.width, style: 'solid', color: '{light.signal}' }, $description: `Offset ${t.focus.offset}; colour is the signal token of the scheme.` },
    duration: { fast: { $type: 'duration', $value: t.motion.fast }, base: { $type: 'duration', $value: t.motion.base } },
    easing: { $type: 'cubicBezier', $value: t.motion.easing.match(/[\d.]+/g).map(Number) },
  };
}

// ---------------------------------------------------------------- markdown placeholders

const row = (cells) => `| ${cells.join(' | ')} |`;
const table = (head, rows) => [row(head), row(head.map(() => '---')), ...rows.map(row)].join('\n');

/** Expands `{{…}}` in BRAND.md from the tokens. An unknown placeholder fails the build. */
export function expand(markdown, t, css) {
  const pairs = t.contrast.flatMap((p) => p.schemes.map((s) => ({ ...p, scheme: s, fgv: t.schemes[s][p.fg], bgv: t.schemes[s][p.bg] })));
  const values = {
    version: t.version,
    shape: `${t.shape.radiusSm} / ${t.shape.radiusMd} / ${t.shape.radiusLg} px`,
    space: t.space.steps.join(', '),
    focus: `${t.focus.width} \`signal\`, ${t.focus.offset} offset`,
    motion: `${t.motion.fast} / ${t.motion.base}, \`${t.motion.easing}\``,
    elevationLight: `\`${t.elevation.light}\``,
    'table:palette': table(
      ['Colour', 'Hex', 'Job'],
      Object.values(t.palette).map((p) => [`**${p.name}**`, `\`${p.value}\``, p.job]),
    ),
    'table:banned': table(
      ['Pair', 'Ratio', 'Why'],
      t.bannedPairs.map((p) => [`\`${p.fg}\` on \`${p.bg}\``, `${contrast(p.fg, p.bg).toFixed(2)}:1`, p.why]),
    ),
    'table:kill': table(['Retired', 'Values'], t.killList.map((k) => [k.name, k.values.map((v) => `\`${v}\``).join(' ')])),
    'table:typescale': table(
      ['Role', 'Family', 'Weight', 'Size', 'Tracking', 'Line height'],
      Object.entries(t.typography.scale).map(([role, s]) => [
        role[0].toUpperCase() + role.slice(1),
        t.typography[s.family].family + (s.case ? `, ${s.case}` : ''),
        String(s.weight),
        s.size,
        s.tracking,
        String(s.lineHeight),
      ]),
    ),
    'table:schemes': table(
      ['Token', 'Dark "Ink"', 'Light "Paper"'],
      Object.keys(t.schemes.dark).map((k) => [`\`${k}\``, `\`${t.schemes.dark[k]}\``, `\`${t.schemes.light[k]}\``]),
    ),
    'table:contrast': table(
      ['Theme', 'Pair', 'Ratio', 'Minimum', 'Verdict'],
      pairs.map((p) => {
        const ratio = contrast(p.fgv, p.bgv);
        return [p.scheme === 'dark' ? 'Dark' : 'Light', `\`${p.fg}\` ${p.fgv} on \`${p.bg}\` ${p.bgv}`, `${ratio.toFixed(2)}:1`, `${p.min}:1`, verdict(ratio, p.min)];
      }),
    ),
    'code:tokens.css': css.trimEnd(),
  };
  const failing = pairs.filter((p) => contrast(p.fgv, p.bgv) < p.min);
  if (failing.length > 0) throw new Error(`contrast below minimum: ${failing.map((p) => `${p.scheme} ${p.fg}/${p.bg}`).join(', ')}`);
  return markdown.replace(/\{\{([\w:.]+)\}\}/g, (_, key) => {
    if (!(key in values)) throw new Error(`BRAND.md: unknown placeholder {{${key}}}`);
    return values[key];
  });
}

// ---------------------------------------------------------------- markdown → HTML (the subset BRAND.md uses)

const escapeHtml = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');

function inline(text) {
  const code = [];
  let s = text.replace(/`([^`]+)`/g, (_, c) => `\u0000${code.push(c) - 1}\u0000`);
  s = escapeHtml(s)
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (_, label, href) => `<a href="${href}">${label}</a>`)
    .replace(/(^|\s)(#[0-9A-Fa-f]{6})\b/g, '$1<span class="hex" style="--swatch:$2">$2</span>');
  return s.replace(/\u0000(\d+)\u0000/g, (_, i) => {
    const c = code[Number(i)];
    return /^#[0-9A-Fa-f]{6}$/.test(c) ? `<code class="hex" style="--swatch:${c}">${escapeHtml(c)}</code>` : `<code>${escapeHtml(c)}</code>`;
  });
}

/** Renders headings, paragraphs, lists (- and 1.), tables and fenced code. */
export function renderMarkdown(md) {
  const lines = md.split('\n');
  const out = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (line.startsWith('```')) {
      const body = [];
      for (i++; i < lines.length && !lines[i].startsWith('```'); i++) body.push(lines[i]);
      i++;
      out.push(`<pre class="code"><code>${escapeHtml(body.join('\n'))}</code></pre>`);
    } else if (/^#{3,4} /.test(line)) {
      const level = line.indexOf(' ');
      out.push(`<h${level}>${inline(line.slice(level + 1))}</h${level}>`);
      i++;
    } else if (line.startsWith('|')) {
      const rows = [];
      for (; i < lines.length && lines[i].startsWith('|'); i++) rows.push(lines[i].slice(1, -1).split(' | ').map((c) => c.trim()));
      const [head, , ...body] = rows;
      out.push(
        `<div class="table-wrap"><table><thead><tr>${head.map((c) => `<th>${inline(c)}</th>`).join('')}</tr></thead><tbody>${body
          .map((r) => `<tr>${r.map((c) => `<td${/^(FAIL)$/.test(c) ? ' class="fail"' : ''}>${inline(c)}</td>`).join('')}</tr>`)
          .join('')}</tbody></table></div>`,
      );
    } else if (/^(- |\d+\. )/.test(line)) {
      const ordered = /^\d/.test(line);
      const items = [];
      for (; i < lines.length && /^(- |\d+\. )/.test(lines[i]); i++) items.push(lines[i].replace(/^(- |\d+\. )/, ''));
      const tag = ordered ? 'ol' : 'ul';
      out.push(`<${tag} class="rules">${items.map((it) => `<li>${inline(it)}</li>`).join('')}</${tag}>`);
    } else if (line.trim() === '') {
      i++;
    } else {
      const para = [];
      for (; i < lines.length && lines[i].trim() !== '' && !/^(#|\||- |\d+\. |```)/.test(lines[i]); i++) para.push(lines[i]);
      out.push(`<p>${inline(para.join(' '))}</p>`);
    }
  }
  return out.join('\n');
}

/** Splits BRAND.md into the title block and `## NN Title` sections. */
export function sections(md) {
  const parts = md.split(/^## /m);
  const [head, ...rest] = parts;
  return {
    head,
    sections: rest.map((part) => {
      const nl = part.indexOf('\n');
      const [num, ...title] = part.slice(0, nl).split(' ');
      const body = part.slice(nl + 1).trim();
      const firstBreak = body.search(/\n\s*\n/);
      const intro = /^[-|#`\d]/.test(body) ? '' : firstBreak === -1 ? body : body.slice(0, firstBreak);
      return { num, title: title.join(' '), id: title.join('-').toLowerCase().replace(/[^a-z0-9-]/g, ''), intro, rest: intro ? body.slice(intro.length).trim() : body };
    }),
  };
}

// ---------------------------------------------------------------- the visual blocks

const markInner = (mark) => mark.replace(/^<svg[^>]*>/, '').replace(/<\/svg>$/, '');
const monoMark = (mark, colour) => markInner(mark).replace(/fill="#[0-9A-F]{6}"/g, `fill="${colour}"`);

function visuals(t, mark, lockup) {
  const inner = markInner(mark);
  const svg = (body, cls = '', vb = '0 0 372 372') => `<svg class="${cls}" viewBox="${vb}" aria-hidden="true">${body}</svg>`;
  const lockupBody = lockup.replace(/^<svg[^>]*>/, '').replace(/<\/svg>$/, '').replace(/<title>.*?<\/title>/, '');
  const lockupVb = /viewBox="([^"]+)"/.exec(lockup)[1];
  const swatch = (p) =>
    `<button type="button" class="swatch" data-copy="${p.value}" style="--swatch:${p.value}"><span class="chip"></span><span class="label"><strong>${p.name}</strong><code>${p.value}</code></span><span class="job">${p.job}</span></button>`;
  const mini = (scheme) => `
    <div class="mini" data-theme="${scheme}">
      <aside class="mini-nav">
        <div class="mini-brand">${svg(inner, 'mini-mark')}<span>Elitea</span></div>
        <button type="button" class="mini-create">+ Create</button>
        <a class="active">Chats</a><a>Agents</a><a>Pipelines</a><a>Toolkits</a><a>Credentials</a>
      </aside>
      <div class="mini-main">
        <div class="mini-top"><span class="mini-model"><i></i>Model · gpt-5</span><span class="mini-bell" aria-hidden="true"><i></i></span></div>
        <div class="mini-hello">${svg(inner, 'mini-tile')}<strong>Hello, Sam.</strong><span>What can I do for you today?</span></div>
        <div class="mini-input"><span>Message Elitea…</span><button type="button" aria-label="Send">↑</button></div>
        <div class="mini-row"><a href="#tokens">Open the toolkit catalogue</a><span class="mini-ok">Saved</span><span class="mini-warn">Token expires in 3 days</span></div>
      </div>
    </div>`;
  return {
    name: `<figure class="lockup-row">
      <div class="tile paper">${svg(lockupBody, 'lockup', lockupVb)}</div>
      <div class="tile ink">${svg(lockupBody, 'lockup', lockupVb)}</div>
    </figure>`,
    logo: `<div class="logo-grid">
      <figure class="tile ink">${svg(inner, 'mark')}<figcaption>Coloured on Ink, default</figcaption></figure>
      <figure class="tile paper">${svg(inner, 'mark')}<figcaption>Coloured on Paper</figcaption></figure>
      <figure class="tile white">${svg(monoMark(mark, t.palette.ink.value), 'mark')}<figcaption>One-colour Ink</figcaption></figure>
      <figure class="tile ink">${svg(monoMark(mark, '#FFFFFF'), 'mark')}<figcaption>One-colour white</figcaption></figure>
    </div>
    <div class="logo-grid misuse">
      <figure class="tile paper">${svg(markInner(mark).replace('#00BFE6', '#FF6B1A').replace('#4A4DE8', '#22C55E'), 'mark')}<figcaption>✕ Recolour the layers</figcaption></figure>
      <figure class="tile paper">${svg(inner, 'mark squash')}<figcaption>✕ Stretch or condense</figcaption></figure>
      <figure class="tile paper"><svg class="mark" viewBox="0 0 372 372" aria-hidden="true"><defs><linearGradient id="g" x1="0" x2="1"><stop stop-color="#F767CF"/><stop offset="1" stop-color="#24E1FA"/></linearGradient></defs>${monoMark(mark, 'url(#g)')}</svg><figcaption>✕ Gradient the mark</figcaption></figure>
      <figure class="tile" style="background:#00BFE6">${svg(inner, 'mark')}<figcaption>✕ Sit it on a brand colour</figcaption></figure>
    </div>
    <figure class="clearspace tile paper"><div class="cs">${svg(inner, 'mark')}</div><figcaption>Clear space x = one layer's height (104 of 372 units).</figcaption></figure>`,
    colour: `<div class="swatches">${Object.values(t.palette).map(swatch).join('')}</div>
    <div class="bar" role="img" aria-label="Proportion: neutrals 78%, indigo 12%, cyan 5%, violet 3%, orange 2%"><span style="flex:78" class="n"></span><span style="flex:12;background:#4A4DE8"></span><span style="flex:5;background:#00BFE6"></span><span style="flex:3;background:#7A5CFF"></span><span style="flex:2;background:#FF6B1A"></span></div>
    <div class="theme-pair">${mini('dark')}${mini('light')}</div>`,
    type: `<div class="specimen">
      <p class="eyebrow">Elitea AI · type specimen</p>
      <p class="spec-display">Agents that do the work.<br/>Data that stays yours.</p>
      <p class="spec-body">Create and edit agents and pipelines from the chat. Connect a toolkit, give it credentials, and Elitea keeps everything inside your project.</p>
      <p class="spec-mono">project_user_1024 · https://elitea.example/llm/v1 · #4A4DE8</p>
    </div>`,
  };
}

// ---------------------------------------------------------------- assemble

function page(template, t, md, mark, lockup) {
  const { head, sections: secs } = sections(md);
  const v = visuals(t, mark, lockup);
  const lede = head.split('\n').filter((l) => l && !l.startsWith('#'));
  const nav = secs.map((s) => `<a href="#${s.id}">${escapeHtml(s.title)}</a>`).join('');
  const body = secs
    .map(
      (s) => `<section id="${s.id}" aria-labelledby="${s.id}-h">
  <header class="sec-head"><div><p class="num">${s.num}</p><h2 id="${s.id}-h">${escapeHtml(s.title)}</h2></div>${s.intro ? `<p class="intro">${inline(s.intro.replace(/\n/g, ' '))}</p>` : ''}</header>
  ${v[s.id] ?? ''}
  <div class="prose">${renderMarkdown(s.rest)}</div>
</section>`,
    )
    .join('\n');
  const filled = {
    version: t.version,
    lede: lede.map((p) => `<p class="lede">${inline(p)}</p>`).join(''),
    nav,
    sections: body,
    mark: `<svg class="top-mark" viewBox="0 0 372 372" aria-hidden="true">${markInner(mark)}</svg>`,
  };
  return template.replace(/\{\{(\w+)\}\}/g, (_, k) => {
    if (!(k in filled)) throw new Error(`template.html: unknown placeholder {{${k}}}`);
    return filled[k];
  });
}

function listFiles(dir, base = dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => (e.isDirectory() ? listFiles(join(dir, e.name), base) : [relative(base, join(dir, e.name))]));
}

export function build(outDir) {
  const t = JSON.parse(readFileSync(join(CORE_BRAND, 'tokens.json'), 'utf8'));
  const mark = readFileSync(join(CORE_BRAND, 'logo-mark.svg'), 'utf8').trim();
  const lockup = readFileSync(join(CORE_BRAND, 'logo-full.svg'), 'utf8').trim();
  const css = tokensCss(t);
  const md = expand(readFileSync(join(BRAND_DIR, 'BRAND.md'), 'utf8'), t, css);

  rmSync(outDir, { recursive: true, force: true });
  mkdirSync(join(outDir, 'assets'), { recursive: true });
  cpSync(join(BRAND_DIR, 'assets'), join(outDir, 'assets'), { recursive: true });
  cpSync(FONTS_DIR, join(outDir, 'fonts'), { recursive: true });

  const xmlns = 'xmlns="http://www.w3.org/2000/svg"';
  const inner = markInner(mark);
  const svgFiles = {
    'assets/elitea-mark.svg': `<svg ${xmlns} viewBox="0 0 372 372">${inner}</svg>`,
    'assets/elitea-mark-ink.svg': `<svg ${xmlns} viewBox="0 0 372 372">${monoMark(mark, t.palette.ink.value)}</svg>`,
    'assets/elitea-mark-white.svg': `<svg ${xmlns} viewBox="0 0 372 372">${monoMark(mark, '#FFFFFF')}</svg>`,
    'assets/elitea-app-icon.svg': `<svg ${xmlns} viewBox="0 0 512 512"><rect width="512" height="512" rx="114" fill="${t.palette.ink.value}"/><g transform="translate(70 70)">${inner}</g></svg>`,
    'assets/elitea-lockup-light.svg': lockup.replace('fill="currentColor"', `fill="${t.schemes.light.text}"`),
    'assets/elitea-lockup-dark.svg': lockup.replace('fill="currentColor"', `fill="${t.schemes.dark.text}"`),
  };
  for (const [path, content] of Object.entries(svgFiles)) writeFileSync(join(outDir, path), `${content}\n`);

  writeFileSync(join(outDir, 'tokens.css'), css);
  writeFileSync(join(outDir, 'tokens.json'), `${JSON.stringify(tokensDtcg(t), null, 2)}\n`);
  writeFileSync(join(outDir, 'BRAND.md'), md);
  writeFileSync(join(outDir, 'styles.css'), readFileSync(join(BRAND_DIR, 'styles.css'), 'utf8'));
  writeFileSync(join(outDir, 'brand.js'), readFileSync(join(BRAND_DIR, 'brand.js'), 'utf8'));
  writeFileSync(join(outDir, 'index.html'), page(readFileSync(join(BRAND_DIR, 'template.html'), 'utf8'), t, md, mark, lockup));
  writeFileSync(
    join(outDir, 'llms.txt'),
    `# Elitea AI brand guide

> The identity of Elitea AI: name, LE-03 mark, colour, type, voice and design tokens (v${t.version}). Read BRAND.md before producing anything that carries the brand; take every value from tokens.json or tokens.css.

## Guide

- [BRAND.md](BRAND.md): every rule, as Markdown. Sections: name, logo, colour, type, voice, tokens, accessibility, guardrails, for agents, changes from v2, downloads.
- [Brand guide (HTML)](index.html): the same guide for people, with swatches and a themed preview.

## Tokens

- [tokens.json](tokens.json): W3C design-token format; palette, light and dark schemes, fonts, radii, chart order, focus, motion.
- [tokens.css](tokens.css): \`--elitea-*\` custom properties; light by default, dark under [data-theme="dark"] or the system preference.

## Assets

${Object.keys(svgFiles)
  .concat(listFiles(join(BRAND_DIR, 'assets')).map((f) => `assets/${f}`))
  .map((f) => `- [${f.split('/').pop()}](${f})`)
  .join('\n')}

## Fonts (SIL OFL 1.1)

${readdirSync(FONTS_DIR)
  .filter((f) => f.endsWith('.woff2'))
  .map((f) => `- [${f}](fonts/${f})`)
  .join('\n')}
`,
  );
  const files = listFiles(outDir).filter((f) => f !== 'brand.json').sort();
  writeFileSync(join(outDir, 'brand.json'), `${JSON.stringify({ name: t.name, version: t.version, url: SITE_URL, guide: 'BRAND.md', tokens: 'tokens.json', files }, null, 2)}\n`);
  return { files: [...files, 'brand.json'] };
}

function main(argv) {
  const at = argv.indexOf('--out');
  const out = at === -1 ? join(BRAND_DIR, 'dist') : resolve(argv[at + 1] ?? '');
  if (at !== -1 && !argv[at + 1]) throw new Error('--out needs a directory');
  if (!existsSync(FONTS_DIR)) throw new Error(`fonts missing: ${FONTS_DIR}`);
  const { files } = build(out);
  console.log(`brand: built ${files.length} files into ${relative(process.cwd(), out) || '.'}`);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) main(process.argv.slice(2));
