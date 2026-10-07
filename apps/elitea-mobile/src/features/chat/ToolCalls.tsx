import { useState } from 'react';
import { ActivityIndicator, Pressable, StyleSheet, View } from 'react-native';

import { FONT_SCALE } from '@/constants/typeScale';
import type { ToolStep } from '@/features/chat/toolActivity';
import { useDesign } from '@/design/DesignProvider';
import { Icon, pressedOverlay, ripple, Text, uiPlatform, type IconName, type TextTone } from '@/ui';

/**
 * The tool steps of one answer, inside its bubble (docs/design-system.md §4.7): a sunken card
 * whose disclosure row ("Used 3 tools · 4.2s", 44 pt) opens one row per call, grouped under
 * the sub-agent that ran it; expanded while the answer streams. A row shows the tool, its
 * status as a symbol AND a word (never colour alone) and opens the call's input and output.
 */

/**
 * "<toolkit> · <tool>". The frames' `metadata.display_name` is the TOOLKIT's
 * display name (the SDK sets it per toolkit), so it names the toolkit when
 * present; the tool is always named by `tool_name`, as the web app does.
 */
export function toolLabel(step: Pick<ToolStep, 'name' | 'displayName' | 'toolkit'>): string {
  const toolkit = step.displayName ?? step.toolkit;
  return toolkit ? `${toolkit} · ${step.name}` : step.name;
}

const STATUS_WORD: Record<ToolStep['status'], string> = {
  running: 'running',
  done: 'done',
  error: 'failed',
  needs_auth: 'needs sign-in',
  waiting: 'waiting for approval',
};

/** The status symbol of a finished or waiting step; a running one shows a spinner. Never colour alone: the label says it too. */
const STATUS_TONE: Record<Exclude<ToolStep['status'], 'running'>, TextTone> = {
  done: 'success',
  error: 'danger',
  needs_auth: 'warning',
  waiting: 'accent',
};

const STATUS_ICON: Record<Exclude<ToolStep['status'], 'running'>, IconName> = {
  done: 'checkCircle',
  error: 'errorCircle',
  needs_auth: 'warning',
  waiting: 'pauseCircle',
};

/** A step's status in words: a paused call says what it waits for. */
export function statusWord(step: Pick<ToolStep, 'status' | 'waitingFor'>): string {
  return step.status === 'waiting' && step.waitingFor === 'answer' ? 'waiting for your answer' : STATUS_WORD[step.status];
}

/** The wall time the calls covered (first start to last end), when known. */
export function toolsDurationMs(steps: readonly ToolStep[]): number | null {
  const starts = steps.map((s) => s.startedAt).filter((t): t is number => t !== null);
  const ends = steps.map((s) => s.endedAt).filter((t): t is number => t !== null);
  if (starts.length === 0 || ends.length === 0) return null;
  const span = Math.max(...ends) - Math.min(...starts);
  return span >= 0 ? span : null;
}

