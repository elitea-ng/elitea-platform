import { Platform } from 'react-native';

import { identity } from '@/config/identity';

/**
 * What the app claims about this device at sign-in (shown to the user on the
 * workspace's confirmation page and in its device list). Deliberately coarse:
 * no hardware identifiers.
 */
export function devicePlatform(os: string = Platform.OS): 'ios' | 'android' | 'macos' | 'windows' | 'other' {
  return os === 'ios' || os === 'android' || os === 'macos' || os === 'windows' ? os : 'other';
}

export function deviceName(
  os: string = Platform.OS,
  constants: Record<string, unknown> = Platform.constants as unknown as Record<string, unknown>,
): string {
  let model: string;
  if (os === 'ios') model = constants.interfaceIdiom === 'pad' ? 'iPad' : 'iPhone';
  else if (os === 'android') model = typeof constants.Model === 'string' && constants.Model ? constants.Model : 'Android';
  else model = 'device';
  // The server accepts 1 to 64 bytes without control characters.
  const name = `${identity.displayName} on ${model}`.replace(/[\u0000-\u001f\u007f]/g, '');
  return truncateUtf8(name, 64);
}

function truncateUtf8(text: string, maxBytes: number): string {
  let bytes = 0;
  let out = '';
  for (const char of text) {
    const code = char.codePointAt(0) ?? 0;
    const size = code < 0x80 ? 1 : code < 0x800 ? 2 : code < 0x10000 ? 3 : 4;
    if (bytes + size > maxBytes) break;
    bytes += size;
    out += char;
  }
  return out;
}
