import { useState } from 'react';
import { StyleSheet, TextInput, View, type TextInputProps } from 'react-native';

import { useDesign } from '@/design/DesignProvider';

import { Icon } from './Icon';
import { hitTarget } from './platform';
import { Text } from './Text';
import { maxScale, ramp } from './type';

export interface TextFieldProps extends Omit<TextInputProps, 'style'> {
  /** The visible label above the field; also its accessibility label. */
  label: string;
  /** A line under the field: the error (danger, with an icon, announced) wins over the note. */
  error?: string | null;
  note?: string | null;
  noteTestID?: string;
}

/**
 * The base text field (§4.13): a `surfaceSunken` fill, a 1 px `separatorStrong` border (3:1), a
 * 2 pt `focus` border while focused, `danger` with an icon on error. The label sits above it (iOS
 * grouped-form style and the Material 3 outlined field's label), the field grows with the text
 * size (minHeight, not height) and keeps the 44/48 target.
 */
export function TextField({ label, error, note, noteTestID, onFocus, onBlur, ...rest }: TextFieldProps) {
  const { colors, radius } = useDesign();
  const [focused, setFocused] = useState(false);
  const border = error ? colors.danger : focused ? colors.focus : colors.separatorStrong;
  return (
    <View style={styles.wrap}>
      <Text variant="footnote" tone="secondary" emphasized importantForAccessibility="no" accessibilityElementsHidden>
        {label}
      </Text>
      <TextInput
        accessibilityLabel={label}
        placeholderTextColor={colors.textTertiary}
        maxFontSizeMultiplier={maxScale.body}
        onFocus={(event) => {
          setFocused(true);
          onFocus?.(event);
        }}
        onBlur={(event) => {
          setFocused(false);
          onBlur?.(event);
        }}
        style={[
          styles.input,
          {
            minHeight: hitTarget() + 4,
            color: colors.textPrimary,
            backgroundColor: colors.surfaceSunken,
            borderColor: border,
            borderWidth: focused || error ? 2 : 1,
            borderRadius: radius.sm,
          },
        ]}
        {...rest}
      />
      {error ? (
        <View style={styles.note}>
          <Icon name="error" size={16} tone="danger" />
          <Text accessibilityRole="alert" variant="footnote" tone="danger" style={styles.noteText}>
            {error}
          </Text>
        </View>
      ) : note ? (
        <Text testID={noteTestID} variant="footnote" tone="secondary">
          {note}
        </Text>
      ) : null}
    </View>
  );
}

const styles = StyleSheet.create({
  wrap: { gap: 6, alignSelf: 'stretch' },
  input: { ...ramp.body, paddingHorizontal: 14, paddingVertical: 10, borderCurve: 'continuous' },
  note: { flexDirection: 'row', alignItems: 'flex-start', gap: 6 },
  noteText: { flex: 1 },
});
