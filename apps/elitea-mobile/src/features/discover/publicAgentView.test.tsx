import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react-native';
import { StyleSheet } from 'react-native';

import { SAMPLE_PUBLIC_AGENTS } from '@/features/designGallery/secondaryFixtures';
import { PublicAgentView } from '@/features/discover/PublicAgentView';

jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));

// No garbage-collection timers: nothing outlives the test.
const queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
afterAll(() => queries.clear());

it('the starter that is starting keeps full opacity with its spinner; the others are disabled (F4)', async () => {
  await render(
    <QueryClientProvider client={queries}>
      <PublicAgentView
        agent={SAMPLE_PUBLIC_AGENTS[0]}
        projectId={null}
        projectLabel="My project"
        welcome={null}
        starters={['Plan a trip', 'Pack a bag']}
        busy="starter:0"
        error={null}
        onStart={jest.fn()}
        onStarter={jest.fn()}
      />
    </QueryClientProvider>,
  );
  const running = screen.getByTestId('starter-0');
  expect(running.props.accessibilityState).toMatchObject({
    busy: true,
    disabled: false,
  });
  expect(StyleSheet.flatten(running.props.style)?.opacity).toBeUndefined();
  const other = screen.getByTestId('starter-1');
  expect(other.props.accessibilityState).toMatchObject({ disabled: true });
  expect(StyleSheet.flatten(other.props.style)).toMatchObject({
    opacity: 0.38,
  });
});
