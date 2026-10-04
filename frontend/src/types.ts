// Mirrors the Go API's JSON. Every money value is a decimal *string*, never a number.

export type Money = string;

/**
 * Currencies the ledger supports, with their decimal places. Keep in sync with the `currencies` insert in
 * backend/internal/repo/migrations/001_init.sql (same order). Anything outside this list is not in the ledger.
 */
export const CURRENCY_EXPONENTS = {
  USD: 2, EUR: 2, GBP: 2, SGD: 2, JPY: 0, AED: 2, AUD: 2, BRL: 2, CAD: 2, CHF: 2,
  CNY: 2, DKK: 2, HKD: 2, IDR: 2, INR: 2, KRW: 0, KWD: 3, MXN: 2, MYR: 2, NOK: 2,
  NZD: 2, PHP: 2, PKR: 2, SAR: 2, SEK: 2, THB: 2, TRY: 2, TWD: 2, VND: 0, ZAR: 2,
} as const;
export type Currency = keyof typeof CURRENCY_EXPONENTS;
export const CURRENCIES = Object.keys(CURRENCY_EXPONENTS) as Currency[];
export type TxKind = "transfer" | "deposit" | "reversal";

export interface Account {
  id: number;
  name: string;
  currency: Currency;
  balance: Money;
  is_system: boolean;
  allow_negative: boolean;
  created_at: string;
}

export interface Transaction {
  id: string;
  kind: TxKind;
  source_account_id: number;
  destination_account_id: number;
  source_amount: Money;
  source_currency: Currency;
  destination_amount: Money;
  destination_currency: Currency;
  fx_rate?: Money;
  reverses_transaction_id?: string;
  reversed_by_transaction_id?: string;
  created_at: string;
}

/** Response of POST /transactions, /accounts/{id}/deposit and /transactions/{id}/reverse. */
export interface PostingResult {
  transaction_id: string;
  status: "confirmed";
  idempotent_replay: boolean;
  transaction: Transaction;
}

export interface Entry {
  entry_id: number;
  transaction_id: string;
  kind: TxKind;
  amount: Money; // signed: + credit, - debit
  currency: Currency;
  balance_after: Money;
  source_account_id: number;
  destination_account_id: number;
  reversed_by_transaction_id?: string;
  created_at: string;
}

/** A ledger-stored rate: always 1 USD = `rate` units of `quote` (at most one row per non-USD currency). */
export interface Rate {
  base: "USD";
  quote: Currency;
  rate: Money;
  effective_at: string;
}

export interface Issue {
  check: string;
  message: string;
}

export interface CurrencyTotal {
  currency: Currency;
  balance_sum: Money;
  entries_sum: Money;
  expected: Money;
  ok: boolean;
}

export interface IntegrityReport {
  checked_at: string;
  ok: boolean;
  currency_totals: CurrencyTotal[];
  issues: Issue[];
}

export interface ReconLine {
  account_id: number;
  account_name: string;
  currency: Currency;
  opening: Money;
  credits: Money;
  debits: Money;
  closing: Money;
  recorded_closing: Money;
  current_balance: Money;
  checked_against_current: boolean;
  discrepancies: string[];
}

export interface Reconciliation {
  id: number;
  period_start: string;
  period_end: string;
  status: "clean" | "discrepancies";
  accounts_checked: number;
  discrepancy_count: number;
  lines?: ReconLine[];
  issues?: Issue[];
  created_at: string;
}

export interface ApiErrorBody {
  error?: { code?: string; message?: string };
}
