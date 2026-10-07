import { ActivityIndicator, Pressable, StyleSheet, View, type PressableProps } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import type { Design } from '@/design/resolve';

import { haptic, type HapticEvent } from './haptics';
import { Icon } from './Icon';
import type { IconName } from './icons';
import { hitSlopFor, pressedOverlay, ripple, uiPlatform, type UiPlatform } from './platform';
import { Text, type TextTone } from './Text';

export type ButtonVariant = 'filled' | 'tonal' | 'plain' | 'destructive' | 'destructivePlain' | 'glass';
export type ButtonSize = 'regular' | 'small';

/** Height of a button (§4.13): 50 pt iOS / 48 dp Android; small buttons keep the hit target via hitSlop. */
export function buttonHeight(size: ButtonSize, on: UiPlatform = uiPlatform): number {
  if (size === 'small') return 36;
  return on === 'ios' ? 50 : 48;
}

interface Look {
  background: string;
  pressed: string;
  tone: TextTone;
  color?: string;
  border?: string;
}

export function buttonLook(variant: ButtonVariant, design: Pick<Design, 'colors' | 'scheme'>): Look {
  const c = design.colors;
  switch (variant) {
    case 'filled':
      return { background: c.accent, pressed: c.accentPressed, tone: 'onAccent' };
    case 'tonal':
      return { background: c.accentSoft, pressed: c.accentSoft, tone: 'accentSoftOn' };
    case 'destructive':
      return { background: c.danger, pressed: c.danger, tone: 'dangerOn' };
    case 'destructivePlain':
      return { background: 'transparent', pressed: pressedOverlay(design), tone: 'danger' };
    case 'glass':
      return { background: c.surfaceGlass.opaqueFallback, pressed: c.surfaceGlass.opaqueFallback, tone: 'primary', border: c.separator };
    case 'plain':
      return { background: 'transparent', pressed: pressedOverlay(design), tone: 'accent' };
  }
}

export interface ButtonProps extends Omit<PressableProps, 'children' | 'style'> {
  title: string;
  variant?: ButtonVariant;
  size?: ButtonSize;
  icon?: IconName;
  /** Shows a spinner; the button reads as busy and does not take taps. */
  loading?: boolean;
  /** Stretch to the container width. Default: true for regular, false for small. */
  block?: boolean;
  haptic?: HapticEvent;
  /** A Pressable's static style (Expo's widened web style type fits Pressable, not View, #30). */
  style?: Exclude<PressableProps['style'], (...args: never[]) => unknown>;
}

/**
 * The base button (§4.13): one filled primary per screen, tonal secondary, plain text,
 * destructive. Pill shape, subheadline semibold label, never all-caps. The label wraps at
 * accessibility sizes (minHeight, not height). Disabled: content α .38 + container α .12,
 * marked `accessibilityState.disabled`.
 */
export function Button({
  title,
  variant = 'filled',
  size = 'regular',
  icon,
  loading = false,
  block,
  disabled,
  haptic: feedback,
  onPress,
  style,
  accessibilityLabel,
  ...rest
}: ButtonProps) {
  const design = useDesign();
  const look = buttonLook(variant, design);
  const height = buttonHeight(size);
  const inactive = disabled === true || loading;
  const stretch = block ?? size === 'regular';
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={accessibilityLabel ?? title}
      accessibilityState={{ disabled: inactive, busy: loading }}
      disabled={inactive}
      hitSlop={hitSlopFor(height)}
      android_ripple={look.background === 'transparent' || variant === 'glass' ? ripple(design) : ripple({ scheme: 'dark' })}
      onPress={(event) => {
        if (feedback) haptic(feedback);
        onPress?.(event);
      }}
      style={({ pressed }) => [
        styles.base,
        {
          minHeight: height,
          paddingHorizontal: size === 'small' ? design.space[4] : design.space[6],
          backgroundColor: pressed && uiPlatform === 'ios' ? look.pressed : look.background,
          alignSelf: stretch ? 'stretch' : 'flex-start',
        },
        look.border ? { borderWidth: StyleSheet.hairlineWidth, borderColor: look.border } : null,
        disabled && look.background !== 'transparent' ? { backgroundColor: disabledContainer(design) } : null,
        style,
      ]}
      {...rest}>
      <View style={[styles.content, disabled ? styles.disabled : null]}>
        {loading ? (
          <ActivityIndicator color={look.color ?? (variant === 'filled' ? design.colors.accentOn : design.colors.accent)} />
        ) : icon ? (
          <Icon name={icon} size={design.size.iconInline} tone={look.tone} weight="semibold" />
        ) : null}
        <Text variant="subheadline" tone={look.tone} style={styles.label}>
          {title}
        </Text>
      </View>
    </Pressable>
  );
}

function disabledContainer(design: Pick<Design, 'scheme'>): string {
  return design.scheme === 'dark' ? 'rgba(247, 242, 245, 0.12)' : 'rgba(28, 22, 34, 0.12)';
}

const styles = StyleSheet.create({
  base: {
    borderRadius: 9999,
    borderCurve: 'continuous',
    alignItems: 'center',
    justifyContent: 'center',
    overflow: 'hidden',
    paddingVertical: 8,
  },
  content: { flexDirection: 'row', alignItems: 'center', gap: 8, flexShrink: 1 },
  label: { fontWeight: '600', textAlign: 'center', flexShrink: 1 },
  disabled: { opacity: 0.38 },
});
