import { useEffect, useState, type ReactNode } from 'react';
import {
  Animated,
  PanResponder,
  StyleSheet,
  Text,
  View,
  type AccessibilityActionEvent,
  type AccessibilityActionInfo,
  type AccessibilityState,
} from 'react-native';

import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';

/**
 * A list row that is removed with a swipe to the left (core PanResponder and
 * Animated, no gesture library). Past `SWIPE_COMMIT_FRACTION` of the row's
 * width, or a fast fling, the row slides out and `onDelete` runs; when that
 * resolves false (the server refused, the device is offline) the row slides
 * back. Shorter swipes spring back.
 *
 * Screen readers get the same thing as a named custom action
 * (`accessibilityActions`), so the delete never depends on a gesture. The
 * action reaches the row through `children` (a render prop) and is spread
 * onto the row's own accessible element: VoiceOver and TalkBack offer only the
 * focused element's actions, and a wrapper that is not itself an
 * accessibility element is never focused.
 * Vertical drags are left to the list, so scrolling still works.
 */

export const SWIPE_COMMIT_FRACTION = 0.35;
const FLING_VELOCITY = 0.8;
const START_SLOP = 12;

export interface SwipeToDeleteProps {
  /** The action's name for screen readers and the label behind the row ("Dismiss", "Delete"). */
  actionLabel: string;
  /** Removes the item; resolve false to keep the row. Absent: the row cannot be swiped (offline copy, older server). */
  onDelete: (() => Promise<boolean>) | null;
  testID?: string;
  /** Renders the row; spread `a11y` onto the row's accessible element (the one a screen reader focuses). */
  children: (a11y: SwipeRowA11y) => ReactNode;
}

/** What the focused row element carries so a screen reader can offer the delete. */
export interface SwipeRowA11y {
  accessibilityActions?: AccessibilityActionInfo[];
  onAccessibilityAction?: (event: AccessibilityActionEvent) => void;
  accessibilityState?: AccessibilityState;
}

interface SwipeController {
  offset: Animated.Value;
  width: number;
  busy: boolean;
  onDelete: (() => Promise<boolean>) | null;
  setBusy: (busy: boolean) => void;
  bind: (onDelete: (() => Promise<boolean>) | null, setBusy: (busy: boolean) => void) => void;
  setWidth: (width: number) => void;
  run: () => Promise<void>;
  responder: ReturnType<typeof PanResponder.create>;
}

/** The row's gesture state lives outside React state: it changes per frame, never per render. */
function createController(): SwipeController {
  const spring = () => Animated.spring(ctl.offset, { toValue: 0, useNativeDriver: true }).start();
  const ctl: SwipeController = {
    offset: new Animated.Value(0),
    width: 1,
    busy: false,
    onDelete: null,
    setBusy: () => {},
    bind: (onDelete, setBusy) => {
      ctl.onDelete = onDelete;
      ctl.setBusy = setBusy;
    },
    setWidth: (width) => {
      ctl.width = Math.max(1, width);
    },
    run: async () => {
      const remove = ctl.onDelete;
      if (remove === null || ctl.busy) return;
      ctl.busy = true;
      ctl.setBusy(true);
      let removed = false;
      try {
        removed = await remove();
      } catch {
        removed = false;
      }
      ctl.busy = false;
      ctl.setBusy(false);
      if (!removed) spring();
    },
    responder: PanResponder.create({
      onMoveShouldSetPanResponder: (_, g) =>
        ctl.onDelete !== null && !ctl.busy && g.dx < -START_SLOP && Math.abs(g.dx) > Math.abs(g.dy) * 1.5,
      onPanResponderMove: (_, g) => ctl.offset.setValue(Math.min(0, g.dx)),
      onPanResponderRelease: (_, g) => {
        const commit = -g.dx > ctl.width * SWIPE_COMMIT_FRACTION || g.vx < -FLING_VELOCITY;
        if (!commit) {
          spring();
          return;
        }
        Animated.timing(ctl.offset, { toValue: -ctl.width, duration: 160, useNativeDriver: true }).start(() => {
          void ctl.run();
        });
      },
      onPanResponderTerminate: spring,
    }),
  };
  return ctl;
}

export function SwipeToDelete({ actionLabel, onDelete, testID, children }: SwipeToDeleteProps) {
  const palette = usePalette();
  const [ctl] = useState(createController);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    ctl.bind(onDelete, setBusy);
  }, [ctl, onDelete]);

  const onAction = (event: AccessibilityActionEvent) => {
    if (event.nativeEvent.actionName === 'delete') void ctl.run();
  };

  const a11y: SwipeRowA11y = {
    accessibilityActions: onDelete !== null ? [{ name: 'delete', label: actionLabel }] : undefined,
    onAccessibilityAction: onDelete !== null ? onAction : undefined,
    accessibilityState: busy ? { busy: true } : undefined,
  };

  return (
    <View
      testID={testID}
      onLayout={(e) => {
        ctl.setWidth(e.nativeEvent.layout.width);
      }}>
      {onDelete !== null ? (
        <View style={[StyleSheet.absoluteFill, styles.behind, { backgroundColor: palette.danger }]} importantForAccessibility="no-hide-descendants">
          <Text style={styles.behindText}>{actionLabel}</Text>
        </View>
      ) : null}
      <Animated.View
        style={{ transform: [{ translateX: ctl.offset }], backgroundColor: palette.background }}
        {...(onDelete !== null ? ctl.responder.panHandlers : {})}>
        {children(a11y)}
      </Animated.View>
    </View>
  );
}

const styles = StyleSheet.create({
  behind: { alignItems: 'flex-end', justifyContent: 'center', paddingHorizontal: Spacing.three },
  behindText: { color: '#ffffff', fontWeight: '700', fontSize: 15 },
});
