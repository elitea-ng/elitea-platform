/** The modifier the shortcuts use, as the platform writes it: ⌘ on macOS, Ctrl+ elsewhere. */
export function modKey(): string {
  const platform = typeof navigator === 'undefined' ? '' : navigator.platform || navigator.userAgent;
  return /mac/i.test(platform) ? '⌘' : 'Ctrl+';
}
