/**
 * Sample data for the design gallery's tab preview (development builds only). Never sent
 * anywhere. Only the fields the real screens show: the previews render the same components as
 * the Chats and Settings tabs (`ConversationRow`, `SettingsList`), fed by these values (D2).
 */
export interface SampleChat {
  id: number;
  name: string;
  age: string;
}

export const SAMPLE_CHATS: readonly SampleChat[] = [
  { id: 1, name: 'Zefir launch planning', age: '9:41' },
  { id: 2, name: 'Q3 planning', age: 'Yesterday' },
  { id: 3, name: 'Film club charter', age: 'Mon' },
  { id: 4, name: 'Trip to Lisbon', age: 'Sep 28' },
  { id: 5, name: 'Onboarding checklist', age: 'Sep 21' },
  { id: 6, name: 'Release notes v1.60', age: 'Sep 14' },
];

export const SAMPLE_HOST = 'acme.elitea.ai';
export const SAMPLE_ORIGIN = `https://${SAMPLE_HOST}`;
export const SAMPLE_PROJECT_ID = 42;
