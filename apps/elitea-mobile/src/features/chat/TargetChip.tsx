import { useState } from 'react';
import { Pressable, StyleSheet } from 'react-native';

import { FONT_SCALE } from '@/constants/typeScale';
import { useDesign } from '@/design/DesignProvider';
import { ActionMenu } from '@/features/chat/ActionMenu';
import type { AgentOption } from '@/features/chat/messageModel';
import { haptic, hitSlopFor, Icon, ripple, Text } from '@/ui';

/**
 * "To: Research ▾" in the composer when the conversation has more than one agent: who the next
 * message goes to (docs/design-system.md §4.5, the participants/target chip). A tap lists the
 * attached agents as a sheet of radio rows.
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
  const design = useDesign();
  const { colors } = design;
  const [open, setOpen] = useState(false);
  const name = (option: AgentOption) => option.name ?? `Agent ${option.participantId}`;
  return (
    <>
      <Pressable
        accessibilityRole="button"
        accessibilityLabel={`To: ${name(current)}`}
        accessibilityHint="Chooses which agent answers"
        onPress={() => setOpen(true)}
        hitSlop={hitSlopFor(32)}
        android_ripple={ripple(design)}
        style={[styles.chip, { backgroundColor: colors.accentSoft }]}>
        <Icon name="agent" size={14} tone="accentSoftOn" />
        <Text variant="footnote" tone="accentSoftOn" maxFontSizeMultiplier={FONT_SCALE.control} numberOfLines={1} style={styles.label}>
          To: <Text variant="footnote" tone="accentSoftOn" emphasized maxFontSizeMultiplier={FONT_SCALE.control}>{name(current)}</Text>
        </Text>
        <Icon name="chevronDown" size={12} tone="accentSoftOn" weight="semibold" />
      </Pressable>
      <ActionMenu
        visible={open}
        title="Send to"
        onClose={() => setOpen(false)}
        items={choices.map((item) => ({
          key: String(item.participantId),
          label: name(item),
          icon: 'agent' as const,
          checked: item.participantId === current.participantId,
          onPress: () => {
            haptic('tabOrChipChange');
            onChoose(item.participantId);
            setOpen(false);
          },
        }))}
      />
    </>
  );
}

const styles = StyleSheet.create({
  chip: { alignSelf: 'flex-start', flexDirection: 'row', alignItems: 'center', gap: 6, minHeight: 32, borderRadius: 16, paddingHorizontal: 12, maxWidth: '100%' },
  label: { flexShrink: 1 },
});
