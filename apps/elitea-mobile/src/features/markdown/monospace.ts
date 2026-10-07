import { Platform } from 'react-native';

/** The platform's built-in monospace face (no font to bundle). */
export const MONOSPACE_FONT = Platform.select({ ios: 'Menlo', default: 'monospace' });
