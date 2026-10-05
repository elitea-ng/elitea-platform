import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { Pressable, StyleSheet, Text, TextInput, View } from 'react-native';

import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import {
  EVERYONE,
  deriveMentions,
  detectTrigger,
  replaceRange,
  type ActiveTrigger,
  type DerivedMentions,
  type MentionCandidate,
  type Selection,
  type TriggerChar,
} from '@/features/chat/mentions/mentions';
import { SuggestionStrip, type Suggestion, type SuggestionState } from '@/features/chat/SuggestionStrip';

/** What the composer hands over on send. */
export interface ComposerSend {
  text: string;
  /** Derived from the final text; null when the conversation offers no one to mention. */
  mentions: DerivedMentions | null;
}

/** The answer to a pick from an outside source (`#` agent, `~` skill, `/` tool). */
export type PickResult = { insert: string } | { error: string };

export interface ComposerProps {
  /** Why sending is impossible; renders instead of the input. */
  disabledReason?: string | null;
  busy: boolean;
  /** Returns (or resolves to) an error text when the message was not accepted. */
  onSend: (input: ComposerSend) => string | null | Promise<string | null>;
  /** The other people in the conversation ("@"); "Everyone" is offered with them. */
  people?: readonly MentionCandidate[];
  /** A one-line note under the input (e.g. where mentions notify). */
  hint?: string | null;
  /** Triggers served from outside (the screen fetches their suggestions). */
  externalTriggers?: readonly TriggerChar[];
  /** Told whenever the outside trigger under the caret changes (null: none). */
  onTriggerChange?: (trigger: ActiveTrigger | null) => void;
  /** The suggestions for the current outside trigger. */
  externalSuggestions?: SuggestionState | null;
  /** A pick from an outside strip: what to put in place of the token, or why not. */
  onPickExternal?: (trigger: ActiveTrigger, item: Suggestion) => Promise<PickResult> | PickResult;
  /** Rendered above the input row (the target chip, the attachment tray). */
  accessory?: ReactNode;
  /** Rendered left of the input (the attachment button). */
  leading?: ReactNode;
  /** Sending is allowed with empty text (files attached). */
  allowEmptyText?: boolean;
}

/** The "@" rows for a query: Everyone first, then names starting with it, then names containing it. */
export function peopleSuggestions(people: readonly MentionCandidate[], query: string): Suggestion[] {
  const q = query.toLocaleLowerCase();
  const rows: Suggestion[] = [];
  if (people.length > 0 && EVERYONE.toLocaleLowerCase().startsWith(q)) {
    rows.push({ key: 'everyone', title: EVERYONE, subtitle: 'Notify everyone in this project', initial: '@' });
  }
  const named = people.filter((p) => p.name.trim() !== '');
  const starts = named.filter((p) => p.name.toLocaleLowerCase().startsWith(q));
  const contains = named.filter((p) => !starts.includes(p) && p.name.toLocaleLowerCase().includes(q));
  for (const p of [...starts, ...contains]) rows.push({ key: `user-${p.userId}`, title: p.name });
  return rows;
}

