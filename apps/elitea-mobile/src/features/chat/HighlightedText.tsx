import { StyleSheet, Text, type TextStyle } from 'react-native';

import { highlightSpans } from '@/features/chat/mentions/mentions';
import { MONOSPACE_FONT } from '@/features/markdown/Markdown';
import { useShareOut } from '@/security/policy';

/**
 * A sent message with its tokens set off: `@Name` and `#Agent` in bold,
 * `~skill` and `/Toolkit/tool` in monospace. Display only (a heuristic over
 * the conversation's names); the plain text is still what is selectable (unless the workspace forbids
 * sharing out).
 */
export function HighlightedText({
  text,
  people,
  agents,
  style,
}: {
  text: string;
  people: readonly string[];
  agents: readonly string[];
  style: TextStyle | TextStyle[];
}) {
  const spans = highlightSpans(text, { people, agents });
  const selectable = useShareOut();
  return (
    <Text selectable={selectable} style={style}>
      {spans.map((span, index) =>
        span.kind === 'text' ? (
          span.text
        ) : (
          <Text key={index} style={span.kind === 'mention' || span.kind === 'agent' ? styles.name : styles.code}>
            {span.text}
          </Text>
        ),
      )}
    </Text>
  );
}

const styles = StyleSheet.create({
  name: { fontWeight: '700' },
  code: { fontFamily: MONOSPACE_FONT },
});
