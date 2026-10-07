import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react-native';
import { useWindowDimensions } from 'react-native';

import { AnswerActions } from '@/features/chat/actions/AnswerActions';
import { planActions } from '@/features/chat/actions/messageActions';
import { SAMPLE_MESSAGES } from '@/features/designGallery/chatFixtures';

// The RN jest window reports an accessibility text size (font scale 2): the layout switch is on.
it('at accessibility text sizes the action row collapses to the "…" menu (§1.7)', async () => {
  const Probe = () => {
    const { fontScale } = useWindowDimensions();
    return fontScale >= 1.35 ? (
      <AnswerActions
        message={SAMPLE_MESSAGES[1]}
        plan={planActions(SAMPLE_MESSAGES[1], { shareOut: true, contract13: true, online: true, canSpeak: true, canRegenerate: true })}
        projectId={null}
        reading={false}
        onReadAloud={jest.fn()}
        onStopReading={jest.fn()}
        onRegenerate={jest.fn()}
        onMore={jest.fn()}
        onDislike={jest.fn()}
        onNote={jest.fn()}
      />
    ) : null;
  };
  await render(
    <QueryClientProvider client={new QueryClient()}>
      <Probe />
    </QueryClientProvider>,
  );
  expect(screen.getByRole('button', { name: 'Answer actions' })).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Copy answer' })).toBeNull();
  expect(screen.queryByRole('button', { name: 'Like' })).toBeNull();
});
