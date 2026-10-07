import { useState } from 'react';
import { Pressable, ScrollView, StyleSheet, Text, TextInput, View } from 'react-native';

import { Spacing } from '@/constants/theme';
import { FONT_SCALE } from '@/constants/typeScale';
import { usePalette } from '@/constants/use-palette';
import { useModalAnimation } from '@/features/a11y/motion';
import {
  AUTHORIZATION_CONTRACT,
  ContinueBodyError,
  HITL_CONTRACT,
  OUTPUT_LIMIT_CONTRACT,
  askUserQuestions,
  buildAnswerValue,
  buildAuthSkipBody,
  buildHitlContinueBody,
  buildOutputLimitBody,
  isAskUser,
  missingAnswers,
  type ChatContinueRequest,
  type ContinueContract,
  type HitlDecision,
  type PauseTarget,
} from '@/features/chat/pauses/continueBodies';
import type { HitlInterrupt, TurnPause } from '@/features/chat/toolActivity';
import { MONOSPACE_FONT } from '@/features/markdown/Markdown';
import { useShareOut } from '@/security/policy';
import { LockAwareModal } from '@/security/lock/LockAwareModal';
import { useDesign } from '@/design/DesignProvider';
import { Button as KitButton, haptic, Icon, ramp, type IconName } from '@/ui';

export interface PauseCardProps {
  pause: TurnPause;
  /** Null when the pause cannot be addressed (no conversation uuid or message id). */
  target: PauseTarget | null;
  offline: boolean;
  /** Sends the continuation; resolves to an error text, or null once it started. */
  onContinue: (contract: ContinueContract, body: ChatContinueRequest) => Promise<string | null>;
  /** Opens the conversation in the web app (system browser). */
  onOpenWeb: (() => void) | null;
  /** The workspace the tool runs on (its host), always said on an approval (§3.5). */
  host?: string | null;
}

const ACTION_LABEL: Record<string, string> = {
  approve: 'Approve',
  reject: 'Reject',
  edit: 'Edit',
  block_with_comment: 'Block with comment',
  answer: 'Answer',
};

/**
 * What a paused answer waits for, inline above the composer: an approval
 * (HITL), the answers to an ask_user question, a toolkit sign-in, or the
 * Continue offer of an answer cut at its output limit. Every action is
 * online only, disabled while it is in flight, and never queued.
 */
export function PauseCard(props: PauseCardProps) {
  switch (props.pause.kind) {
    case 'hitl':
      return <HitlCard {...props} pause={props.pause} />;
    case 'auth':
      return <AuthCard {...props} pause={props.pause} />;
    case 'output_limit':
      return <OutputLimitCard {...props} />;
  }
}

function useSubmit(props: PauseCardProps) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const submit = (contract: ContinueContract, build: (target: PauseTarget) => ChatContinueRequest) => {
    if (busy || props.offline || props.target === null) return;
    let body: ChatContinueRequest;
    try {
      body = build(props.target);
    } catch (e) {
      setError(e instanceof ContinueBodyError ? continueBodyText(e) : 'This answer cannot be sent.');
      return;
    }
    setBusy(true);
    setError(null);
    props
      .onContinue(contract, body)
      .then((problem) => setError(problem))
      .catch(() => setError('This answer could not be sent. Try again.'))
      .finally(() => setBusy(false));
  };
  return { busy, error, submit, disabled: busy || props.offline || props.target === null };
}

export function continueBodyText(error: ContinueBodyError): string {
  switch (error.reason) {
    case 'value':
      return 'Fill this in first.';
    case 'interrupt':
    case 'target':
      return 'This pause can only be answered in the web app.';
    default:
      return 'That choice is not offered here.';
  }
}

