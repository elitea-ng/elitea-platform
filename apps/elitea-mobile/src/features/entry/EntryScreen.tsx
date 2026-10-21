import type { ReactNode } from 'react';
import { ScrollView, StyleSheet, View } from 'react-native';
import { KeyboardAvoidingView } from 'react-native-keyboard-controller';
import { SafeAreaView } from 'react-native-safe-area-context';

import { MaxContentWidth } from '@/constants/theme';
import { useDesign } from '@/design/DesignProvider';
import { GradientBackground } from '@/ui';

/**
 * The frame of the signed-out screens (connect, confirm, sign-in; docs/screens.md §1): the
 * pastel gradient ground, safe areas on every edge (edge-to-edge on Android, the home indicator
 * on iOS), the keyboard lifting the content (react-native-keyboard-controller, the same insets
 * as the chat), and one centred column at most `MaxContentWidth` wide that scrolls when the
 * text is large. `footer` stays at the bottom of the screen when there is room; with
 * `pinFooter` it is a fixed bar under the scrolling content instead, so a primary action can never
 * be pushed below the fold (the confirm screen at accessibility sizes, EH-5). `back` is pinned to
 * the top leading edge, outside the centred column, where back navigation belongs (EH-6).
 */
export function EntryScreen({
  children,
  footer,
  pinFooter = false,
  back,
  testID,
}: {
  children: ReactNode;
  footer?: ReactNode;
  pinFooter?: boolean;
  back?: ReactNode;
  testID?: string;
}) {
  const { space, colors } = useDesign();
  const gutter = { paddingHorizontal: space.screenGutter };
  return (
    <GradientBackground testID={testID}>
      <SafeAreaView style={styles.flex} edges={['top', 'bottom', 'left', 'right']}>
        {back ? (
          <View testID="entry-back-bar" style={[styles.backBar, gutter]}>
            {back}
          </View>
        ) : null}
        <KeyboardAvoidingView style={styles.flex} behavior="padding">
          <ScrollView
            testID="entry-scroll"
            style={styles.flex}
            contentContainerStyle={[styles.content, gutter, back ? styles.contentUnderBack : null]}
            keyboardShouldPersistTaps="handled"
            keyboardDismissMode="interactive">
            <View style={[styles.column, styles.main]}>{children}</View>
            {footer && !pinFooter ? <View style={[styles.column, styles.footer]}>{footer}</View> : null}
          </ScrollView>
          {footer && pinFooter ? (
            <View testID="entry-pinned-footer" style={[styles.pinned, gutter, { borderTopColor: colors.separator }]}>
              <View style={[styles.column, styles.footer]}>{footer}</View>
            </View>
          ) : null}
        </KeyboardAvoidingView>
      </SafeAreaView>
    </GradientBackground>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  content: { flexGrow: 1, paddingVertical: 24, justifyContent: 'space-between', gap: 24 },
  column: { width: '100%', maxWidth: MaxContentWidth, alignSelf: 'center', gap: 16 },
  // The main column sits in the middle of the free height; the footer stays at the bottom.
  main: { flexGrow: 1, justifyContent: 'center' },
  footer: { gap: 8 },
  backBar: { flexDirection: 'row', alignItems: 'center' },
  contentUnderBack: { paddingTop: 8 },
  pinned: { paddingTop: 12, paddingBottom: 8, borderTopWidth: StyleSheet.hairlineWidth },
});
