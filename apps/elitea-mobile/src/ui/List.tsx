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
  /**
   * The row whose content is open beside the list (a split view's selection): read as selected,
   * no check. The caller draws the highlight (its own background).
   */
  current?: boolean;
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
  /** Under the title and subtitle, inside the row's text column (an agent's tags, F5). */
  footer?: ReactNode;
  /**
   * The iOS disclosure chevron. Default: shown when the row navigates (it has onPress, is not
   * destructive and is not a choice). Pass false for a row whose tap acts in place (Add, F3).
   */
  chevron?: boolean;
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
  selected: choice,
  current = false,
  disabled = false,
  accessibilityLabel,
  accessibilityHint,
  accessibilityActions,
  onAccessibilityAction,
  busy,
  onLongPress,
  numberOfLines = 1,
  footer,
  chevron,
  testID,
}: ListRowProps) {
  const design = useDesign();
  const { accessibilityLayout } = design;
  const tappable = onPress !== undefined || onLongPress !== undefined;
  const centred = destructive && uiPlatform === 'ios' && !leading && !icon && !trailing;
  const valueBelow = accessibilityLayout && value;
  const minHeight = { minHeight: Math.max(hitTarget(), 52) };
  const main = (
    <>
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
        {footer}
      </View>
      {value && !valueBelow ? (
        <Text variant="body" tone="secondary" numberOfLines={1} style={styles.value}>
          {value}
        </Text>
      ) : null}
    </>
  );
  const selected = choice ?? (current ? true : undefined);
  const check = choice ? <Icon name="check" size={20} tone="accent" weight="semibold" /> : null;
  const showChevron = chevron ?? (onPress !== undefined && !destructive && selected === undefined);
  const body = (
    <View style={[styles.row, minHeight]}>
      {main}
      {trailing}
      {check}
      {showChevron && uiPlatform === 'ios' ? <Icon name="chevronRight" size={14} tone="tertiary" weight="semibold" /> : null}
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
    const state = selected !== undefined || busy ? { selected, busy } : undefined;
    if (trailing) {
      // A trailing control (Remove, a switch) stays its own accessibility element (F1): an
      // accessible row is ONE element on iOS, and VoiceOver could never focus a button inside it.
      return (
        <View style={[styles.row, minHeight]}>
          <View accessible {...a11y} accessibilityState={state} style={styles.mainGroup}>
            {main}
          </View>
          {trailing}
          {check}
        </View>
      );
    }
    return (
      <View accessible {...a11y} accessibilityState={state}>
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
  mainGroup: { flex: 1, flexDirection: 'row', alignItems: 'center', gap: 12 },
  divider: { height: StyleSheet.hairlineWidth, marginStart: 16 },
  row: { flexDirection: 'row', alignItems: 'center', gap: 12, paddingHorizontal: 16, paddingVertical: 10 },
  text: { flex: 1, gap: 2 },
  centred: { alignItems: 'center' },
  value: { maxWidth: '45%' },
  footer: { paddingHorizontal: 16 },
  disabled: { opacity: 0.38 },
});