function Frame({
  title,
  children,
  note,
  icon = 'warning',
  host,
}: {
  title: string;
  children: React.ReactNode;
  note?: string | null;
  icon?: IconName;
  host?: string | null;
}) {
  const { colors, radius, space, elevation } = useDesign();
  // §4.7: a surface card with a warning leading edge, inline at the end of the transcript.
  // Never the mascot, never branded chrome: only the buttons take the (corrected) accent.
  return (
    <View
      accessibilityRole="summary"
      style={[
        styles.card,
        elevation.e1,
        {
          backgroundColor: colors.surface,
          borderColor: colors.separator,
          borderStartColor: colors.warning,
          borderRadius: radius.card,
          padding: space.cardPadding,
          marginHorizontal: space.screenGutterCompact,
        },
      ]}>
      <View style={styles.titleRow}>
        <View style={[styles.titleIcon, { backgroundColor: colors.warningSoft }]}>
          <Icon name={icon} size={18} tone="warning" />
        </View>
        <Text maxFontSizeMultiplier={FONT_SCALE.body} accessibilityRole="header" style={[styles.title, { color: colors.textPrimary }]}>
          {title}
        </Text>
      </View>
      {host ? (
        <Text maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.subtitle, { color: colors.textSecondary }]}>
          On {host}
        </Text>
      ) : null}
      {children}
      {note ? (
        <View style={[styles.noteRow, { backgroundColor: colors.dangerSoft, borderRadius: radius.sm }]}>
          <Icon name="error" size={16} tone="danger" />
          <Text maxFontSizeMultiplier={FONT_SCALE.body} accessibilityRole="alert" style={[styles.note, styles.flex, { color: colors.danger }]}>
            {note}
          </Text>
        </View>
      ) : null}
    </View>
  );
}

function Button({
  label,
  onPress,
  disabled,
  primary,
  danger,
}: {
  label: string;
  onPress: () => void;
  disabled?: boolean;
  primary?: boolean;
  danger?: boolean;
}) {
  return (
    <KitButton
      title={label}
      size="small"
      variant={primary ? 'filled' : danger ? 'destructiveTonal' : 'tonal'}
      disabled={disabled}
      onPress={onPress}
      style={styles.button}
    />
  );
}

function offlineNote(props: PauseCardProps): string | null {
  if (props.offline) return "You're offline.";
  if (props.target === null) return 'This pause can only be answered in the web app.';
  return null;
}

// ---------------------------------------------------------------------------

function HitlCard(props: PauseCardProps & { pause: Extract<TurnPause, { kind: 'hitl' }> }) {
  const { pause } = props;
  const { busy, error, submit, disabled } = useSubmit(props);
  const [decisions, setDecisions] = useState<Record<number, HitlDecision>>({});
  const several = pause.interrupts.length > 1;

  const decide = (index: number, decision: HitlDecision) => {
    if (!several) {
      submit(HITL_CONTRACT, (target) => buildHitlContinueBody(target, [decision]));
      return;
    }
    setDecisions((current) => ({ ...current, [index]: decision }));
  };
  const allDecided = pause.interrupts.every((_, i) => decisions[i] !== undefined);
  const title = several ? `${pause.interrupts.length} approvals needed` : (pause.interrupts[0]?.actionLabel ?? 'Approval needed');

  return (
    <Frame title={title} note={error ?? offlineNote(props)} icon="tool" host={props.host}>
      {pause.interrupts.map((interrupt, index) => (
        <InterruptBody
          key={interrupt.interruptId ?? index}
          interrupt={interrupt}
          disabled={disabled}
          decided={decisions[index] ?? null}
          onDecide={(decision) => decide(index, decision)}
          showTitle={several}
        />
      ))}
      {several ? (
        <Button
          label={busy ? 'Sending…' : 'Send decisions'}
          primary
          disabled={disabled || !allDecided}
          onPress={() => submit(HITL_CONTRACT, (target) => buildHitlContinueBody(target, pause.interrupts.map((_, i) => decisions[i])))}
        />
      ) : null}
    </Frame>
  );
}

