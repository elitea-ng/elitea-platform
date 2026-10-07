import { useEffect, useMemo, useState } from 'react';
import { Platform, StyleSheet, View, useColorScheme } from 'react-native';
import { useMarkdown } from 'react-native-marked';

import { usePalette } from '@/constants/use-palette';
import { SafeRenderer } from '@/features/markdown/SafeRenderer';
import { useShareOut } from '@/security/policy';
import type { LinkOpener } from '@/features/markdown/links';
import { ramp } from '@/ui';

export interface MarkdownProps {
  value: string;
  /** While streaming, re-render at most every `throttleMs`. */
  streaming?: boolean;
  throttleMs?: number;
  openLink?: LinkOpener;
}

/** The platform's built-in monospace face (no font to bundle). */
export const MONOSPACE_FONT = Platform.select({ ios: 'Menlo', default: 'monospace' });

/**
 * react-native-marked styles inline code italic and light in the body face;
 * code reads as code only in an upright monospace face.
 */
const codeStyles = {
  codespan: { fontFamily: MONOSPACE_FONT, fontStyle: 'normal', fontWeight: '400' },
  codeText: { fontFamily: MONOSPACE_FONT, fontStyle: 'normal' },
} as const;

/** Re-render a fast-changing string at most every `ms` (latest value wins). */
export function useThrottled(value: string, ms: number, enabled: boolean): string {
  const [shown, setShown] = useState(value);
  useEffect(() => {
    if (!enabled) return;
    const timer = setTimeout(() => setShown(value), ms);
    return () => clearTimeout(timer);
  }, [value, ms, enabled]);
  return enabled ? shown : value;
}

/** Agent output as markdown, through the SafeRenderer (no remote images, guarded links). */
export function Markdown({ value, streaming = false, throttleMs = 50, openLink }: MarkdownProps) {
  const palette = usePalette();
  const colorScheme = useColorScheme();
  const shown = useThrottled(value, throttleMs, streaming);
  const selectable = useShareOut();
  const renderer = useMemo(
    () => new SafeRenderer({ openLink, chipStyle: { ...ramp.subheadline, color: palette.accent }, selectable }),
    [openLink, palette.accent, selectable],
  );
  const nodes = useMarkdown(shown, {
    renderer,
    styles: codeStyles,
    colorScheme: colorScheme ?? undefined,
    theme: {
      colors: { text: palette.text, link: palette.accent, border: palette.border, code: palette.background },
    },
  });
  return <View style={styles.container}>{nodes}</View>;
}

const styles = StyleSheet.create({
  container: { gap: 4 },
});
