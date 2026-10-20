import { Redirect, Stack, useLocalSearchParams } from 'expo-router';
import { useState } from 'react';
import { FlatList, StyleSheet } from 'react-native';
import { KeyboardAvoidingView } from 'react-native-keyboard-controller';

import { DesignProvider, useDesign } from '@/design/DesignProvider';
import { GALLERY_BRANDS, parseGalleryParams } from '@/features/designGallery/DesignGallery';
import { Composer } from '@/features/chat/Composer';
import { MessageBubble } from '@/features/chat/MessageBubble';
import { headerActionOptions } from '@/ui';

interface Line {
  key: string;
  role: 'user' | 'assistant';
  text: string;
}

const SAMPLE: Line[] = [
  { key: '1', role: 'user', text: 'Can you summarise the launch plan?' },
  { key: '2', role: 'assistant', text: 'Sure. The launch is planned for November: beta in week 1, store review in week 2, and the public release in week 3.' },
];

/**
 * Development builds only: the conversation layout with the real `Composer` and the same
 * keyboard handling as `/c/[id]` (keyboard-controller `KeyboardAvoidingView` with
 * `automaticOffset`, the composer padded by the home-indicator inset when the keyboard is
 * down), so the composer can be checked against the keyboard without a server.
 * `?brand=zefir|magenta|green|lemon` previews a sample brand instead of the workspace's.
 */
export default function DesignComposerRoute() {
  const params = useLocalSearchParams<{ brand?: string }>();
  if (!__DEV__) return <Redirect href="/" />;
  if (params.brand === undefined) return <ComposerPreview />;
  return (
    <DesignProvider brand={GALLERY_BRANDS[parseGalleryParams(params).brand]}>
      <ComposerPreview />
    </DesignProvider>
  );
}

function ComposerPreview() {
  const design = useDesign();
  const [lines, setLines] = useState<Line[]>(SAMPLE);
  return (
    <KeyboardAvoidingView style={[styles.flex, { backgroundColor: design.colors.background }]} behavior="padding" automaticOffset>
      <Stack.Screen
        options={{
          title: 'Zefir launch planning',
          headerShown: true,
          ...headerActionOptions(
            [
              { id: 'export', icon: 'share', label: 'Export conversation', onPress: () => {} },
              { id: 'people', icon: 'people', label: 'Participants', onPress: () => {} },
            ],
            design,
          ),
        }}
      />
      <FlatList
        inverted
        data={[...lines].reverse()}
        keyExtractor={(line) => line.key}
        keyboardShouldPersistTaps="handled"
        contentContainerStyle={styles.list}
        renderItem={({ item }) => (
          <MessageBubble role={item.role} author={item.role === 'assistant' ? 'Zefir Helper' : null}>
            {item.text}
          </MessageBubble>
        )}
      />
      <Composer
        busy={false}
        onSend={({ text }) => {
          setLines((current) => [...current, { key: String(current.length + 1), role: 'user', text }]);
          return null;
        }}
      />
    </KeyboardAvoidingView>
  );
}

const styles = StyleSheet.create({ flex: { flex: 1 }, list: { paddingVertical: 12 } });
