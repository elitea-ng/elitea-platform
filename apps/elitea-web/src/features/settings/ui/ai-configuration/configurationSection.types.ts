/**
 * The one shared type between `ConfigurationSection` and the pieces split out
 * of it. It lives here, not in the component, because importing it back from
 * the parent closed an import cycle that `check-layer-cycle` fails.
 */
export interface AdditionalDefaultSetting {
  key?: string;
  label: React.ReactNode;
  labelWidth?: string;
  value: string;
  options: Array<{ value: string; label: string }>;
  onChange: (value: string) => void;
  /** Message from the last failed save of this select's default model. */
  error?: string | undefined;
}
