/**
 * The one reading column a conversation sits in — Chat and a Local work
 * thread alike: the transcript and the composer share it, centred, so a line
 * stays readable on a wide window and both screens take the same room. The
 * scroll container stays full width (its scrollbar at the window's edge);
 * the column goes inside it.
 */
export const CONVERSATION_COLUMN = {
  width: '100%',
  maxWidth: '48rem',
  marginX: 'auto',
  paddingX: 3,
  boxSizing: 'border-box',
} as const;
