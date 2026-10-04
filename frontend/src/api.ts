// Thin fetch wrapper. Money is only ever handled as strings here: JSON numbers would round-trip through
// float64 and silently lose precision.
import { CURRENCY_EXPONENTS } from "./types";
import type {
  Account, ApiErrorBody, Currency, Entry, IntegrityReport, PostingResult, Rate, Reconciliation, Transaction,
} from "./types";

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    public readonly code: string,
    message: string,
  ) {
    super(message);
  }
}

interface CallOptions {
  body?: unknown;
  key?: string; // Idempotency-Key
}

async function call<T>(method: "GET" | "POST", path: string, { body, key }: CallOptions = {}): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (key) headers["Idempotency-Key"] = key;

  let res: Response;
  try {
    res = await fetch(`/api${path}`, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  } catch {
    throw new ApiError(0, "network", "Could not reach the server. Retrying with the same request is safe.");
  }
  const data: unknown = await res.json().catch(() => null);
  if (!res.ok) {
    const e = (data as ApiErrorBody | null)?.error;
    throw new ApiError(res.status, e?.code ?? "error", e?.message ?? `Request failed (${res.status})`);
  }
  return data as T;
}

export const api = {
  accounts: () => call<Account[]>("GET", "/accounts"),
  history: (id: number) => call<Entry[]>("GET", `/accounts/${id}/transactions?order=desc&limit=100`),
  createAccount: (name: string, currency: Currency, initialBalance: string) =>
    call<Account>("POST", "/accounts", { body: { name, currency, initial_balance: initialBalance || "0" } }),
  deposit: (id: number, amount: string, key: string) =>
    call<PostingResult>("POST", `/accounts/${id}/deposit`, { body: { amount }, key }),
  /** `exchangeRate` (1 source = rate destination) is sent only when given; otherwise the server derives one. */
  transfer: (source: number, destination: number, amount: string, key: string, exchangeRate?: string) =>
    call<PostingResult>("POST", "/transactions", {
      body: {
        source_account_id: source,
        destination_account_id: destination,
        amount,
        ...(exchangeRate ? { exchange_rate: exchangeRate } : {}),
      },
      key,
    }),
  reverse: (txId: string, key: string) => call<PostingResult>("POST", `/transactions/${txId}/reverse`, { key }),
  transaction: (id: string) => call<Transaction>("GET", `/transactions/${id}`),
  rates: () => call<Rate[]>("GET", "/exchange-rates"),
  /** Stores "1 USD = rate quote", replacing the previous value for that currency. */
  setRate: (quote: Currency, rate: string) => call<Rate>("POST", "/exchange-rates", { body: { quote, rate } }),
  integrity: () => call<IntegrityReport>("GET", "/audit/integrity"),
  reconcile: (period: string) => call<Reconciliation>("POST", "/reconciliations", { body: { period } }),
};

export const newKey = (): string => crypto.randomUUID();

// String-based formatting (no floats): trim padding zeros, keep the currency's usual minimum decimals.
export function fmt(amount: string, currency?: Currency): string {
  const neg = amount.startsWith("-");
  const [int = "0", frac = ""] = amount.replace("-", "").split(".");
  const min = currency ? CURRENCY_EXPONENTS[currency] : 2;
  let f = frac.replace(/0+$/, "");
  while (f.length < min) f += "0";
  const grouped = int.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  return `${neg ? "−" : ""}${grouped}${f ? "." + f : ""}`;
}
