// Money formatting. Amounts are dollars as floating-point numbers.

export function formatMoney(amount) {
  return `$${amount.toFixed(2)}`;
}
