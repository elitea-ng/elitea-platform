import { Pressable, StyleSheet, View, type ViewStyle } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import type { SemanticColors } from '@/design/tokens';

import { haptic } from './haptics';
import { Icon } from './Icon';
import type { IconName } from './icons';
import { hitTarget, ripple, uiPlatform, type UiPlatform } from './platform';
import { Text } from './Text';

export interface Segment<T extends string> {
  value: T;
  label: string;
  icon?: IconName;
}

/**
 * The selected segment. iOS: an `elevated` thumb (a step lighter than the card in dark mode, as
 * UISegmentedControl draws it) with a `separatorStrong` hairline, which is what gives the thumb
 * 3:1 non-text contrast against the `surfaceSunken` track in both schemes (WCAG 1.4.11; the e1
 * shadow is invisible on a dark ground). Android: the M3 `accentSoft` fill plus a check.
 */
export function selectedSegmentStyle(colors: SemanticColors, on: UiPlatform = uiPlatform): ViewStyle {
  if (on === 'android') return { backgroundColor: colors.accentSoft };
  return { backgroundColor: colors.elevated, borderWidth: 1, borderColor: colors.separatorStrong };
}

/**
 * A segmented control (iOS) / segmented button (M3, Android): one choice of a few. Selection is
 * shown by a filled segment AND (Android) a check, never by colour alone; each segment is a
 * radio for screen readers. Segments stack vertically at accessibility text sizes.
 */
export function SegmentedControl<T extends string>({
  segments,
  value,
  onChange,
  accessibilityLabel,
}: {
  segments: readonly Segment<T>[];
  value: T;
  onChange: (value: T) => void;
  accessibilityLabel: string;
}) {
  const design = useDesign();
  const { colors } = design;
  const stacked = design.accessibilityLayout;
  return (
    <View
      accessibilityRole="radiogroup"
      accessibilityLabel={accessibilityLabel}
      style={[
        styles.track,
        stacked ? styles.stacked : null,
        { backgroundColor: uiPlatform === 'ios' ? colors.surfaceSunken : 'transparent', borderColor: colors.separatorStrong, borderWidth: uiPlatform === 'android' ? 1 : 0 },
      ]}>
      {segments.map((segment) => {
        const selected = segment.value === value;
        return (
          <Pressable
            key={segment.value}
            accessibilityRole="radio"
            accessibilityLabel={segment.label}
            accessibilityState={{ selected, checked: selected }}
            android_ripple={ripple(design)}
            onPress={() => {
              if (selected) return;
              haptic('tabOrChipChange');
              onChange(segment.value);
            }}
            style={[
              styles.segment,
              { minHeight: hitTarget() - 6 },
              selected ? selectedSegmentStyle(colors, uiPlatform) : null,
              selected && uiPlatform === 'ios' ? design.elevation.e1 : null,
            ]}>
            {selected && uiPlatform === 'android' ? <Icon name="check" size={16} color={colors.accentSoftOn} /> : null}
            {segment.icon ? <Icon name={segment.icon} size={16} color={selected && uiPlatform === 'android' ? colors.accentSoftOn : colors.textPrimary} /> : null}
            <Text variant="subheadline" color={selected && uiPlatform === 'android' ? colors.accentSoftOn : colors.textPrimary} emphasized={selected} numberOfLines={2} style={styles.label}>
              {segment.label}
            </Text>
          </Pressable>
        );
      })}
    </View>
  );
}

const styles = StyleSheet.create({
  track: { flexDirection: 'row', borderRadius: 9999, borderCurve: 'continuous', padding: 3, gap: 2, overflow: 'hidden' },
  stacked: { flexDirection: 'column', borderRadius: 16 },
  segment: {
    flex: 1,
    flexDirection: 'row',
    gap: 6,
    alignItems: 'center',
    justifyContent: 'center',
    borderRadius: 9999,
    borderCurve: 'continuous',
    paddingHorizontal: 10,
    paddingVertical: 6,
  },
  label: { textAlign: 'center', flexShrink: 1 },
});
