import { describe, expect, it } from 'vitest';

import { isMarkdownFile } from './isMarkdownFile';

function file(name: string, type: string): File {
  return new File(['# heading'], name, { type });
}

describe('isMarkdownFile (issue 889, ELITEA-0940)', () => {
  it('admits a Markdown file by extension, whatever the OS called its type', () => {
    // The `.md` the picker offers, with the three type spellings the same file
    // gets on different platforms.
    expect(isMarkdownFile(file('notes.md', 'text/markdown'))).toBe(true);
    expect(isMarkdownFile(file('notes.md', 'text/plain'))).toBe(true);
    expect(isMarkdownFile(file('notes.md', ''))).toBe(true);
    expect(isMarkdownFile(file('NOTES.MD', ''))).toBe(true);
    expect(isMarkdownFile(file('overview.markdown', ''))).toBe(true);
  });

  it('admits a Markdown file by type when the name carries no extension', () => {
    expect(isMarkdownFile(file('clipboard-paste', 'text/markdown'))).toBe(true);
    expect(isMarkdownFile(file('clipboard-paste', 'text/x-markdown'))).toBe(true);
  });

  it('refuses everything else, including a .txt holding valid Markdown', () => {
    // THE case. The content is fine; the file is not a Markdown file, and the
    // `accept` attribute never stopped it from reaching the input.
    expect(isMarkdownFile(file('not-markdown.txt', 'text/plain'))).toBe(false);
    expect(isMarkdownFile(file('report.pdf', 'application/pdf'))).toBe(false);
    expect(isMarkdownFile(file('archive.zip', 'application/zip'))).toBe(false);
    expect(isMarkdownFile(file('no-extension', ''))).toBe(false);
    // A name that merely CONTAINS the extension is not one: `.md` has to end
    // the name, or `readme.mdx.txt` would import as Markdown.
    expect(isMarkdownFile(file('readme.md.txt', 'text/plain'))).toBe(false);
  });
});
