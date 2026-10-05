import { useState } from 'react';
import { ActivityIndicator, Pressable, StyleSheet, Text, View } from 'react-native';

import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import type { ToolStep } from '@/features/chat/toolActivity';

/**
 * The tool calls of one answer, under its bubble: a collapsed row
 * ("Used 3 tools · 4.2s"), expanded (always while the answer streams) into
 * one chip per call, grouped under the sub-agent that ran it. A chip opens
 * the call's inputs and output.
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

const STATUS_MARK: Record<ToolStep['status'], string> = { running: '', done: '✓', error: '✗', needs_auth: '!', waiting: '⏸' };

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
  const palette = usePalette();
  const [expanded, setExpanded] = useState(false);
  if (steps.length === 0) return null;
  const open = expanded || streaming;
  const summary = toolsSummary(steps);
  return (
    <View style={styles.box}>
      <Pressable
        accessibilityRole="button"
        accessibilityLabel={summary}
        accessibilityState={{ expanded: open }}
        accessibilityHint={open ? 'Hides the tool calls' : 'Shows the tool calls'}
        disabled={streaming}
        onPress={() => setExpanded((v) => !v)}>
        <Text style={[styles.summary, { color: palette.textSecondary }]}>
          {summary} {streaming ? '' : open ? '▾' : '›'}
        </Text>
      </Pressable>
      {open
        ? groupByAgent(steps).map((group) => (
            <View key={group.agent ?? '-'} style={styles.group}>
              {group.agent ? (
                <Text style={[styles.agent, { color: palette.textSecondary }]} numberOfLines={1}>
                  {group.agent}
                </Text>
              ) : null}
              <View style={styles.chips}>
                {group.steps.map((step) => (
                  <Pressable
                    key={step.id}
                    accessibilityRole="button"
                    accessibilityLabel={`${toolLabel(step)}, ${STATUS_WORD[step.status]}`}
                    accessibilityHint="Shows the input and output"
                    onPress={() => onOpen(step)}
                    style={[
                      styles.chip,
                      {
                        borderColor: step.status === 'error' ? palette.danger : palette.border,
                        backgroundColor: palette.background,
                      },
                    ]}>
                    <Text numberOfLines={1} style={[styles.chipText, { color: palette.text }]}>
                      {toolLabel(step)}
                    </Text>
                    {step.status === 'running' ? (
                      <ActivityIndicator size="small" color={palette.accent} />
                    ) : (
                      <Text
                        style={[
                          styles.mark,
                          { color: step.status === 'error' || step.status === 'needs_auth' ? palette.danger : palette.accent },
                        ]}>
                        {STATUS_MARK[step.status]}
                      </Text>
                    )}
                  </Pressable>
                ))}
              </View>
            </View>
          ))
        : null}
    </View>
  );
}

const styles = StyleSheet.create({
  box: { gap: Spacing.one, marginTop: Spacing.one },
  summary: { fontSize: 13, fontWeight: '500' },
  group: { gap: 2 },
  agent: { fontSize: 12, fontWeight: '600' },
  chips: { flexDirection: 'row', flexWrap: 'wrap', gap: Spacing.one },
  chip: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: Spacing.one,
    borderWidth: StyleSheet.hairlineWidth,
    borderRadius: 12,
    paddingHorizontal: Spacing.two,
    paddingVertical: 3,
    maxWidth: 260,
  },
  chipText: { fontSize: 12, flexShrink: 1 },
  mark: { fontSize: 12, fontWeight: '700' },
});
