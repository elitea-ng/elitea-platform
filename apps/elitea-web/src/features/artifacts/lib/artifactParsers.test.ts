import { describe, expect, it } from 'vitest';

import { artifactImageBlob, artifactPreviewKind, isArtifactPreviewableSize, parseDataFile } from './artifactParsers';

describe('artifact parsers', () => {
  it('parses quoted CSV values, escaped quotes, and line endings', () => {
    expect(parseDataFile('name,note\r\nAlice,"hello, world"\r\nBob,"said ""yes"""', 'csv')).toEqual({
      headers: ['name', 'note'],
      rows: [
        ['Alice', 'hello, world'],
        ['Bob', 'said "yes"'],
      ],
    });
  });

  it('parses TSV and empty content', () => {
    expect(parseDataFile('name\tage\nAda\t36', 'tsv')).toEqual({
      headers: ['name', 'age'],
      rows: [['Ada', '36']],
    });
    expect(parseDataFile('  ', 'csv')).toEqual({ headers: [], rows: [] });
  });

  it.each([
    ['README.md', 'markdown'],
    ['table.csv', 'csv'],
    ['table.tsv', 'tsv'],
    ['flow.mmd', 'mermaid'],
    ['photo.PNG', 'image'],
    ['data.json', 'text'],
    ['report.docx', 'docx'],
    ['report.pdf', 'pdf'],
    ['scans/REPORT.PDF', 'pdf'],
    ['diagram.SVG', 'image'],
    ['archive.zip', 'unsupported'],
    ['LICENSE', 'text'],
    ['Dockerfile', 'text'],
    ['.gitignore', 'text'],
    ['worker.rb', 'text'],
    ['server.rs', 'text'],
    ['index.php', 'text'],
    ['main.cpp', 'text'],
    ['app.toml', 'text'],
    ['config.ini', 'text'],
    ['deploy.log', 'text'],
    ['setup.ps1', 'text'],
  ] as const)('classifies %s as %s', (filename, expected) => {
    expect(artifactPreviewKind(filename)).toBe(expected);
  });

  describe('artifactImageBlob', () => {
    it('types an untyped SVG so an <img> shows it, and keeps a typed image as it is', () => {
      const untyped = new Blob(['<svg/>'], { type: 'application/octet-stream' });
      expect(artifactImageBlob(untyped, 'icons/Logo.SVG').type).toBe('image/svg+xml');
      expect(artifactImageBlob(new Blob(['x']), 'photo.jpg').type).toBe('image/jpeg');
      const typed = new Blob(['x'], { type: 'image/png' });
      expect(artifactImageBlob(typed, 'photo.svg')).toBe(typed);
      expect(artifactImageBlob(untyped, 'notes.txt')).toBe(untyped);
    });
  });

  describe('isArtifactPreviewableSize', () => {
    it('allows any size for docx, since its own preview path handles that separately', () => {
      expect(isArtifactPreviewableSize(50 * 1024 * 1024, 'docx')).toBe(true);
    });
    it('allows an unknown size to be permissive', () => {
      expect(isArtifactPreviewableSize(undefined, 'text')).toBe(true);
    });
    it('rejects files over the 2MB preview limit', () => {
      expect(isArtifactPreviewableSize(2 * 1024 * 1024 + 1, 'text')).toBe(false);
      expect(isArtifactPreviewableSize(2 * 1024 * 1024, 'text')).toBe(true);
    });
  });
});
