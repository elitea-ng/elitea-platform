import * as Clipboard from 'expo-clipboard';
import { LinearGradient } from 'expo-linear-gradient';
import { useEffect, useRef, useState, type ComponentRef } from 'react';
import { I18nManager, ScrollView, StyleSheet, View, type NativeScrollEvent, type NativeSyntheticEvent } from 'react-native';

import { FONT_SCALE } from '@/constants/typeScale';
import { rgb } from '@/design/contrast';
import { useDesign } from '@/design/DesignProvider';
import { formatColor } from '@/theme/color';
import { haptic, IconButton, ramp, Text } from '@/ui';

import { InlineCaret } from './InlineCaret';
import { MONOSPACE_FONT } from './monospace';

/**
 * A fenced code block in an answer (docs/design-system.md §4.6): a sunken card with the
 * language over the code, set in the code role, scrolling sideways instead of wrapping, and a
 * copy button when the workspace allows copying out (`allow_share_out`); without it the code
 * is neither copyable nor selectable. A line wider than the card shows that it goes on: the
 * scroll indicator (flashed when the overflow first appears) and a fade at the trailing edge
 * that clears once the end is in view.
 */
export function CodeBlock({
  code,
  language,
  selectable,
  caret = false,
}: {
  code: string;
  language: string | null;
  selectable: boolean;
  /** The answer is still streaming inside this block: the caret follows the code (never copied). */
  caret?: boolean;
}) {
  const { colors, radius } = useDesign();
  const [copied, setCopied] = useState(false);
  const [box, setBox] = useState(0);
  const [content, setContent] = useState(0);
  const [atEnd, setAtEnd] = useState(false);
  const scroller = useRef<ComponentRef<typeof ScrollView>>(null);
  const text = code.replace(/\n$/, '');
  const overflows = content > box + 1 && box > 0;
  useEffect(() => {
    if (overflows) scroller.current?.flashScrollIndicators();
  }, [overflows]);
  const onScroll = (event: NativeSyntheticEvent<NativeScrollEvent>) => {
    const { contentOffset, contentSize, layoutMeasurement } = event.nativeEvent;
    setAtEnd(contentOffset.x + layoutMeasurement.width >= contentSize.width - 2);
  };
  return (
    <View testID="markdown-code-block" style={[styles.card, { backgroundColor: colors.surfaceSunken, borderColor: colors.separator, borderRadius: radius.sm + 2 }]}>
      <View style={styles.header}>
        <Text variant="caption" tone="secondary" maxFontSizeMultiplier={FONT_SCALE.control} style={styles.language}>
          {language ?? 'Code'}
        </Text>
        {selectable ? (
          <IconButton
            icon={copied ? 'check' : 'copy'}
            variant="plain"
            size={32}
            iconSize={16}
            iconColor={colors.textSecondary}
            accessibilityLabel={language ? `Copy ${language} code block` : 'Copy code block'}
            onPress={() => {
              void Clipboard.setStringAsync(text)
                .then(() => {
                  haptic('copy');
                  setCopied(true);
                  setTimeout(() => setCopied(false), 1500);
                })
                .catch(() => {});
            }}
          />
        ) : null}
      </View>
      <ScrollView
        ref={scroller}
        horizontal
        testID="markdown-code-scroll"
        showsHorizontalScrollIndicator={overflows}
        onLayout={(event) => setBox(event.nativeEvent.layout.width)}
        onContentSizeChange={(width) => setContent(width)}
        onScroll={onScroll}
        scrollEventThrottle={64}
        accessibilityHint={overflows ? 'Some lines are wider than the screen; swipe sideways to read them' : undefined}
        contentContainerStyle={styles.scroll}>
        <Text selectable={selectable} maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.code, { color: colors.textPrimary }]}>
          {text}
          {caret ? <InlineCaret /> : null}
        </Text>
      </ScrollView>
      {overflows && !atEnd ? (
        <LinearGradient
          testID="markdown-code-fade"
          pointerEvents="none"
          colors={[formatColor({ ...rgb(colors.surfaceSunken), a: 0 }), colors.surfaceSunken]}
          start={{ x: I18nManager.isRTL ? 1 : 0, y: 0 }}
          end={{ x: I18nManager.isRTL ? 0 : 1, y: 0 }}
          style={styles.fade}
        />
      ) : null}
    </View>
  );
}

const styles = StyleSheet.create({
  card: { borderWidth: StyleSheet.hairlineWidth, overflow: 'hidden', marginVertical: 4, borderCurve: 'continuous' },
  header: { flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between', paddingStart: 12, minHeight: 36 },
  language: { fontWeight: '600', textTransform: 'lowercase' },
  scroll: { paddingHorizontal: 12, paddingBottom: 12 },
  code: { ...ramp.code, fontFamily: MONOSPACE_FONT },
  fade: { position: 'absolute', end: 0, top: 36, bottom: 0, width: 32 },
});
