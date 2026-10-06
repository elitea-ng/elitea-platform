import * as Clipboard from 'expo-clipboard';
import { useState } from 'react';
import { ActivityIndicator, Pressable, ScrollView, StyleSheet, Text, View } from 'react-native';

import { useMessageTraceDetail, type PersistedToolStep } from '@/api/queries/messageTraces';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { useModalAnimation } from '@/features/a11y/motion';
import { toolLabel } from '@/features/chat/ToolCalls';
import { prettyJson, type ToolStep } from '@/features/chat/toolActivity';
import { MONOSPACE_FONT } from '@/features/markdown/Markdown';
import { useShareOut } from '@/security/policy';
import { LockAwareModal } from '@/security/lock/LockAwareModal';

type Tab = 'input' | 'output';

function isPersisted(step: ToolStep): step is PersistedToolStep {
  return typeof (step as Partial<PersistedToolStep>).traceId === 'number';
}

/**
 * One tool call's inputs and output, in a sheet: Input/Output tabs,
 * selectable monospace text (JSON pretty-printed), Copy (both only when the
 * workspace allows sharing out), and a banner when
 * the text was cut to the in-memory cap or the output is incomplete. A
 * persisted call's I/O is fetched only now (getMessageTrace).
 */
export function ToolDetailSheet({
  step,
  projectId,
  onClose,
}: {
  step: ToolStep | null;
  projectId: number | null;
  onClose: () => void;
}) {
  const animation = useModalAnimation('slide');
  return (
    <LockAwareModal visible={step !== null} animationType={animation} presentationStyle="pageSheet" onRequestClose={onClose}>
      {step ? <SheetBody step={step} projectId={projectId} onClose={onClose} /> : null}
    </LockAwareModal>
  );
}

function SheetBody({ step, projectId, onClose }: { step: ToolStep; projectId: number | null; onClose: () => void }) {
  const palette = usePalette();
  const [tab, setTab] = useState<Tab>(step.status === 'error' ? 'output' : 'input');
  const [copied, setCopied] = useState(false);
  const detail = useMessageTraceDetail(projectId, isPersisted(step) ? step : null);
  const remote = detail.data ?? null;

  const inputs = isPersisted(step) ? (remote?.inputs ?? null) : step.inputs;
  const inputsCut = isPersisted(step) ? (remote?.inputsTruncated ?? false) : step.inputsTruncated;
  const outputText = step.status === 'error' ? step.error : isPersisted(step) ? (remote?.output ?? null) : step.output;
  const outputCut = isPersisted(step) ? (remote?.outputTruncated ?? false) : step.outputTruncated;

  const text = tab === 'input' ? inputs : outputText;
  const shown = text === null ? null : prettyJson(text);
  const banners = [
    (tab === 'input' ? inputsCut : outputCut) ? 'Only the first 8 KB is shown.' : null,
    tab === 'output' && step.partial ? 'Part of this output did not arrive.' : null,
    tab === 'output' && step.status === 'running' ? 'The tool is still running.' : null,
    tab === 'output' && step.status === 'needs_auth' ? 'The tool has not run: its toolkit needs a sign-in.' : null,
    tab === 'output' && step.status === 'waiting'
      ? step.waitingFor === 'answer'
        ? 'The tool has not run: it waits for your answer.'
        : 'The tool has not run: it waits for your approval.'
      : null,
  ].filter((b): b is string => b !== null);
  const loading = isPersisted(step) && detail.isLoading;
  // allow_share_out off: no Copy, and no system selection menu either.
  const shareOut = useShareOut();
  const empty = loading ? null : detail.error ? "Couldn't load this tool call." : remote === null && isPersisted(step) ? 'Not available on this workspace.' : 'Nothing to show.';

  return (
    <View style={[styles.sheet, { backgroundColor: palette.background }]}>
      <View style={styles.header}>
        <Text accessibilityRole="header" numberOfLines={2} style={[styles.title, { color: palette.text }]}>
          {toolLabel(step)}
        </Text>
        <Pressable accessibilityRole="button" onPress={onClose} hitSlop={12}>
          <Text style={[styles.action, { color: palette.accent }]}>Done</Text>
        </Pressable>
      </View>
      {step.parentAgent ? (
        <Text style={[styles.sub, { color: palette.textSecondary }]}>Run by {step.parentAgent}</Text>
      ) : null}
      <View accessibilityRole="tablist" style={[styles.tabs, { borderColor: palette.border }]}>
        {(['input', 'output'] as Tab[]).map((name) => (
          <Pressable
            key={name}
            accessibilityRole="tab"
            accessibilityState={{ selected: tab === name }}
            onPress={() => {
              setTab(name);
              setCopied(false);
            }}
            style={[styles.tab, tab === name ? { backgroundColor: palette.backgroundElement } : null]}>
            <Text style={[styles.tabText, { color: palette.text, fontWeight: tab === name ? '700' : '400' }]}>
              {name === 'input' ? 'Input' : 'Output'}
            </Text>
          </Pressable>
        ))}
      </View>
      {banners.map((banner) => (
        <Text key={banner} accessibilityLiveRegion="polite" style={[styles.banner, { color: palette.textSecondary, borderColor: palette.border }]}>
          {banner}
        </Text>
      ))}
      {loading ? <ActivityIndicator style={styles.loading} color={palette.accent} /> : null}
      <ScrollView style={[styles.body, { borderColor: palette.border }]} contentContainerStyle={styles.bodyContent}>
        {shown !== null && shown !== '' ? (
          <Text selectable={shareOut} style={[styles.code, { color: palette.text }]}>
            {shown}
          </Text>
        ) : empty ? (
          <Text style={[styles.sub, { color: palette.textSecondary }]}>{empty}</Text>
        ) : null}
      </ScrollView>
      {shown && shareOut ? (
        <Pressable
          accessibilityRole="button"
          accessibilityLabel={tab === 'input' ? 'Copy input' : 'Copy output'}
          onPress={() => {
            void Clipboard.setStringAsync(shown).then(() => setCopied(true));
          }}
          style={[styles.copy, { borderColor: palette.border }]}>
          <Text style={[styles.action, { color: palette.accent }]}>{copied ? 'Copied' : 'Copy'}</Text>
        </Pressable>
      ) : null}
    </View>
  );
}

const styles = StyleSheet.create({
  sheet: { flex: 1, padding: Spacing.three, gap: Spacing.two },
  header: { flexDirection: 'row', alignItems: 'center', gap: Spacing.two },
  title: { flex: 1, fontSize: 17, fontWeight: '700' },
  sub: { fontSize: 13 },
  action: { fontSize: 16, fontWeight: '600' },
  tabs: { flexDirection: 'row', borderWidth: StyleSheet.hairlineWidth, borderRadius: 10, overflow: 'hidden' },
  tab: { flex: 1, alignItems: 'center', paddingVertical: Spacing.two },
  tabText: { fontSize: 14 },
  banner: { fontSize: 13, borderWidth: StyleSheet.hairlineWidth, borderRadius: 8, padding: Spacing.two },
  loading: { padding: Spacing.two },
  body: { flex: 1, borderWidth: StyleSheet.hairlineWidth, borderRadius: 8 },
  bodyContent: { padding: Spacing.two },
  code: { fontFamily: MONOSPACE_FONT, fontSize: 13, lineHeight: 18 },
  copy: { alignItems: 'center', paddingVertical: Spacing.two, borderWidth: StyleSheet.hairlineWidth, borderRadius: 10 },
});
