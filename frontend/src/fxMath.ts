// Exchange-rate arithmetic with decimal.js (no floats). Triangulation through USD is treated as lossless, so a
// pair rate is a plain division of two "per 1 USD" quotes. The rounding rules mirror the Go service:
//   rate   = (quote per USD) / (base per USD), rounded half-up to 12 dp   (Go: DivRound(.., 12))
//   amount = source amount x rate, rounded half-even to the destination currency's decimal places

import Decimal from "decimal.js";
import { CURRENCY_EXPONENTS, type Currency, type Money } from "./types";

/** Own constructor so global Decimal settings elsewhere cannot change our results. */
const D = Decimal.clone({ precision: 60, rounding: Decimal.ROUND_HALF_UP, toExpNeg: -30, toExpPos: 30 });

export const RATE_DP = 12; // the backend stores and accepts at most 12 decimal places

const PLAIN_DECIMAL = /^\d+(\.\d+)?$/;

/** Trimmed plain decimal string, or null. No exponent notation, no sign, no separators. */
export function parseDecimal(s: string): Decimal | null {
  const t = s.trim();
  return PLAIN_DECIMAL.test(t) ? new D(t) : null;
}

export type RateCheck = { ok: true; value: string } | { ok: false; reason: string };

/** Validates what the user typed into the rate field (same limits as the backend: > 0, <= 12 dp). */
export function checkRateInput(s: string): RateCheck {
  const t = s.trim();
  if (t === "") return { ok: false, reason: "Enter an exchange rate." };
  const d = parseDecimal(t);
  if (!d) return { ok: false, reason: "The rate must be a plain number such as 1.2345." };
  if (d.isZero()) return { ok: false, reason: "The rate must be greater than zero." };
  if (d.decimalPlaces() > RATE_DP) return { ok: false, reason: `The rate can have at most ${RATE_DP} decimal places.` };
  return { ok: true, value: t };
}

/** Fixed-point string without trailing zeros ("1.3000" -> "1.3"). */
const trim = (d: Decimal): string => d.toFixed().replace(/(\.\d*?)0+$/, "$1").replace(/\.$/, "");

/**
 * Rate for 1 `base` in `quote` from "units per 1 USD" quotes (`perUsd`, USD itself is implicitly 1).
 * Null when either quote is missing/unusable, the currencies are equal, or the result rounds to zero.
 */
export function crossRate(perUsd: Readonly<Record<string, Money>>, base: string, quote: string): Money | null {
  if (base === quote) return null;
  const per = (c: string): Decimal | null => {
    if (c === "USD") return new D(1);
    const v = perUsd[c];
    const d = v === undefined ? null : parseDecimal(v);
    return d && d.gt(0) ? d : null;
  };
  const b = per(base);
  const q = per(quote);
  if (!b || !q) return null;
  const r = q.div(b).toDecimalPlaces(RATE_DP, Decimal.ROUND_HALF_UP);
  return r.isZero() ? null : trim(r);
}

/** What the recipient gets: amount x rate, rounded half-even to the currency's decimal places. Null on bad input. */
export function convert(amount: string, rate: string, to: Currency): Money | null {
  const a = parseDecimal(amount);
  const r = parseDecimal(rate);
  if (!a || !r) return null;
  return a.mul(r).toDecimalPlaces(CURRENCY_EXPONENTS[to], Decimal.ROUND_HALF_EVEN).toFixed(CURRENCY_EXPONENTS[to]);
}

/** "2.3263929277654998" -> "2.326393": 6 significant digits below 1, else 6 decimals; the tooltip has the full value. */
export function shortRate(v: string): string {
  const d = parseDecimal(v);
  if (!d) return v;
  if (d.isZero()) return "0";
  const dp = d.lt(1) ? Math.min(RATE_DP, 5 - d.e) : 6; // d.e = exponent of the first significant digit (0.00464 -> -3)
  return trim(d.toDecimalPlaces(dp, Decimal.ROUND_HALF_UP));
}