export function formatDuration(ms: number): string {
  if (ms < 1000) return `${Math.max(0, Math.round(ms))}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  const minutes = Math.floor(ms / 60_000);
  return `${minutes}m ${Math.round((ms % 60_000) / 1000)}s`;
}

export function toolsSummary(steps: readonly ToolStep[]): string {
  const count = steps.length === 1 ? 'Used 1 tool' : `Used ${steps.length} tools`;
  const ms = toolsDurationMs(steps);
  return ms === null ? count : `${count} · ${formatDuration(ms)}`;
}

/** Steps grouped by the sub-agent that ran them (null: the answering agent), first-seen order. */
export function groupByAgent(steps: readonly ToolStep[]): { agent: string | null; steps: ToolStep[] }[] {
  const groups: { agent: string | null; steps: ToolStep[] }[] = [];
  for (const step of steps) {
    const group = groups.find((g) => g.agent === step.parentAgent);
    if (group) group.steps.push(step);
    else groups.push({ agent: step.parentAgent, steps: [step] });
  }
  return groups;
}

export function ToolActivity({
  steps,
  streaming = false,
  onOpen,
}: {
  steps: readonly ToolStep[];
  streaming?: boolean;
  onOpen: (step: ToolStep) => void;
}) {
  const design = useDesign();
  const { colors, radius } = design;
  const [expanded, setExpanded] = useState(false);
  if (steps.length === 0) return null;
  const open = expanded || streaming;
  const summary = toolsSummary(steps);
  const running = steps.some((s) => s.status === 'running');
  const failed = steps.some((s) => s.status === 'error');
  const pressed = (on: boolean) => (on && uiPlatform === 'ios' ? { backgroundColor: pressedOverlay(design) } : null);
  return (
    <View style={[styles.card, { backgroundColor: colors.surfaceSunken, borderRadius: radius.md }]}>
      <Pressable
        accessibilityRole="button"
        accessibilityLabel={summary}
        accessibilityState={{ expanded: open }}
        accessibilityHint={open ? 'Hides the tool calls' : 'Shows the tool calls'}
        disabled={streaming}
        onPress={() => setExpanded((v) => !v)}
        android_ripple={ripple(design)}
        style={({ pressed: on }) => [styles.summaryRow, pressed(on)]}>
        {running ? (
          <ActivityIndicator size="small" color={colors.accent} />
        ) : (
          <Icon name={failed ? 'errorCircle' : 'tool'} size={18} tone={failed ? 'danger' : 'secondary'} />
        )}
        <Text variant="callout" tone="secondary" maxFontSizeMultiplier={FONT_SCALE.control} style={styles.flex}>
          {summary}
        </Text>
        {streaming ? null : <Icon name={open ? 'chevronDown' : 'chevronRight'} size={14} tone="tertiary" weight="semibold" />}
      </Pressable>
      {open
        ? groupByAgent(steps).map((group) => (
            <View key={group.agent ?? '-'} style={[styles.group, { borderTopColor: colors.separator }]}>
              {group.agent ? (
                <Text variant="caption" tone="secondary" maxFontSizeMultiplier={FONT_SCALE.control} numberOfLines={1} style={styles.agent}>
                  {group.agent}
                </Text>
              ) : null}
              {group.steps.map((step) => (
                <Pressable
                  key={step.id}
                  accessibilityRole="button"
                  accessibilityLabel={`${toolLabel(step)}, ${statusWord(step)}`}
                  accessibilityHint="Shows the input and output"
                  onPress={() => onOpen(step)}
                  android_ripple={ripple(design)}
                  style={({ pressed: on }) => [styles.step, pressed(on)]}>
                  {step.status === 'running' ? (
                    <ActivityIndicator size="small" color={colors.accent} />
                  ) : (
                    <Icon name={STATUS_ICON[step.status]} size={18} tone={STATUS_TONE[step.status]} />
                  )}
                  <View style={styles.flex}>
                    <Text variant="callout" maxFontSizeMultiplier={FONT_SCALE.control} numberOfLines={1}>
                      {toolLabel(step)}
                    </Text>
                    <Text variant="caption" tone={step.status === 'error' ? 'danger' : 'secondary'} maxFontSizeMultiplier={FONT_SCALE.control} numberOfLines={1}>
                      {statusWord(step)}
                    </Text>
                  </View>
                  <Icon name="chevronRight" size={14} tone="tertiary" weight="semibold" />
                </Pressable>
              ))}
            </View>
          ))
        : null}
    </View>
  );
}

const styles = StyleSheet.create({
  card: { overflow: 'hidden', marginTop: 2, borderCurve: 'continuous' },
  summaryRow: { flexDirection: 'row', alignItems: 'center', gap: 10, minHeight: 44, paddingHorizontal: 12, paddingVertical: 8 },
  group: { borderTopWidth: StyleSheet.hairlineWidth },
  agent: { fontWeight: '600', paddingHorizontal: 12, paddingTop: 8 },
  step: { flexDirection: 'row', alignItems: 'center', gap: 10, minHeight: 44, paddingHorizontal: 12, paddingVertical: 6 },
  flex: { flex: 1 },
});
