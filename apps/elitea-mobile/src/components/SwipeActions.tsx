import { useEffect, useState, type ReactNode } from 'react';
import { Animated, PanResponder, StyleSheet, View, type AccessibilityActionEvent } from 'react-native';

import type { SwipeRowA11y } from '@/components/SwipeToDelete';
import { useDesign } from '@/design/DesignProvider';
import { haptic, Icon, Text, type IconName } from '@/ui';

/**
 * A list row with a swipe action on each side (docs/design-system.md §4.11), core PanResponder
 * and Animated like `SwipeToDelete`:
 * - `leading` (drag right; iOS only in the Chats list): a toggle such as Pin. Past the
 *   threshold the row springs back and the action runs; the row stays.
 * - `trailing` (drag left): a removal such as Delete. Past the threshold the row slides out
 *   and the action runs; it resolves false when nothing was removed (the user cancelled the
 *   confirmation, the action failed) and the row slides back.
 * Crossing the threshold plays the swipe haptic once (§1.10). Vertical drags stay with the list.
 *
 * Every swipe action, plus `extra` actions (Rename), is also a named accessibility action on
 * the row's own accessible element (through `children`), because swipes are invisible to
 * VoiceOver and TalkBack; the same actions are in the row's long-press menu.
 */

export interface RowAction {
  /** The accessibility action name (`pin`, `rename`, `delete`). */
  name: string;
  /** What VoiceOver/TalkBack and the swipe background say ("Pin", "Unpin", "Delete"). */
  label: string;
  icon: IconName;
  /** Runs the action; a trailing (removing) action resolves true when the row went away. */
  run: () => Promise<boolean> | void;
}

export const SWIPE_ACTION_FRACTION = 0.3;
const FLING_VELOCITY = 0.8;
const START_SLOP = 12;

interface Controller {
  offset: Animated.Value;
  leadOpacity: Animated.AnimatedInterpolation<number>;
  trailOpacity: Animated.AnimatedInterpolation<number>;
  width: number;
  busy: boolean;
  armed: boolean;
  leading: RowAction | null;
  trailing: RowAction | null;
  setBusy: (busy: boolean) => void;
  bind: (leading: RowAction | null, trailing: RowAction | null, setBusy: (busy: boolean) => void) => void;
  setWidth: (width: number) => void;
  responder: ReturnType<typeof PanResponder.create>;
  runTrailing: () => Promise<void>;
  runLeading: () => Promise<void>;
}

function createController(): Controller {
  const spring = () => Animated.spring(ctl.offset, { toValue: 0, useNativeDriver: true }).start();
  const past = (dx: number) => Math.abs(dx) > ctl.width * SWIPE_ACTION_FRACTION;
  const offset = new Animated.Value(0);
  const ctl: Controller = {
    offset,
    leadOpacity: offset.interpolate({ inputRange: [0, 1], outputRange: [0, 1], extrapolate: 'clamp' }),
    trailOpacity: offset.interpolate({ inputRange: [-1, 0], outputRange: [1, 0], extrapolate: 'clamp' }),
    width: 1,
    busy: false,
    armed: false,
    leading: null,
    trailing: null,
    setBusy: () => {},
    bind: (leading, trailing, setBusy) => {
      ctl.leading = leading;
      ctl.trailing = trailing;
      ctl.setBusy = setBusy;
    },
    setWidth: (width) => {
      ctl.width = Math.max(1, width);
    },
    runLeading: async () => {
      const action = ctl.leading;
      if (action === null || ctl.busy) return;
      ctl.busy = true;
      ctl.setBusy(true);
      try {
        await action.run();
      } catch {
        // The action reports its own failure.
      }
      ctl.busy = false;
      ctl.setBusy(false);
    },
    runTrailing: async () => {
      const action = ctl.trailing;
      if (action === null || ctl.busy) return;
      ctl.busy = true;
      ctl.setBusy(true);
      let removed = false;
      try {
        removed = (await action.run()) === true;
      } catch {
        removed = false;
      }
      ctl.busy = false;
      ctl.setBusy(false);
      if (!removed) spring();
    },
    responder: PanResponder.create({
      onMoveShouldSetPanResponder: (_, g) =>
        !ctl.busy &&
        Math.abs(g.dx) > Math.abs(g.dy) * 1.5 &&
        ((g.dx < -START_SLOP && ctl.trailing !== null) || (g.dx > START_SLOP && ctl.leading !== null)),
      onPanResponderGrant: () => {
        ctl.armed = false;
      },
      onPanResponderMove: (_, g) => {
        const dx = g.dx < 0 ? (ctl.trailing ? g.dx : 0) : ctl.leading ? g.dx : 0;
        ctl.offset.setValue(dx);
        const armed = past(dx);
        if (armed && !ctl.armed) haptic('swipeActionThreshold');
        ctl.armed = armed;
      },
      onPanResponderRelease: (_, g) => {
        const toTrailing = ctl.trailing !== null && g.dx < 0 && (past(g.dx) || g.vx < -FLING_VELOCITY);
        const toLeading = ctl.leading !== null && g.dx > 0 && (past(g.dx) || g.vx > FLING_VELOCITY);
        if (toTrailing) {
          Animated.timing(ctl.offset, { toValue: -ctl.width, duration: 160, useNativeDriver: true }).start(() => {
            void ctl.runTrailing();
          });
          return;
        }
        spring();
        if (toLeading) void ctl.runLeading();
      },
      onPanResponderTerminate: spring,
    }),
  };
  return ctl;
}

