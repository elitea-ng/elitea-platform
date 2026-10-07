/**
 * Sample data for the design gallery's tab preview (development builds only). Never sent
 * anywhere. Only the fields the real screens show: the previews render the same components as
 * the Chats and Settings tabs (`ConversationRow`, `SettingsList`), fed by these values (D2).
 */
export interface SampleChat {
  id: number;
  name: string;
  /** Minutes before "now" the chat was last updated (the list groups by day). */
  minutesAgo: number;
}

export const SAMPLE_CHATS: readonly SampleChat[] = [
  { id: 1, name: 'Zefir launch planning', minutesAgo: 12 },
  { id: 2, name: 'Q3 planning', minutesAgo: 3 * 60 },
  { id: 3, name: 'Film club charter', minutesAgo: 26 * 60 },
  { id: 4, name: 'Trip to Lisbon', minutesAgo: 4 * 24 * 60 },
  { id: 5, name: 'Onboarding checklist', minutesAgo: 9 * 24 * 60 },
  { id: 6, name: 'Release notes v1.60', minutesAgo: 23 * 24 * 60 },
];

/** A sample chat as a server row (`updated_at`), relative to `now`. */
export function sampleChatRow(chat: SampleChat, now: number): { id: number; name: string; updated_at: string } {
  return { id: chat.id, name: chat.name, updated_at: new Date(now - chat.minutesAgo * 60_000).toISOString() };
}

/** The signed-in user of the previews. */
export const SAMPLE_USER_NAME = 'Alex Morgan';

export const SAMPLE_HOST = 'acme.elitea.ai';
export const SAMPLE_ORIGIN = `https://${SAMPLE_HOST}`;
export const SAMPLE_PROJECT_ID = 42;
export const SAMPLE_PROJECT_NAME = 'Zefir Team';
