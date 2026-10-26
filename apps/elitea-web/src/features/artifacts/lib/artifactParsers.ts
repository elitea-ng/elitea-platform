import type { ArtifactDataFile } from '../model/types';

function parseDelimitedLine(line: string, delimiter: ',' | '\t'): string[] {
  if (delimiter === '\t') return line.split('\t');
  const cells: string[] = [];
  let value = '';
  let quoted = false;
  for (let index = 0; index < line.length; index += 1) {
    const character = line[index];
    const nextCharacter = line[index + 1];
    if (character === '"' && quoted && nextCharacter === '"') {
      value += '"';
      index += 1;
    } else if (character === '"') {
      quoted = !quoted;
    } else if (character === delimiter && !quoted) {
      cells.push(value.trim());
      value = '';
    } else {
      value += character;
    }
  }
  cells.push(value.trim());
  return cells;
}

export function parseDataFile(content: string, type: 'csv' | 'tsv'): ArtifactDataFile {
  const normalized = content.trim();
  if (normalized === '') return { headers: [], rows: [] };
  const delimiter = type === 'csv' ? ',' : '\t';
  const [header = '', ...lines] = normalized.split(/\r?\n/);
  return {
    headers: parseDelimitedLine(header, delimiter),
    rows: lines.map((line) => parseDelimitedLine(line, delimiter)),
  };
}

export type ArtifactPreviewKind = 'markdown' | 'csv' | 'tsv' | 'mermaid' | 'image' | 'pdf' | 'text' | 'docx' | 'unsupported';

const DEDICATED_KIND_EXTENSIONS: ReadonlyMap<string, ArtifactPreviewKind> = new Map([
  ['md', 'markdown'], ['markdown', 'markdown'],
  ['csv', 'csv'],
  ['tsv', 'tsv'],
  ['mermaid', 'mermaid'], ['mmd', 'mermaid'],
  ['docx', 'docx'], ['doc', 'docx'],
  ['pdf', 'pdf'],
]);

/** The image kinds and the MIME type each is shown with. */
const IMAGE_MIME_TYPES: ReadonlyMap<string, string> = new Map([
  ['png', 'image/png'], ['apng', 'image/apng'],
  ['jpg', 'image/jpeg'], ['jpeg', 'image/jpeg'],
  ['gif', 'image/gif'],
  ['webp', 'image/webp'],
  ['svg', 'image/svg+xml'],
  ['bmp', 'image/bmp'],
  ['ico', 'image/x-icon'],
  ['tiff', 'image/tiff'], ['tif', 'image/tiff'],
  ['avif', 'image/avif'],
]);

/** Ported from apps/elitea-ui/src/utils/filePreview.js's PREVIEWABLE_EXTENSIONS (minus the extensions handled by their own dedicated kinds above: md/markdown, csv, tsv, mermaid/mmd, docx/doc, and the image set). */
const TEXT_EXTENSIONS = new Set([
  'txt', 'text',
  'json', 'jsonl', 'ndjson',
  'js', 'jsx', 'javascript', 'ts', 'tsx', 'typescript', 'py', 'python', 'java',
  'c', 'cpp', 'cxx', 'cc', 'h', 'hpp', 'cs', 'csharp', 'php', 'rb', 'ruby',
  'go', 'rs', 'rust', 'kt', 'kotlin', 'swift', 'r', 'scala',
  'sh', 'bash', 'zsh', 'ps1', 'powershell',
  'html', 'htm', 'css', 'scss', 'sass', 'less', 'xml',
  'yaml', 'yml', 'toml', 'ini', 'conf', 'config', 'env', 'properties',
  'rst', 'tex',
  'feature', 'gherkin',
  'log', 'logs',
  'pl', 'perl', 'lua', 'vim', 'dart',
  'rdf', 'owl', 'n3', 'ttl', 'sparql',
  'gradle', 'mvn', 'pom', 'cmake', 'gyp',
  'cfg', 'cnf', 'rc', 'profile', 'bashrc', 'zshrc', 'vimrc', 'gitconfig', 'npmrc', 'yarnrc',
  'license', 'licence', 'copyright', 'notice', 'authors', 'contributors', 'changelog', 'changes', 'history',
  'adoc', 'asciidoc', 'org', 'wiki',
  'sql', 'cql', 'hql', 'psql',
  'dockerignore', 'k8s', 'kube', 'helm',
  'jenkins', 'jenkinsfile', 'travis', 'circleci', 'github', 'gitlab-ci',
  'spec', 'test',
  'patch', 'diff',
  'dot', 'gv', 'puml', 'plantuml',
  'wat', 'wast',
  'proto', 'protobuf', 'thrift', 'avsc', 'avro', 'xsd', 'wsdl',
  'dockerfile', 'makefile', 'gitignore', 'editorconfig', 'eslintrc', 'prettierrc', 'babelrc',
]);

/** Ported from filePreview.js's canPreviewFile "special case: files without extensions that are commonly text-based". */
const TEXT_SPECIAL_FILENAMES = new Set([
  'dockerfile', 'makefile', 'gemfile', 'rakefile', 'vagrantfile', 'procfile', 'gruntfile', 'gulpfile', 'webpack',
  'gitignore', 'gitattributes', 'gitmodules', 'gitkeep',
  'readme', 'license', 'licence', 'copyright', 'authors', 'contributors', 'changelog', 'changes', 'history', 'news', 'todo', 'notice',
  'editorconfig', 'prettierrc', 'eslintrc', 'babelrc', 'npmrc', 'yarnrc', 'dockerignore', 'gitignore', 'npmignore', 'eslintignore',
  'jenkinsfile', 'circleci', 'travis',
  'bashrc', 'zshrc', 'profile', 'vimrc', 'tmux', 'screenrc',
]);

/** Baseline getPreviewSizeLimit(): large files are not previewed, for performance (DOCX is exempt — its own preview path handles that separately). */
export const ARTIFACT_PREVIEW_SIZE_LIMIT_BYTES = 2 * 1024 * 1024;

export function isArtifactPreviewableSize(sizeInBytes: number | undefined, kind: ArtifactPreviewKind): boolean {
  if (kind === 'docx') return true;
  if (typeof sizeInBytes !== 'number') return true;
  return sizeInBytes <= ARTIFACT_PREVIEW_SIZE_LIMIT_BYTES;
}

export function artifactPreviewKind(filename: string): ArtifactPreviewKind {
  const lower = filename.toLowerCase();
  const basename = lower.split('/').pop() ?? lower;
  const extension = basename.includes('.') ? (basename.split('.').pop() ?? '') : '';
  const dedicated = DEDICATED_KIND_EXTENSIONS.get(extension);
  if (dedicated !== undefined) return dedicated;
  if (IMAGE_MIME_TYPES.has(extension)) return 'image';
  if (TEXT_EXTENSIONS.has(extension) || TEXT_SPECIAL_FILENAMES.has(basename)) return 'text';
  return 'unsupported';
}

/**
 * The image as the preview shows it: typed from the file name when the stored
 * type is not an image one. An object stored as `application/octet-stream`
 * (an upload that sent no type) still renders as a raster image, because the
 * browser sniffs those, but never as SVG: an `<img>` shows SVG only when the
 * blob says `image/svg+xml`. It stays in `<img>`, where SVG runs no script.
 */
export function artifactImageBlob(blob: Blob, filename: string): Blob {
  if (blob.type.startsWith('image/')) return blob;
  const lower = filename.toLowerCase();
  const type = IMAGE_MIME_TYPES.get(lower.includes('.') ? (lower.split('.').pop() ?? '') : '');
  return type === undefined ? blob : new Blob([blob], { type });
}
