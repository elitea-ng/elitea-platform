// Checkout summary built from the cart, discount, tax and format modules.
// All amounts are integer cents.
import { subtotalCents } from './cart.mjs';
import { discountCents } from './discount.mjs';
import { taxCents } from './tax.mjs';
import { formatCents } from './format.mjs';

/**
 * @param {{items: object[]}} cart
 * @param {{discount?: object, taxRatePercent?: number}} [options]
 */
export function summarize(cart, { discount, taxRatePercent = 0 } = {}) {
  const sub = subtotalCents(cart);
  const disc = discountCents(sub, discount);
  const taxable = sub - disc;
  const tax = taxCents(taxable, taxRatePercent);
  const total = taxable + tax;
  return {
    subtotalCents: sub,
    discountCents: disc,
    taxCents: tax,
    totalCents: total,
    display: {
      subtotal: formatCents(sub),
      discount: formatCents(disc),
      tax: formatCents(tax),
      total: formatCents(total),
    },
  };
}