export function Composer({
  disabledReason,
  busy,
  onSend,
  people = [],
  hint,
  externalTriggers = [],
  onTriggerChange,
  externalSuggestions,
  onPickExternal,
  accessory,
  leading,
  allowEmptyText = false,
}: ComposerProps) {
  const palette = usePalette();
  const [text, setText] = useState('');
  const [selection, setSelection] = useState<Selection>({ start: 0, end: 0 });
  /** Set once after a pick to move the caret; released on the next selection event. */
  const [forcedSelection, setForcedSelection] = useState<Selection | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [picking, setPicking] = useState(false);
  /** A token whose pick failed; its strip stays hidden until the token changes. */
  const [dismissed, setDismissed] = useState<string | null>(null);

  const triggers = useMemo<TriggerChar[]>(
    () => [...(people.length > 0 ? (['@'] as TriggerChar[]) : []), ...externalTriggers],
    [people.length, externalTriggers],
  );
  const detected = detectTrigger(text, selection, triggers);
  const tokenKey = detected ? `${detected.start}:${detected.trigger}${detected.query}` : null;
  const active = detected && tokenKey !== dismissed ? detected : null;
  const external = active && active.trigger !== '@' ? active : null;
  const externalKey = external ? `${external.start}:${external.trigger}${external.query}` : null;

  useEffect(() => {
    onTriggerChange?.(external);
    // Re-announced only when the token itself changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [externalKey]);

  if (disabledReason) {
    return (
      <View style={[styles.bar, { borderTopColor: palette.border, backgroundColor: palette.background }]}>
        <Text style={[styles.notice, { color: palette.textSecondary }]}>{disabledReason}</Text>
      </View>
    );
  }

  const canSend = !busy && !submitting && !picking && (text.trim() !== '' || allowEmptyText);

  const settle = (sent: string, problem: string | null) => {
    setError(problem);
    // Only the accepted text is cleared; an edit made meanwhile is kept.
    if (problem === null) setText((current) => (current === sent ? '' : current));
  };
  const submit = () => {
    if (!canSend) return;
    const sent = text;
    const mentions = people.length > 0 ? deriveMentions(sent, people, { everyone: true }) : null;
    const result = onSend({ text: sent, mentions });
    if (!(result instanceof Promise)) {
      settle(sent, result);
      return;
    }
    setSubmitting(true);
    result
      .then((problem) => settle(sent, problem))
      .catch(() => settle(sent, 'This message could not be sent.'))
      .finally(() => setSubmitting(false));
  };

  const applyInsert = (trigger: ActiveTrigger, insert: string) => {
    const next = replaceRange(text, trigger.start, trigger.end, insert);
    setDismissed(null);
    setText(next.text);
    setSelection({ start: next.caret, end: next.caret });
    setForcedSelection({ start: next.caret, end: next.caret });
    setError(null);
  };

  const pick = (item: Suggestion) => {
    if (!active) return;
    if (active.trigger === '@') {
      const name = item.key === 'everyone' ? EVERYONE : item.title;
      applyInsert(active, `@${name} `);
      return;
    }
    if (!onPickExternal) return;
    const trigger = active;
    const outcome = onPickExternal(trigger, item);
    const finish = (result: PickResult) => {
      if ('error' in result) {
        setError(result.error);
        setDismissed(tokenKey);
      } else {
        applyInsert(trigger, result.insert);
      }
    };
    if (!(outcome instanceof Promise)) {
      finish(outcome);
      return;
    }
    setPicking(true);
    outcome
      .then(finish)
      .catch(() => finish({ error: 'That could not be added. Try again.' }))
      .finally(() => setPicking(false));
  };

  const stripState: SuggestionState | null = !active
    ? null
    : active.trigger === '@'
      ? { items: peopleSuggestions(people, active.query) }
      : (externalSuggestions ?? { items: [], loading: true });

  return (
    <View style={[styles.bar, { borderTopColor: palette.border, backgroundColor: palette.background }]}>
      {stripState ? (
        <SuggestionStrip
          state={picking ? { ...stripState, loading: true } : stripState}
          onPick={pick}
          label={active?.trigger === '@' ? 'People' : 'Suggestions'}
        />
      ) : null}
      {accessory}
      {error ? (
        <Text accessibilityRole="alert" style={[styles.error, { color: palette.danger }]}>
          {error}
        </Text>
      ) : null}
      <View style={styles.row}>
        {leading}
        <TextInput
          value={text}
          selection={forcedSelection}
          onChangeText={(value) => {
            // Typing at the end keeps the caret at the end; the platform's
            // selection event (when it comes) is the truth.
            if (selection.start === selection.end && selection.end === text.length) {
              setSelection({ start: value.length, end: value.length });
            }
            setText(value);
            setError(null);
          }}
          onSelectionChange={(event) => {
            setSelection(event.nativeEvent.selection);
            setForcedSelection(undefined);
          }}
          placeholder="Message"
          placeholderTextColor={palette.textSecondary}
          multiline
          accessibilityLabel="Message"
          style={[
            styles.input,
            { color: palette.text, backgroundColor: palette.backgroundElement, borderColor: palette.border },
          ]}
        />
        <Pressable
          accessibilityRole="button"
          accessibilityLabel="Send"
          accessibilityState={{ disabled: !canSend, busy }}
          disabled={!canSend}
          onPress={submit}
          style={[styles.send, { backgroundColor: palette.accent, opacity: canSend ? 1 : 0.4 }]}>
          <Text style={[styles.sendText, { color: palette.onAccent }]}>Send</Text>
        </Pressable>
      </View>
      {hint ? <Text style={[styles.hint, { color: palette.textSecondary }]}>{hint}</Text> : null}
    </View>
  );
}

const styles = StyleSheet.create({
  bar: { borderTopWidth: StyleSheet.hairlineWidth, padding: Spacing.two, gap: Spacing.one },
  row: { flexDirection: 'row', alignItems: 'flex-end', gap: Spacing.two },
  input: {
    flex: 1,
    minHeight: 40,
    maxHeight: 140,
    borderWidth: StyleSheet.hairlineWidth,
    borderRadius: 18,
    paddingHorizontal: Spacing.three - 4,
    paddingTop: Spacing.two,
    paddingBottom: Spacing.two,
    fontSize: 16,
  },
  send: { borderRadius: 18, paddingHorizontal: Spacing.three, height: 40, justifyContent: 'center' },
  sendText: { fontSize: 15, fontWeight: '600' },
  notice: { fontSize: 14, textAlign: 'center', padding: Spacing.two },
  error: { fontSize: 13 },
  hint: { fontSize: 12 },
});
