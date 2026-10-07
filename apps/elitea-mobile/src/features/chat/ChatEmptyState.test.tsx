import { render, screen } from '@testing-library/react-native';

import { ChatEmptyState } from '@/features/chat/ChatEmptyState';

let mockFontScale = 1;
jest.mock('react-native/Libraries/Utilities/useWindowDimensions', () => ({
  __esModule: true,
  default: () => ({ width: 393, height: 852, scale: 3, fontScale: mockFontScale }),
}));

const WELCOME = 'I find answers in your team GitHub, docs and tickets, and I can draft replies and summaries for you.';
const STARTER = 'Find every open bug assigned to me for this sprint and group them by component';

const draw = () =>
  render(<ChatEmptyState agentName="Research" iconUri={null} welcome={WELCOME} suggestions={[STARTER]} onSuggestion={jest.fn()} bottomInset={80} />);

describe('ChatEmptyState at large text sizes (HIG: no truncation)', () => {
  it('clamps the welcome and the starters at standard sizes, to keep the grid even', async () => {
    mockFontScale = 1;
    await draw();
    expect(screen.getByText(WELCOME).props.numberOfLines).toBe(6);
    expect(screen.getByText(STARTER).props.numberOfLines).toBe(3);
  });

  it('shows them in full at accessibility sizes', async () => {
    mockFontScale = 2;
    await draw();
    expect(screen.getByText(WELCOME).props.numberOfLines).toBeUndefined();
    expect(screen.getByText(STARTER).props.numberOfLines).toBeUndefined();
  });
});
