import { Pressable, StyleSheet, View, type PressableProps, type ViewProps } from 'react-native';

import { useDesign } from '@/design/DesignProvider';

import { haptic } from './haptics';
import { Icon } from './Icon';
import type { IconName } from './icons';
import { hitSlopFor, ripple } from './platform';
import { Text } from './Text';

export interface ChipProps extends Omit<PressableProps, 'children' | 'style'> {
  /** Shown text; for an icon-only chip, still required: it becomes the accessibility label. */
  label: string;
  icon?: IconName;
  iconOnly?: boolean;
  /** M3 filter-chip state: the filled look AND `accessibilityState.selected`. */
  selected?: boolean;
  /**
   * The filled look without a selection state, for an action chip that leads its row (e.g. Home's
   * "Choose an agent"): screen readers must not announce "selected" for something that is a button.
   */
  filled?: boolean;
  /** Trailing chevron: the chip opens a picker (e.g. the agent target). */
  menu?: boolean;
  /** A Pressable's static style (Expo's widened web style type fits Pressable, not View, #30). */
  style?: Exclude<PressableProps['style'], (...args: never[]) => unknown>;
}

/**
 * A pill chip (§4.3): height 40, touch target 44/48, `chip`/`chipText` (glass on the gradient),
 * `chipSelected`/`chipSelectedText` when selected (with a check on Android, M3 filter chip).
 * Selection changes fire the selection haptic. Icon-only chips are 40 × 40 circles.
 */
export function Chip({ label, icon, iconOnly = false, selected = false, filled = false, menu = false, onPress, style, disabled, ...rest }: ChipProps) {
  const design = useDesign();
  const { colors } = design;
  const on = selected || filled;
  const fg = on ? colors.chipSelectedText : colors.chipText;
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={label}
      accessibilityState={{ selected, disabled: disabled === true }}
      hitSlop={hitSlopFor(40)}
      android_ripple={ripple(design)}
      disabled={disabled}
      onPress={(event) => {
        haptic('tabOrChipChange');
        onPress?.(event);
      }}
      style={({ pressed }) => [
        styles.chip,
        iconOnly ? styles.iconOnly : null,
        {
          backgroundColor: on ? colors.chipSelected : colors.chip,
          borderColor: on ? colors.chipSelected : colors.separator,
        },
        pressed ? styles.pressed : null,
        disabled ? styles.disabled : null,
        style,
      ]}
      {...rest}>
      <View style={styles.row}>
        {selected && !icon ? <Icon name="check" size={16} color={fg} weight="semibold" /> : null}
        {icon ? <Icon name={icon} size={18} color={fg} selected={on} /> : null}
        {iconOnly ? null : (
          <Text variant="subheadline" color={fg} numberOfLines={1} style={styles.label}>
            {label}
          </Text>
        )}
        {menu ? <Icon name="chevronDown" size={14} color={fg} weight="semibold" /> : null}
      </View>
    </Pressable>
  );
}

/** A horizontal row of chips that wraps (at accessibility sizes the row wraps instead of scrolling). */
export function ChipRow({ children, style }: { children: React.ReactNode; style?: ViewProps['style'] }) {
  return <View style={[styles.chipRow, style]}>{children}</View>;
}

const styles = StyleSheet.create({
  chip: {
    minHeight: 40,
    borderRadius: 9999,
    borderCurve: 'continuous',
    borderWidth: StyleSheet.hairlineWidth,
    paddingHorizontal: 14,
    justifyContent: 'center',
    overflow: 'hidden',
  },
  iconOnly: { width: 40, paddingHorizontal: 0, alignItems: 'center' },
  row: { flexDirection: 'row', alignItems: 'center', gap: 6 },
  label: { fontWeight: '500' },
  chipRow: { flexDirection: 'row', flexWrap: 'wrap', gap: 8 },
  pressed: { opacity: 0.8 },
  disabled: { opacity: 0.38 },
});