export function SwipeActions({
  leading,
  trailing,
  extra = [],
  testID,
  children,
}: {
  leading?: RowAction | null;
  trailing?: RowAction | null;
  /** Actions with no swipe (Rename): accessibility actions only, next to the swipe ones. */
  extra?: readonly RowAction[];
  testID?: string;
  children: (a11y: SwipeRowA11y) => ReactNode;
}) {
  const { colors } = useDesign();
  const [ctl] = useState(createController);
  const [busy, setBusy] = useState(false);
  const lead = leading ?? null;
  const trail = trailing ?? null;
  useEffect(() => {
    ctl.bind(lead, trail, setBusy);
  }, [ctl, lead, trail]);

  const all = [lead, ...extra, trail].filter((a): a is RowAction => a !== null);
  const onAction = (event: AccessibilityActionEvent) => {
    const name = event.nativeEvent.actionName;
    if (ctl.busy) return;
    if (trail && name === trail.name) void ctl.runTrailing();
    else if (lead && name === lead.name) void ctl.runLeading();
    else void extra.find((a) => a.name === name)?.run();
  };
  const a11y: SwipeRowA11y = {
    accessibilityActions: all.length > 0 ? all.map((a) => ({ name: a.name, label: a.label })) : undefined,
    onAccessibilityAction: all.length > 0 ? onAction : undefined,
    accessibilityState: busy ? { busy: true } : undefined,
  };
  const swipeable = lead !== null || trail !== null;
  return (
    <View testID={testID} onLayout={(e) => ctl.setWidth(e.nativeEvent.layout.width)}>
      {lead ? (
        // Pin: the `secondarySoft` / `secondarySoftOn` pair (a proven 4.5:1 pair, §2.1), shown
        // only while the row moves right.
        <Animated.View
          style={[StyleSheet.absoluteFill, styles.behind, styles.start, { backgroundColor: colors.secondarySoft, opacity: ctl.leadOpacity }]}
          importantForAccessibility="no-hide-descendants"
          accessibilityElementsHidden>
          <Icon name={lead.icon} size={20} color={colors.secondarySoftOn} />
          <Text variant="subheadline" color={colors.secondarySoftOn} style={styles.behindText}>
            {lead.label}
          </Text>
        </Animated.View>
      ) : null}
      {trail ? (
        <Animated.View
          style={[StyleSheet.absoluteFill, styles.behind, styles.end, { backgroundColor: colors.danger, opacity: ctl.trailOpacity }]}
          importantForAccessibility="no-hide-descendants"
          accessibilityElementsHidden>
          <Icon name={trail.icon} size={20} tone="dangerOn" />
          <Text variant="subheadline" tone="dangerOn" style={styles.behindText}>
            {trail.label}
          </Text>
        </Animated.View>
      ) : null}
      <Animated.View style={{ transform: [{ translateX: ctl.offset }], backgroundColor: colors.surface }} {...(swipeable ? ctl.responder.panHandlers : {})}>
        {children(a11y)}
      </Animated.View>
    </View>
  );
}

const styles = StyleSheet.create({
  behind: { flexDirection: 'row', alignItems: 'center', gap: 6, paddingHorizontal: 20 },
  start: { justifyContent: 'flex-start' },
  end: { justifyContent: 'flex-end' },
  behindText: { fontWeight: '600' },
});
