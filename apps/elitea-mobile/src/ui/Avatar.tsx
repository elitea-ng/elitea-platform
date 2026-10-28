import { Image, StyleSheet, View, type ViewProps } from 'react-native';

import { EliteaMark } from '@/components/EliteaMark';
import { useDesign } from '@/design/DesignProvider';
import type { Scheme } from '@/design/tokens';

import { Text } from './Text';

/**
 * Initials avatars take one of six pastel tones hashed from the name (audit 2.14), proven
 * ≥ 4.5:1 for the initials in both schemes (Avatar.test.ts). Never the workspace accent:
 * people are not branded.
 */
export const AVATAR_TONES: Record<Scheme, readonly (readonly [fill: string, text: string])[]> = {
  // Brand v3: indigo, cyan, green, amber, red and slate washes. Never violet (AI only) or
  // orange (the dot only).
  light: [
    ['#EDEDFD', '#3336C0'],
    ['#E0F5FA', '#005E73'],
    ['#E8F2EC', '#166534'],
    ['#FCF6F1', '#92400E'],
    ['#F8E8E8', '#991B1B'],
    ['#E8EBF3', '#3A4256'],
  ],
  dark: [
    ['#191A46', '#B4B6FF'],
    ['#0A2A38', '#7FDDF2'],
    ['#0E2521', '#86EFAC'],
    ['#2C2016', '#FCD34D'],
    ['#2C1925', '#FCA5A5'],
    ['#232A45', '#D3D8E6'],
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
   * An agent with no icon of its own shows the Elitea mark (design-system §5): only for the
   * conversation's primary agent, and never twice on one screen. Others get a monogram.
   */
  mark?: boolean;
  size?: number;
  /** Hidden from screen readers when the row next to it already says the name. */
  decorative?: boolean;
  style?: ViewProps['style'];
}

/**
 * A person or agent avatar: their own image; else, for an agent, the Elitea mark (fallback only,
 * labelled with the AGENT's name) or an AI-coloured monogram on `secondarySoft`; else initials on a pastel
 * tone. The Elitea mark never replaces an agent's own icon or the workspace logo.
 */
export function Avatar({ name, kind = 'person', imageUri, mark = false, size = 36, decorative = false, style }: AvatarProps) {
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
  if (kind === 'agent' && mark) {
    return <EliteaMark variant="avatar" size={size} label={name} decorative={decorative} style={style} />;
  }
  // An agent is AI: its monogram takes the AI text colour on the neutral tile (violet = AI only).
  const [fill, text] = kind === 'agent' ? [colors.secondarySoft, colors.aiText] : AVATAR_TONES[scheme][toneIndex(name)];
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
