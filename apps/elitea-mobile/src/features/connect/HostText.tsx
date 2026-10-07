import { StyleSheet, Text, View } from 'react-native';

import { maxScale, ramp } from '@/ui';

/**
 * A host split into the pieces it may wrap between: after each dot ("elitea." "technicaldomain."
 * "xyz"). Line breaking never offers a break after a dot followed by a letter, so a long host at
 * a large text size used to wrap mid-label ("elitea.tec / hnicaldo / main.xyz").
 */
export function hostSegments(host: string): string[] {
  return host.match(/[^.]+\.?|\./g) ?? [host];
}

/** Labels up to this length shrink to stay on one line instead of breaking inside the label. */
export const FIT_LABEL_MAX = 30;

/**
 * A label wider than the line at a large text size shrinks (down to a third) to stay whole on
 * its line, so "technicaldomain." never breaks inside. A label longer than FIT_LABEL_MAX wraps
 * instead: shrinking it that far could still end in an ellipsis, and the host is never cut.
 */
export function segmentFit(segment: string): { numberOfLines?: number; adjustsFontSizeToFit?: boolean; minimumFontScale?: number } {
  return segment.length <= FIT_LABEL_MAX ? { numberOfLines: 1, adjustsFontSizeToFit: true, minimumFontScale: 0.3 } : {};
}

/**
 * A workspace host as a heading-sized label that wraps only at its dots (a label longer than the
 * line still wraps inside itself). Always the system face: a brand font must not be able to
 * disguise the host. Never truncated, because the host is what a phishing link cannot fake; the
 * role's scale cap applies. Read as one element ("elitea.technicaldomain.xyz").
 */
export function HostText({
  host,
  role,
  color,
  heading,
  testID,
}: {
  host: string;
  role: 'title1' | 'title2';
  color: string;
  heading?: boolean;
  testID?: string;
}) {
  return (
    <View
      accessible
      accessibilityRole={heading ? 'header' : 'text'}
      accessibilityLabel={host}
      testID={testID}
      style={styles.row}>
      {hostSegments(host).map((segment, index) => (
        <Text
          // Segments can repeat ("a.a."), so the index is the key.
          key={index}
          importantForAccessibility="no"
          maxFontSizeMultiplier={maxScale[role]}
          {...segmentFit(segment)}
          style={[ramp[role], styles.weight, { color }]}>
          {segment}
        </Text>
      ))}
    </View>
  );
}

const styles = StyleSheet.create({
  row: { flexDirection: 'row', flexWrap: 'wrap' },
  weight: { fontWeight: '800', maxWidth: '100%', flexShrink: 1 },
});
