import { Image, StyleSheet, View, type ViewProps } from 'react-native';

import { Mascot } from '@/components/Mascot';
import { useDesign } from '@/design/DesignProvider';
import type { Scheme } from '@/design/tokens';

import { Text } from './Text';

/**
 * Initials avatars take one of six pastel tones hashed from the name (audit 2.14), proven
 * ≥ 4.5:1 for the initials in both schemes (Avatar.test.ts). Never the workspace accent:
 * people are not branded.
 */
export const AVATAR_TONES: Record<Scheme, readonly (readonly [fill: string, text: string])[]> = {
  light: [
    ['#FCE4EE', '#93244F'],
    ['#DDEBF5', '#1E4A6B'],
    ['#E8F5EF', '#1A6B43'],
    ['#FCEBD6', '#7A4700'],
    ['#EDE4FA', '#4B2E83'],
    ['#F5EFF3', '#5A5262'],
  ],
  dark: [
    ['#3D1E2D', '#FFC2DA'],
    ['#1C2C3A', '#BFDDF3'],
    ['#16291F', '#8FE3BC'],
    ['#2E2213', '#F3C77A'],
    ['#2A2340', '#D5C6F7'],
    ['#26222C', '#E6DDE3'],
  ],
};

/** The first letter of the first two words: "Alex Smith" → "AS". */
export function initialsOf(name: string): string {
  const words = name.trim().split(/\s+/).filter(Boolean);
  const letters = words.slice(0, 2).map((w) => Array.from(w)[0] ?? '');
  return letters.join('').toUpperCase() || '?';
}

/** A stable tone index for a name (FNV-1a), so a person keeps their colour everywhere. */
export function toneIndex(name: string, tones = 6): number {
  let hash = 0x811c9dc5;
  for (const ch of name.trim().toLowerCase()) {
    hash ^= ch.codePointAt(0) ?? 0;
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return hash % tones;
}

export interface AvatarProps {
  /** The person's or agent's name: the accessibility label and the initials. */
  name: string;
  kind?: 'person' | 'agent';
  /** A vetted image (a `data:` URI or a same-origin file the caller already checked). */
  imageUri?: string | null;
  /**
   * An agent with no icon of its own shows the mascot (design-system §5): only for the
   * conversation's primary agent, and never twice on one screen. Others get a monogram.
   */
  mascot?: boolean;
  size?: number;
  /** Hidden from screen readers when the row next to it already says the name. */
  decorative?: boolean;
  style?: ViewProps['style'];
}

/**
 * A person or agent avatar: their own image; else, for an agent, the mascot (fallback only,
 * labelled with the AGENT's name) or a monogram on `secondarySoft`; else initials on a pastel
 * tone. The mascot never replaces an agent's own icon or the workspace logo.
 */
export function Avatar({ name, kind = 'person', imageUri, mascot = false, size = 36, decorative = false, style }: AvatarProps) {
  const { colors, scheme } = useDesign();
  const a11y = decorative
    ? ({ accessible: false, importantForAccessibility: 'no-hide-descendants', accessibilityElementsHidden: true } as const)
    : ({ accessible: true, accessibilityRole: 'image', accessibilityLabel: name } as const);
  const frame = { width: size, height: size, borderRadius: size / 2 };
  if (imageUri) {
    return (
      <View {...a11y} style={[styles.frame, frame, style]}>
        <Image source={{ uri: imageUri }} style={frame} resizeMode="cover" />
      </View>
    );
  }
  if (kind === 'agent' && mascot) {
    return <Mascot variant="avatar" size={size} label={name} decorative={decorative} style={style} />;
  }
  const [fill, text] = kind === 'agent' ? [colors.secondarySoft, colors.secondarySoftOn] : AVATAR_TONES[scheme][toneIndex(name)];
  return (
    <View {...a11y} style={[styles.frame, frame, { backgroundColor: fill }, style]}>
      <Text variant={size >= 48 ? 'headline' : 'footnote'} color={text} maxFontSizeMultiplier={1} style={styles.initials}>
        {initialsOf(name)}
      </Text>
    </View>
  );
}

const styles = StyleSheet.create({
  frame: { overflow: 'hidden', alignItems: 'center', justifyContent: 'center', borderCurve: 'continuous' },
  initials: { fontWeight: '600' },
});
