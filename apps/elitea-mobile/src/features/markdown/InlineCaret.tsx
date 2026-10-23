import { useEffect, useMemo, useState } from 'react';
import { Text } from 'react-native';

import { rgb } from '@/design/contrast';
import { useDesign } from '@/design/DesignProvider';
import { composite, contrastRatio, formatColor } from '@/theme/color';

/**
 * Marks where the streaming caret goes in a markdown source. U+2060 WORD JOINER: invisible
 * and zero-width, so if a renderer path ever leaks it, nothing shows.
 */
export const CARET_MARK = '⁠';

/**
 * `source` with the caret mark after its last character. A last line that is block syntax
 * would stop being it with the mark glued on, so the mark goes on the next line instead: after
 * a code fence (inside the block it opens, or under the one it closes), and in a paragraph of
 * its own after a rule, a setext underline or a table divider.
 */
export function withCaretMark(source: string): string {
  const trimmed = source.replace(/\s+$/, '');
  const lastLine = trimmed.slice(trimmed.lastIndexOf('\n') + 1);
  if (/^ {0,3}(`{3,}|~{3,})/.test(lastLine)) return `${trimmed}\n${CARET_MARK}`;
  if (/^ {0,3}([-=*_] *){3,}$|^ {0,3}\|?( *:?-+:? *\|)+ *:?-*:? *$/.test(lastLine)) return `${trimmed}\n\n${CARET_MARK}`;
  return `${trimmed}${CARET_MARK}`;
}

/** Non-text UI bar (WCAG 1.4.11): the caret stays visible in its dim phase too. */
const CARET_MIN_CONTRAST = 3;

/**
 * The caret's dim phase: the accent at the lowest opacity (from 0.4 up) that still holds 3:1
 * on the answer bubble.
 */
export function dimCaretColor(accent: string, ground: string): string {
  const fill = rgb(accent);
  const under = rgb(ground);
  for (let alpha = 0.4; alpha < 1; alpha += 0.05) {
    const dim = composite({ ...fill, a: alpha }, under);
    if (contrastRatio(dim, under) >= CARET_MIN_CONTRAST) return formatColor(dim);
  }
  return accent;
}

/**
 * The pulsing caret at the end of a streaming answer (docs/design-system.md §10): a nested
 * text run, so it sits on the last line right after the last word and is as tall as the
 * line at every text size. It is an en space on an accent background, which screen readers
 * skip. The pulse swaps two colours (both at least 3:1); under Reduce Motion it is steady.
 */
export function InlineCaret() {
  const { colors, motion, reduceMotion } = useDesign();
  const dim = useMemo(() => dimCaretColor(colors.accent, colors.bubbleAgent), [colors.accent, colors.bubbleAgent]);
  const [phase, setPhase] = useState(true);
  useEffect(() => {
    if (reduceMotion) return;
    const timer = setInterval(() => setPhase((b) => !b), motion.duration.slow);
    return () => clearInterval(timer);
  }, [reduceMotion, motion.duration.slow]);
  const bright = reduceMotion || phase;
  return (
    <Text testID="streaming-caret" style={{ backgroundColor: bright ? colors.accent : dim }}>
      {' '}
    </Text>
  );
}
