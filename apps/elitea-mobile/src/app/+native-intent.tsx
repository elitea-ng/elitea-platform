import { rewriteSystemPath } from '@/linking/nativeIntent';

/** See src/linking/nativeIntent.ts. Must never throw (an error here crashes the app). */
export function redirectSystemPath({ path, initial }: { path: string; initial: boolean }): string | null {
  try {
    return rewriteSystemPath(path, initial);
  } catch {
    return '/';
  }
}