function InterruptBody({
  interrupt,
  disabled,
  decided,
  onDecide,
  showTitle,
}: {
  interrupt: HitlInterrupt;
  disabled: boolean;
  decided: HitlDecision | null;
  onDecide: (decision: HitlDecision) => void;
  showTitle: boolean;
}) {
  const palette = usePalette();
  const [editor, setEditor] = useState<'edit' | 'block_with_comment' | null>(null);
  const subtitle = [interrupt.toolkitName, interrupt.toolName].filter(Boolean).join(' · ');
  return (
    <View style={styles.interrupt}>
      {showTitle ? (
        <Text maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.subtitle, { color: palette.text, fontWeight: '600' }]}>
          {interrupt.actionLabel ?? interrupt.toolName ?? 'Approval'}
          {interrupt.parentAgentName ? ` (${interrupt.parentAgentName})` : ''}
        </Text>
      ) : null}
      {subtitle ? <Text maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.subtitle, { color: palette.textSecondary }]}>{subtitle}</Text> : null}
      {interrupt.policyMessage ? <Text maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.body, { color: palette.textSecondary }]}>{interrupt.policyMessage}</Text> : null}
      {/* The Python worker sends the policy text as the message as well: say it once. */}
      {interrupt.message && !isAskUser(interrupt) && interrupt.message.trim() !== interrupt.policyMessage?.trim() ? (
        <Text maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.body, { color: palette.text }]}>{interrupt.message}</Text>
      ) : null}
      {interrupt.toolArgs ? <MaskedArgs args={interrupt.toolArgs} /> : null}
      {isAskUser(interrupt) ? (
        <AskUser interrupt={interrupt} disabled={disabled} onAnswer={(value) => onDecide({ interrupt, action: 'answer', value })} />
      ) : decided ? (
        <Text maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.subtitle, { color: palette.textSecondary }]}>Chosen: {ACTION_LABEL[decided.action] ?? decided.action}</Text>
      ) : (
        <View style={styles.buttons}>
          {interrupt.availableActions
            .filter((a) => a !== 'answer')
            .map((action) => (
              <Button
                key={action}
                label={ACTION_LABEL[action] ?? action}
                primary={action === 'approve'}
                danger={action === 'reject'}
                disabled={disabled}
                onPress={() => {
                  if (action === 'approve') haptic('approvalApprove');
                  if (action === 'reject') haptic('approvalReject');
                  if (action === 'edit' || action === 'block_with_comment') setEditor(action);
                  else onDecide({ interrupt, action });
                }}
              />
            ))}
        </View>
      )}
      <TextEditor
        mode={editor}
        initial={editor === 'edit' ? JSON.stringify(interrupt.toolArgs ?? {}, null, 2) : ''}
        jsonObject={editor === 'edit' && interrupt.toolArgs !== null}
        onCancel={() => setEditor(null)}
        onDone={(text) => {
          const action = editor;
          setEditor(null);
          if (action) onDecide({ interrupt, action, value: text });
        }}
      />
    </View>
  );
}

/** Tool arguments, masked until a value is tapped (they may be sensitive). */
export function MaskedArgs({ args }: { args: Record<string, unknown> }) {
  const palette = usePalette();
  const [open, setOpen] = useState(false);
  const [revealed, setRevealed] = useState<Record<string, boolean>>({});
  const shareOut = useShareOut();
  const keys = Object.keys(args).slice(0, 50);
  if (keys.length === 0) return null;
  return (
    <View>
      <Pressable accessibilityRole="button" accessibilityState={{ expanded: open }} onPress={() => setOpen((v) => !v)} style={styles.disclosure}>
        <Text maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.subtitle, { color: palette.accent }]}>
          Arguments ({keys.length})
        </Text>
        <Icon name={open ? 'chevronDown' : 'chevronRight'} size={12} color={palette.accent} weight="semibold" />
      </Pressable>
      {open
        ? keys.map((key) => {
            const raw = args[key];
            const text = typeof raw === 'string' ? raw : JSON.stringify(raw);
            return (
              <Pressable
                key={key}
                accessibilityRole="button"
                accessibilityLabel={revealed[key] ? `${key}: ${text}` : `${key}, hidden. Tap to show.`}
                onPress={() => setRevealed((r) => ({ ...r, [key]: !r[key] }))}>
                <Text maxFontSizeMultiplier={FONT_SCALE.body} selectable={shareOut && revealed[key]} style={[styles.arg, { color: palette.text }]}>
                  {key}: {revealed[key] ? text : '••••••'}
                </Text>
              </Pressable>
            );
          })
        : null}
    </View>
  );
}

