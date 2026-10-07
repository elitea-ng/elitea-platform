import { Pressable, StyleSheet, View, type PressableProps } from 'react-native';

import { useDesign } from '@/design/DesignProvider';

import { Badge } from './Badge';
import { haptic, type HapticEvent } from './haptics';
import { Icon } from './Icon';
import type { IconName } from './icons';
import { hitSlopFor, hitTarget, pressedOverlay, ripple, uiPlatform } from './platform';
import type { TextTone } from './Text';

export type IconButtonVariant = 'glass' | 'tonal' | 'filled' | 'plain';

export interface IconButtonProps extends Omit<PressableProps, 'children' | 'style'> {
  icon: IconName;
  /** Required: an icon-only control always speaks (§1.8). */
  accessibilityLabel: string;
  variant?: IconButtonVariant;
  /** Visual diameter; the touch target is grown to 44/48 with hitSlop. Default 40. */
  size?: number;
  badge?: number;
  selected?: boolean;
  haptic?: HapticEvent;
  /** A Pressable's static style (Expo's widened web style type fits Pressable, not View, #30). */
  style?: Exclude<PressableProps['style'], (...args: never[]) => unknown>;
}

/**
 * Circular icon button (§4.13 "icon circle"): glass by default, 40 pt visual, 44 pt / 48 dp
 * touch target, ripple on Android and a pressed tint on iOS. The badge sits inside the frame.
 */
export function IconButton({
  icon,
  accessibilityLabel,
  variant = 'glass',
  size = 40,
  badge,
  selected,
  haptic: feedback,
  onPress,
  disabled,
  style,
  accessibilityState,
  ...rest
}: IconButtonProps) {
  const design = useDesign();
  const { colors } = design;
  const fill =
    variant === 'filled' ? colors.accent : variant === 'tonal' ? colors.accentSoft : variant === 'glass' ? colors.surfaceGlass.opaqueFallback : 'transparent';
  const tone: TextTone = variant === 'filled' ? 'onAccent' : variant === 'tonal' ? 'accentSoftOn' : selected ? 'accent' : 'primary';
  // A plain icon button has no frame of its own, so its frame IS the target.
  const frame = variant === 'plain' ? Math.max(size, hitTarget()) : size;
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={accessibilityLabel}
      accessibilityState={{ disabled: disabled === true, selected, ...accessibilityState }}
      disabled={disabled}
      hitSlop={hitSlopFor(frame)}
      android_ripple={ripple(design, variant === 'plain')}
      onPress={(event) => {
        if (feedback) haptic(feedback);
        onPress?.(event);
      }}
      style={({ pressed }) => [
        styles.base,
        { width: frame, height: frame, borderRadius: frame / 2, backgroundColor: fill },
        variant === 'glass' ? { borderWidth: StyleSheet.hairlineWidth, borderColor: colors.separator } : null,
        pressed && uiPlatform === 'ios' ? { opacity: 0.7 } : null,
        pressed && uiPlatform === 'ios' && variant === 'plain' ? { backgroundColor: pressedOverlay(design) } : null,
        disabled ? styles.disabled : null,
        style,
      ]}
      {...rest}>
      <Icon name={icon} size={design.size.iconButton} tone={tone} selected={selected} />
      {badge !== undefined && badge > 0 ? (
        <View style={styles.badgeSlot} pointerEvents="none">
          <Badge count={badge} />
        </View>
      ) : null}
    </Pressable>
  );
}

const styles = StyleSheet.create({
  base: { alignItems: 'center', justifyContent: 'center', borderCurve: 'continuous' },
  badgeSlot: { position: 'absolute', top: 0, end: 0 },
  disabled: { opacity: 0.38 },
});
