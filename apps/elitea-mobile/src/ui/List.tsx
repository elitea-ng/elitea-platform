import { Children, Fragment, isValidElement, type ReactNode } from 'react';
import {
  Pressable,
  StyleSheet,
  View,
  type AccessibilityActionEvent,
  type AccessibilityActionInfo,
  type ViewProps,
} from 'react-native';

import { useDesign } from '@/design/DesignProvider';

import { Icon } from './Icon';
import type { IconName } from './icons';
import { hitTarget, pressedOverlay, ripple, uiPlatform, type UiPlatform } from './platform';
import { Text } from './Text';

/** Section header text: uppercase footnote on iOS, sentence-case accent label on Android (M3). */
export function sectionTitle(title: string, on: UiPlatform = uiPlatform): string {
  return on === 'ios' ? title.toUpperCase() : title;
}

/**
 * An inset-grouped list section (iOS) / a list with a section header (M3): a `surface` group with
 * radius 16 and hairline dividers between rows, a header and an optional footer note.
 */
export function ListSection({
  title,
  footer,
  children,
  style,
}: {
  title?: string;
  footer?: string;
  children: ReactNode;
  style?: ViewProps['style'];
}) {
  const { colors, radius, scheme } = useDesign();
  const rows = Children.toArray(children).filter(isValidElement);
  return (
    <View style={[styles.section, style]}>
      {title ? (
        <Text
          heading
          variant={uiPlatform === 'ios' ? 'footnote' : 'subheadline'}
          tone={uiPlatform === 'ios' ? 'secondary' : 'accent'}
          style={styles.sectionTitle}>
          {sectionTitle(title)}
        </Text>
      ) : null}
      <View
        style={[
          styles.group,
          { backgroundColor: colors.surface, borderRadius: radius.listRow },
          scheme === 'light' ? { borderColor: colors.separator, borderWidth: StyleSheet.hairlineWidth } : null,
        ]}>
        {rows.map((row, index) => (
          <Fragment key={row.key ?? index}>
            {index > 0 ? <View style={[styles.divider, { backgroundColor: colors.separator }]} /> : null}
            {row}
          </Fragment>
        ))}
      </View>
      {footer ? (
        <Text variant="footnote" tone="secondary" style={styles.footer}>
          {footer}
        </Text>
      ) : null}
    </View>
  );
}

export interface ListRowProps {
  title: string;
  subtitle?: string | null;
  /** Trailing value text ("System", "5 minutes"). */
  value?: string | null;
  leading?: ReactNode;
  /** A leading icon (shorthand for a 28 pt tinted icon). */
  icon?: IconName;
  /** A trailing control (switch, badge, check). */
  trailing?: ReactNode;
  onPress?: () => void;
  /** Destructive rows are danger-coloured and centred on iOS ("Sign out"). */
  destructive?: boolean;
  /** Marks the selected row of a choice list (check on iOS, the row reads as selected). */
  selected?: boolean;
  disabled?: boolean;
  accessibilityLabel?: string;
  accessibilityHint?: string;
  /** Swipe and long-press actions are mirrored here so VoiceOver/TalkBack can reach them (§4.11). */
  accessibilityActions?: AccessibilityActionInfo[];
  onAccessibilityAction?: (event: AccessibilityActionEvent) => void;
  /** Read as busy while an action on the row runs (a swipe's Delete waiting on the server). */
  busy?: boolean;
  onLongPress?: () => void;
  numberOfLines?: number;
  testID?: string;
}

/**
 * A list row (X12/X13): at least 44 pt / 48 dp (minHeight, so it grows with the text), a
 * highlight on iOS and a ripple on Android, a chevron only when the row navigates (iOS), and no
 * press state at all when it does nothing. At accessibility sizes the trailing value moves
 * under the title instead of squeezing it.
 */
export function ListRow({
  title,
  subtitle,
  value,
  leading,
  icon,
  trailing,
  onPress,
  destructive = false,
  selected,
  disabled = false,
  accessibilityLabel,
  accessibilityHint,
  accessibilityActions,
  onAccessibilityAction,
  busy,
  onLongPress,
  numberOfLines = 1,
  testID,
}: ListRowProps) {
  const design = useDesign();
  const { accessibilityLayout } = design;
  const tappable = onPress !== undefined || onLongPress !== undefined;
  const centred = destructive && uiPlatform === 'ios' && !leading && !icon && !trailing;
  const valueBelow = accessibilityLayout && value;
  const body = (
    <View style={[styles.row, { minHeight: Math.max(hitTarget(), 52) }]}>
      {leading ?? (icon ? <Icon name={icon} size={22} tone={destructive ? 'danger' : 'accent'} /> : null)}
      <View style={[styles.text, centred ? styles.centred : null]}>
        <Text variant="body" tone={destructive ? 'danger' : 'primary'} numberOfLines={accessibilityLayout ? undefined : numberOfLines}>
          {title}
        </Text>
        {subtitle ? (
          <Text variant="footnote" tone="secondary" numberOfLines={accessibilityLayout ? undefined : 2}>
            {subtitle}
          </Text>
        ) : null}
        {valueBelow ? (
          <Text variant="callout" tone="secondary">
            {value}
          </Text>
        ) : null}
      </View>
      {value && !valueBelow ? (
        <Text variant="body" tone="secondary" numberOfLines={1} style={styles.value}>
          {value}
        </Text>
      ) : null}
      {trailing}
      {selected ? <Icon name="check" size={20} tone="accent" weight="semibold" /> : null}
      {onPress && uiPlatform === 'ios' && !destructive && selected === undefined ? (
        <Icon name="chevronRight" size={14} tone="tertiary" weight="semibold" />
      ) : null}
    </View>
  );
  const a11y = {
    accessibilityLabel: accessibilityLabel ?? [title, subtitle, value].filter(Boolean).join(', '),
    accessibilityHint,
    accessibilityActions,
    onAccessibilityAction,
    testID,
  };
  if (!tappable) {
    return (
      <View accessible {...a11y} accessibilityState={selected !== undefined || busy ? { selected, busy } : undefined}>
        {body}
      </View>
    );
  }
  return (
    <Pressable
      accessibilityRole="button"
      {...a11y}
      accessibilityState={busy ? { disabled, selected, busy } : { disabled, selected }}
      disabled={disabled}
      onPress={onPress}
      onLongPress={onLongPress}
      android_ripple={ripple(design)}
      style={({ pressed }) => [pressed && uiPlatform === 'ios' ? { backgroundColor: pressedOverlay(design) } : null, disabled ? styles.disabled : null]}>
      {body}
    </Pressable>
  );
}

const styles = StyleSheet.create({
  section: { gap: 6 },
  sectionTitle: { paddingHorizontal: 16 },
  group: { overflow: 'hidden', borderCurve: 'continuous' },
  divider: { height: StyleSheet.hairlineWidth, marginStart: 16 },
  row: { flexDirection: 'row', alignItems: 'center', gap: 12, paddingHorizontal: 16, paddingVertical: 10 },
  text: { flex: 1, gap: 2 },
  centred: { alignItems: 'center' },
  value: { maxWidth: '45%' },
  footer: { paddingHorizontal: 16 },
  disabled: { opacity: 0.38 },
});
