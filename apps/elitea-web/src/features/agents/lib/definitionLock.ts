/** Whether the definition fields are locked: `definitionDisabled` when given, else the form's own `disabled`. */
export function definitionLock(definitionDisabled: boolean | undefined, disabled: boolean): boolean {
  return definitionDisabled ?? disabled;
}
