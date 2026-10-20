import { useEffect, useMemo, useState } from 'react';
import { StyleSheet, View } from 'react-native';
import { useMarkdown } from 'react-native-marked';

import { useDesign } from '@/design/DesignProvider';
import { withCaretMark } from '@/features/markdown/InlineCaret';
import { MONOSPACE_FONT } from '@/features/markdown/monospace';
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
  /** Draw the pulsing streaming caret right after the last character (on its last line). */
  caret?: boolean;
}

export { MONOSPACE_FONT };

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
export function Markdown({ value, streaming = false, throttleMs = 50, openLink, caret = false }: MarkdownProps) {
  const { colors, scheme } = useDesign();
  const shown = useThrottled(value, throttleMs, streaming);
  const selectable = useShareOut();
  const renderer = useMemo(
    () => new SafeRenderer({ openLink, chipStyle: { ...ramp.subheadline, color: colors.accent }, selectable }),
    [openLink, colors.accent, selectable],
  );
  // The type ramp, not the library's sizes: body text is the body role (17 pt on iOS), headings
  // step down the title roles, and every size scales with Dynamic Type / font scale.
  const markedStyles = useMemo(
    () => ({
      text: { ...ramp.body, color: colors.bubbleAgentText },
      paragraph: { paddingVertical: 2 },
      strong: { fontWeight: '600' as const },
      h1: { ...ramp.title2, fontWeight: '700' as const, color: colors.bubbleAgentText },
      h2: { ...ramp.title3, fontWeight: '600' as const, color: colors.bubbleAgentText },
      h3: { ...ramp.headline, fontWeight: '600' as const, color: colors.bubbleAgentText },
      h4: { ...ramp.headline, fontWeight: '600' as const, color: colors.bubbleAgentText },
      li: { ...ramp.body, color: colors.bubbleAgentText },
      link: { ...ramp.body, color: colors.accent, textDecorationLine: 'underline' as const },
      blockquote: { borderStartWidth: 3, borderStartColor: colors.separatorStrong, paddingStart: 10 },
      // react-native-marked styles inline code italic and light in the body face; code reads as
      // code only in an upright monospace face, on the sunken tone.
      codespan: { fontFamily: MONOSPACE_FONT, fontStyle: 'normal' as const, fontWeight: '400' as const, backgroundColor: colors.surfaceSunken },
      codeText: { fontFamily: MONOSPACE_FONT, fontStyle: 'normal' as const },
      hr: { backgroundColor: colors.separator },
    }),
    [colors],
  );
  // The caret is a mark at the end of the source that the renderer swaps for the inline caret,
  // so it lands in whatever block the text ends in (a paragraph, a list item, an open code fence).
  const nodes = useMarkdown(caret ? withCaretMark(shown) : shown, {
    renderer,
    styles: markedStyles,
    colorScheme: scheme,
    theme: {
      colors: { text: colors.bubbleAgentText, link: colors.accent, border: colors.separator, code: colors.surfaceSunken },
    },
  });
  return <View style={styles.container}>{nodes}</View>;
}

const styles = StyleSheet.create({
  container: { gap: 4 },
});
