// Discounts. Amounts are integer cents.
import { assertCents, percentOfCents } from './money.mjs';

/**
 * Returns the discount in cents (not the discounted total), never more than amountCents.
 * @param {number} amountCents
 * @param {{type: 'percent', value: number} | {type: 'fixed', amountCents: number} | undefined} discount
 */
export function discountCents(amountCents, discount) {
  assertCents(amountCents);
  if (!discount) return 0;
  if (discount.type === 'percent') {
    return Math.min(amountCents, percentOfCents(amountCents, discount.value));
  }
  if (discount.type === 'fixed') {
    const off = assertCents(discount.amountCents, 'discount amountCents');
    return Math.max(0, Math.min(amountCents, off));
  }
  throw new Error(`unknown discount type: ${discount.type}`);
}
