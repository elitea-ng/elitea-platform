const MAX_FOLDER_DEPTH = 10;
const VALID_SEGMENT = /^[a-zA-Z0-9._-]+$/;
const FORBIDDEN_PATH_PATTERNS = ['..', '~', '\\', '%2e%2e'];
const FORBIDDEN_FILENAME_CHARACTERS = [':', '*', '?', '"', '<', '>', '|', '\0', '#', '~', '/', '\\'];

// oxlint-disable-next-line complexity -- each branch reports one distinct security validation failure.
export function validateFolderPath(path: string, currentPrefix = ''): string {
  if (path === '') return '';
  const normalized = path.toLowerCase().replaceAll('\\', '/');
  const pattern = FORBIDDEN_PATH_PATTERNS.find((candidate) => normalized.includes(candidate));
  if (pattern !== undefined) {
    return `Path contains forbidden pattern "${pattern}". Path traversal is not allowed.`;
  }
  if (path.startsWith('/') || path.startsWith('\\')) {
    return 'Absolute paths are not allowed. Path must be relative to the current location.';
  }
  if (path.includes('//') || path.includes('\\\\')) {
    return 'Path must not contain consecutive separators.';
  }

  const segments = path.replace(/\/+$/, '').split('/').filter(Boolean);
  const currentDepth = currentPrefix.split('/').filter(Boolean).length;
  if (currentDepth + segments.length > MAX_FOLDER_DEPTH) {
    return `Maximum folder depth is ${MAX_FOLDER_DEPTH} levels.`;
  }

  for (const segment of segments) {
    if (segment === '.' || segment === '..' || /^\.+$/.test(segment)) {
      return `"${segment}" is not allowed. Relative path references are forbidden.`;
    }
    const forbiddenCharacter = FORBIDDEN_FILENAME_CHARACTERS.find((character) => segment.includes(character));
    if (forbiddenCharacter !== undefined) {
      return `"${segment}" contains forbidden character "${forbiddenCharacter}".`;
    }
    if (!VALID_SEGMENT.test(segment)) {
      return `"${segment}" contains invalid characters. Only letters, numbers, dots, hyphens, and underscores are allowed.`;
    }
  }
  return '';
}

export function computeSecurePath(path: string, currentPrefix = ''): string {
  const error = validateFolderPath(path, currentPrefix);
  if (error !== '') throw new Error(error);
  const sanitized = path.replaceAll('\\', '/').replace(/^\/+|\/+$/g, '').replace(/\/+/g, '/');
  const prefix = currentPrefix.replace(/\/+$/, '');
  return [prefix, sanitized].filter(Boolean).join('/');
}

export function validateFileName(name: string): string {
  if (name === '') return 'Filename is empty or invalid.';
  if (name.includes('..')) return 'Filenames cannot contain consecutive dots (..).';
  const forbiddenCharacter = FORBIDDEN_FILENAME_CHARACTERS.find((character) => name.includes(character));
  return forbiddenCharacter === undefined ? '' : `Filenames cannot contain "${forbiddenCharacter}".`;
}

export function ensureTrailingSlash(path: string): string {
  return path !== '' && !path.endsWith('/') ? `${path}/` : path;
}

/**
 * A file name split into the part a person may rename and the extension that stays (#2119).
 * Only a dot after the first character starts an extension, so a name like ".env" has none;
 * the server applies the same rule to a rename (`keyExtension`).
 */
export function splitFileName(name: string): { readonly stem: string; readonly extension: string } {
  const dot = name.lastIndexOf('.');
  if (dot <= 0) return { stem: name, extension: '' };
  return { stem: name.slice(0, dot), extension: name.slice(dot) };
}