function AskUser({ interrupt, disabled, onAnswer }: { interrupt: HitlInterrupt; disabled: boolean; onAnswer: (value: unknown) => void }) {
  const palette = usePalette();
  const questions = askUserQuestions(interrupt);
  const [picked, setPicked] = useState<Record<string, string[]>>({});
  const [typed, setTyped] = useState<Record<string, string>>({});
  const missing = missingAnswers(interrupt, picked, typed);
  const toggle = (key: string, label: string, multi: boolean) =>
    setPicked((current) => {
      const now = current[key] ?? [];
      const next = now.includes(label) ? now.filter((l) => l !== label) : multi ? [...now, label] : [label];
      return { ...current, [key]: next };
    });
  return (
    <View style={styles.interrupt}>
      {questions.map((q) => (
        <View key={q.key} style={styles.question}>
          <Text maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.body, { color: palette.text }]}>
            {q.question}
            {q.optional ? ' (optional)' : ''}
          </Text>
          {q.options.length > 0 ? (
            <View style={styles.chips}>
              {q.options.map((o) => {
                const on = (picked[q.key] ?? []).includes(o.label);
                return (
                  <Pressable
                    key={o.label}
                    accessibilityRole={q.multiSelect ? 'checkbox' : 'radio'}
                    accessibilityState={{ checked: on, disabled }}
                    accessibilityHint={o.description ?? undefined}
                    disabled={disabled}
                    onPress={() => toggle(q.key, o.label, q.multiSelect)}
                    style={[styles.chip, { borderColor: on ? palette.accent : palette.border, backgroundColor: on ? palette.accent : 'transparent' }]}>
                    <Text maxFontSizeMultiplier={FONT_SCALE.body} style={{ ...ramp.subheadline, color: on ? palette.onAccent : palette.text }}>{o.label}</Text>
                  </Pressable>
                );
              })}
            </View>
          ) : null}
          {q.allowOther ? (
            <TextInput
              accessibilityLabel={q.options.length > 0 ? `Other answer to: ${q.question}` : `Answer to: ${q.question}`}
              placeholder={q.options.length > 0 ? 'Other' : 'Your answer'}
              placeholderTextColor={palette.textSecondary}
              editable={!disabled}
              value={typed[q.key] ?? ''}
              onChangeText={(text) => setTyped((t) => ({ ...t, [q.key]: text }))}
              style={[styles.input, { color: palette.text, borderColor: palette.inputBorder }]}
            />
          ) : null}
        </View>
      ))}
      <Button
        label="Submit"
        primary
        disabled={disabled || missing.length > 0}
        onPress={() => onAnswer(buildAnswerValue(interrupt, picked, typed))}
      />
    </View>
  );
}

/** Full-screen text editor: the edited tool arguments (JSON object) or a block comment. */
function TextEditor({
  mode,
  initial,
  jsonObject,
  onCancel,
  onDone,
}: {
  mode: 'edit' | 'block_with_comment' | null;
  initial: string;
  jsonObject: boolean;
  onCancel: () => void;
  onDone: (text: string) => void;
}) {
  const animation = useModalAnimation('slide');
  return (
    <LockAwareModal visible={mode !== null} animationType={animation} presentationStyle="pageSheet" onRequestClose={onCancel}>
      {mode !== null ? <EditorBody mode={mode} initial={initial} jsonObject={jsonObject} onCancel={onCancel} onDone={onDone} /> : null}
    </LockAwareModal>
  );
}

/** The error of an edited-arguments text, or null when it is a JSON object. */
export function jsonObjectError(text: string): string | null {
  try {
    const parsed: unknown = JSON.parse(text);
    return parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed) ? null : 'The arguments must be a JSON object.';
  } catch {
    return 'This is not valid JSON.';
  }
}

function EditorBody({
  mode,
  initial,
  jsonObject,
  onCancel,
  onDone,
}: {
  mode: 'edit' | 'block_with_comment';
  initial: string;
  jsonObject: boolean;
  onCancel: () => void;
  onDone: (text: string) => void;
}) {
  const palette = usePalette();
  // The edit mode is prefilled with the agent's tool arguments: with share-out
  // off, the field still edits but offers no Select All / Copy / Share.
  const shareOut = useShareOut();
  const [text, setText] = useState(initial);
  const problem = text.trim() === '' ? 'Fill this in first.' : jsonObject ? jsonObjectError(text) : null;
  return (
    <View style={[styles.editor, { backgroundColor: palette.background }]}>
      <View style={styles.editorHeader}>
        <Pressable accessibilityRole="button" onPress={onCancel}>
          <Text maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.buttonText, { color: palette.accent }]}>Cancel</Text>
        </Pressable>
        <Text maxFontSizeMultiplier={FONT_SCALE.body} accessibilityRole="header" style={[styles.title, { color: palette.text }]}>
          {mode === 'edit' ? 'Edit' : 'Block with comment'}
        </Text>
        <Pressable accessibilityRole="button" accessibilityState={{ disabled: problem !== null }} disabled={problem !== null} onPress={() => onDone(text)}>
          <Text maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.buttonText, { color: palette.accent, opacity: problem ? 0.4 : 1 }]}>Send</Text>
        </Pressable>
      </View>
      <ScrollView keyboardShouldPersistTaps="handled" contentContainerStyle={styles.editorBody}>
        <TextInput
          accessibilityLabel={mode === 'edit' ? 'Edited value' : 'Comment'}
          multiline
          autoFocus
          autoCapitalize="none"
          autoCorrect={mode !== 'edit'}
          contextMenuHidden={!shareOut}
          value={text}
          onChangeText={setText}
          style={[styles.editorInput, { color: palette.text, borderColor: palette.inputBorder }, jsonObject ? { fontFamily: MONOSPACE_FONT } : null]}
        />
        {problem && text !== '' ? <Text maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.note, { color: palette.danger }]}>{problem}</Text> : null}
      </ScrollView>
    </View>
  );
}

