import { useState } from 'react';
import { Pressable, StyleSheet } from 'react-native';

import { FONT_SCALE } from '@/constants/typeScale';
import { useDesign } from '@/design/DesignProvider';
import { ActionMenu } from '@/features/chat/ActionMenu';
import type { AgentOption } from '@/features/chat/messageModel';
import { TURN_TAKING_COPY } from '@/features/chat/turnTakingCopy';
import { haptic, hitSlopFor, Icon, ripple, Text } from '@/ui';

/**
 * "To: Research ▾" in the composer when the conversation has more than one agent: who the next
 * message goes to (docs/design-system.md §4.5, the participants/target chip). A tap lists the
 * attached agents as a sheet of radio rows.
 *
 * `unchosen` (issue #1624): nothing decides who answers yet (several agents, no pick, no lead,
 * no answer), so the chip reads "Choose an agent" and no row is checked. `leadParticipantId`
 * marks the conversation's lead (ADR-0050 7d); the chip names it "(lead)" when it is the one
 * answering because of the floor. `open`/`onOpenChange` let the thread open the sheet itself
 * (a Send before an agent is chosen).
 */
export function TargetChip({
  current,
  choices,
  onChoose,
  unchosen = false,
  leadParticipantId = 0,
  viaLead = false,
  open: openProp,
  onOpenChange,
}: {
  current: AgentOption;
  choices: readonly AgentOption[];
  onChoose: (participantId: number) => void;
  unchosen?: boolean;
  leadParticipantId?: number;
  viaLead?: boolean;
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
}) {
  const design = useDesign();
  const { colors } = design;
  const [ownOpen, setOwnOpen] = useState(false);
  const open = openProp ?? ownOpen;
  const setOpen = (next: boolean) => {
    if (openProp === undefined) setOwnOpen(next);
    onOpenChange?.(next);
  };
  const name = (option: AgentOption) => option.name ?? `Agent ${option.participantId}`;
  const shown = unchosen ? TURN_TAKING_COPY.chooseAgent : viaLead ? TURN_TAKING_COPY.leadSuffix(name(current)) : name(current);
  return (
    <>
      <Pressable
        accessibilityRole="button"
        accessibilityLabel={unchosen ? shown : TURN_TAKING_COPY.to(shown)}
        accessibilityHint={TURN_TAKING_COPY.chooseAgentHint}
        onPress={() => setOpen(true)}
        hitSlop={hitSlopFor(32)}
        android_ripple={ripple(design)}
        style={[styles.chip, { backgroundColor: colors.accentSoft }]}>
        <Icon name="agent" size={14} tone="accentSoftOn" />
        {unchosen ? (
          <Text variant="footnote" tone="accentSoftOn" emphasized maxFontSizeMultiplier={FONT_SCALE.control} numberOfLines={1} style={styles.label}>
            {shown}
          </Text>
        ) : (
          <Text variant="footnote" tone="accentSoftOn" maxFontSizeMultiplier={FONT_SCALE.control} numberOfLines={1} style={styles.label}>
            To: <Text variant="footnote" tone="accentSoftOn" emphasized maxFontSizeMultiplier={FONT_SCALE.control}>{shown}</Text>
          </Text>
        )}
        <Icon name="chevronDown" size={12} tone="accentSoftOn" weight="semibold" />
      </Pressable>
      <ActionMenu
        visible={open}
        title={TURN_TAKING_COPY.sendTo}
        onClose={() => setOpen(false)}
        items={choices.map((item) => ({
          key: String(item.participantId),
          label: item.participantId === leadParticipantId ? TURN_TAKING_COPY.leadSuffix(name(item)) : name(item),
          icon: 'agent' as const,
          checked: !unchosen && item.participantId === current.participantId,
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
