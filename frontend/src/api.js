// Thin fetch wrapper. Money is only ever handled as strings here: JSON numbers would round-trip through
// float64 and silently lose precision.

export class ApiError extends Error {
  constructor(status, code, message) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

async function call(method, path, { body, key } = {}) {
  const headers = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (key) headers["Idempotency-Key"] = key;
  let res;
  try {
    res = await fetch(`/api${path}`, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  } catch {
    throw new ApiError(0, "network", "Could not reach the server. Retrying with the same request is safe.");
  }
  const data = await res.json().catch(() => null);
  if (!res.ok) {
    const e = data?.error;
    throw new ApiError(res.status, e?.code ?? "error", e?.message ?? `Request failed (${res.status})`);
  }
  return data;
}

export const api = {
  accounts: () => call("GET", "/accounts"),
  account: (id) => call("GET", `/accounts/${id}`),
  history: (id) => call("GET", `/accounts/${id}/transactions?order=desc&limit=100`),
  createAccount: (name, currency, initial_balance) =>
    call("POST", "/accounts", { body: { name, currency, initial_balance: initial_balance || "0" } }),
  deposit: (id, amount, key) => call("POST", `/accounts/${id}/deposit`, { body: { amount }, key }),
  transfer: (source_account_id, destination_account_id, amount, key) =>
    call("POST", "/transactions", { body: { source_account_id, destination_account_id, amount }, key }),
  reverse: (txId, key) => call("POST", `/transactions/${txId}/reverse`, { key }),
  rates: () => call("GET", "/exchange-rates"),
  setRate: (base, quote, rate) => call("POST", "/exchange-rates", { body: { base, quote, rate } }),
  integrity: () => call("GET", "/audit/integrity"),
  reconcile: (period) => call("POST", "/reconciliations", { body: { period } }),
  reconciliations: () => call("GET", "/reconciliations"),
};

export const newKey = () => crypto.randomUUID();

const MIN_DECIMALS = { JPY: 0 };

// String-based formatting (no floats): trim padding zeros, keep the currency's usual minimum decimals.
export function fmt(amount, currency = "") {
  if (amount === undefined || amount === null) return "";
  const neg = amount.startsWith("-");
  const [int, frac = ""] = amount.replace("-", "").split(".");
  const min = MIN_DECIMALS[currency] ?? 2;
  let f = frac.replace(/0+$/, "");
  while (f.length < min) f += "0";
  const grouped = int.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  return `${neg ? "−" : ""}${grouped}${f ? "." + f : ""}`;
}
