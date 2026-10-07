import { useState } from 'react';
import { FlatList, Pressable, StyleSheet, Text, View } from 'react-native';

import { Spacing } from '@/constants/theme';
import { FONT_SCALE } from '@/constants/typeScale';
import { usePalette } from '@/constants/use-palette';
import type { AgentOption } from '@/features/chat/messageModel';
import { LockAwareModal } from '@/security/lock/LockAwareModal';
import { Icon, ramp } from '@/ui';

/**
 * "To: Research Agent ▾" above the composer when the conversation has more
 * than one agent: who the next message goes to. A tap lists the attached
 * agents to choose from.
 */
export function TargetChip({
  current,
  choices,
  onChoose,
}: {
  current: AgentOption;
  choices: readonly AgentOption[];
  onChoose: (participantId: number) => void;
}) {
  const palette = usePalette();
  const [open, setOpen] = useState(false);
  const name = (option: AgentOption) => option.name ?? `Agent ${option.participantId}`;
  return (
    <>
      <Pressable
        accessibilityRole="button"
        accessibilityLabel={`To: ${name(current)}`}
        accessibilityHint="Chooses which agent answers"
        onPress={() => setOpen(true)}
        style={[styles.chip, { borderColor: palette.border, backgroundColor: palette.backgroundElement }]}>
        <Text maxFontSizeMultiplier={FONT_SCALE.control} numberOfLines={1} style={[styles.chipText, { color: palette.text }]}>
          To: <Text maxFontSizeMultiplier={FONT_SCALE.control} style={styles.bold}>{name(current)}</Text>
        </Text>
        <Icon name="chevronDown" size={12} color={palette.textSecondary} weight="semibold" />
      </Pressable>
      <LockAwareModal visible={open} transparent animationType="fade" onRequestClose={() => setOpen(false)}>
        <Pressable accessibilityLabel="Close" style={styles.backdrop} onPress={() => setOpen(false)}>
          <View style={[styles.sheet, { backgroundColor: palette.background, borderColor: palette.border }]}>
            <Text maxFontSizeMultiplier={FONT_SCALE.control} accessibilityRole="header" style={[styles.title, { color: palette.text }]}>
              Send to
            </Text>
            <FlatList
              data={choices}
              keyExtractor={(item) => String(item.participantId)}
              renderItem={({ item }) => {
                const selected = item.participantId === current.participantId;
                return (
                  <Pressable
                    accessibilityRole="radio"
                    accessibilityState={{ checked: selected }}
                    onPress={() => {
                      onChoose(item.participantId);
                      setOpen(false);
                    }}
                    style={styles.row}>
                    <Text maxFontSizeMultiplier={FONT_SCALE.control} style={[styles.rowText, { color: palette.text, fontWeight: selected ? '700' : '400' }]}>{name(item)}</Text>
                    {selected ? <Icon name="check" size={18} color={palette.accent} weight="semibold" /> : null}
                  </Pressable>
                );
              }}
            />
          </View>
        </Pressable>
      </LockAwareModal>
    </>
  );
}

const styles = StyleSheet.create({
  chip: { alignSelf: 'flex-start', flexDirection: 'row', alignItems: 'center', gap: 4, minHeight: 32, borderWidth: StyleSheet.hairlineWidth, borderRadius: 16, paddingHorizontal: Spacing.two + 2, paddingVertical: 4, maxWidth: '100%' },
  chipText: { ...ramp.footnote },
  bold: { fontWeight: '700' },
  backdrop: { flex: 1, backgroundColor: 'rgba(0,0,0,0.35)', justifyContent: 'flex-end' },
  sheet: { borderTopLeftRadius: 16, borderTopRightRadius: 16, borderWidth: StyleSheet.hairlineWidth, padding: Spacing.three, maxHeight: '60%' },
  title: { ...ramp.headline, fontWeight: '700', marginBottom: Spacing.two },
  row: { flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between', paddingVertical: Spacing.two + 2 },
  rowText: { ...ramp.body, flex: 1 },
});