// ---------------------------------------------------------------------------

function AuthCard(props: PauseCardProps & { pause: Extract<TurnPause, { kind: 'auth' }> }) {
  const palette = usePalette();
  const { pause } = props;
  const { busy, error, submit, disabled } = useSubmit(props);
  const names = [...new Set(pause.requests.map((r) => r.toolkitName ?? r.resourceName ?? 'A toolkit'))];
  const title = names.length === 1 ? `${names[0]} needs sign-in` : `${names.length} toolkits need sign-in`;
  return (
    <Frame title={title} note={error ?? offlineNote(props)} icon="lock" host={props.host}>
      <Text maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.body, { color: palette.textSecondary }]}>
        The tool has not run. Skip it to let the agent continue without it, or sign in from the web app.
      </Text>
      <View style={styles.buttons}>
        <Button
          label={busy ? 'Skipping…' : 'Skip'}
          primary
          disabled={disabled}
          onPress={() => submit(AUTHORIZATION_CONTRACT, (target) => buildAuthSkipBody(target, pause.requests))}
        />
        {props.onOpenWeb ? <Button label="Open in web" onPress={props.onOpenWeb} /> : null}
      </View>
    </Frame>
  );
}

function OutputLimitCard(props: PauseCardProps) {
  const palette = usePalette();
  const { busy, error, submit, disabled } = useSubmit(props);
  const label = props.pause.kind === 'output_limit' && props.pause.action ? props.pause.action : 'Continue';
  return (
    <Frame title="The answer reached its length limit" note={error ?? offlineNote(props)} icon="info">
      <Text maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.body, { color: palette.textSecondary }]}>Continue to get the rest of it.</Text>
      <Button label={busy ? 'Continuing…' : label} primary disabled={disabled} onPress={() => submit(OUTPUT_LIMIT_CONTRACT, buildOutputLimitBody)} />
    </Frame>
  );
}

const styles = StyleSheet.create({
  disclosure: { flexDirection: 'row', alignItems: 'center', gap: 4, minHeight: 44 },
  card: { borderWidth: StyleSheet.hairlineWidth, borderStartWidth: 3, gap: Spacing.two, marginVertical: Spacing.two, borderCurve: 'continuous' },
  titleRow: { flexDirection: 'row', alignItems: 'center', gap: Spacing.two },
  titleIcon: { width: 32, height: 32, borderRadius: 10, alignItems: 'center', justifyContent: 'center' },
  title: { ...ramp.headline, fontWeight: '600', flex: 1 },
  noteRow: { flexDirection: 'row', alignItems: 'flex-start', gap: 6, paddingHorizontal: 8, paddingVertical: 6 },
  flex: { flex: 1 },
  subtitle: { ...ramp.footnote },
  body: { ...ramp.subheadline },
  note: { ...ramp.footnote },
  interrupt: { gap: Spacing.one },
  question: { gap: Spacing.one },
  buttons: { flexDirection: 'row', flexWrap: 'wrap', gap: Spacing.two, marginTop: Spacing.one },
  button: { flexGrow: 1 },
  buttonText: { ...ramp.subheadline, fontWeight: '600' },
  chips: { flexDirection: 'row', flexWrap: 'wrap', gap: Spacing.one },
  chip: { borderWidth: 1, borderRadius: 16, paddingHorizontal: Spacing.two + 2, paddingVertical: Spacing.one + 2 },
  input: { ...ramp.subheadline, borderWidth: StyleSheet.hairlineWidth, borderRadius: 10, paddingHorizontal: Spacing.two, paddingVertical: Spacing.two },
  arg: { ...ramp.footnote, fontFamily: MONOSPACE_FONT, paddingVertical: 2 },
  editor: { flex: 1, padding: Spacing.three, gap: Spacing.two },
  editorHeader: { flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between' },
  editorBody: { gap: Spacing.two },
  editorInput: { ...ramp.subheadline, minHeight: 240, borderWidth: StyleSheet.hairlineWidth, borderRadius: 10, padding: Spacing.two, textAlignVertical: 'top' },
});
