// Market exchange rates from CurrencyFreaks, cached in localStorage with a TTL.
// They feed (a) the scrollable rates table and (b) the suggested exchange rate in the transfer form, which the
// user can always overwrite. Only the ledger's currencies are requested (`symbols=`), which keeps the response small.
// Nothing here touches the ledger's own stored rates.

import { CURRENCIES, type Money } from "./types";

const API_URL = "https://api.currencyfreaks.com/v2.0/rates/latest";
const API_KEY = import.meta.env.VITE_CURRENCYFREAKS_KEY as string | undefined;
const SYMBOLS = CURRENCIES.join(",");
const CACHE_KEY = `fx-market-rates-v3:${SYMBOLS}`; // changes with the currency list, so stale shapes are never reused

/** How often the table refreshes. Each refresh is one API request, so mind the plan's monthly quota. */
const MINUTES = Number(import.meta.env.VITE_FX_REFRESH_MINUTES);
export const REFRESH_MS = (Number.isFinite(MINUTES) && MINUTES >= 1 ? MINUTES : 60) * 60 * 1000;
/** After a failed refresh, wait this long before trying again on its own (a manual refresh ignores it). */
const RETRY_AFTER_FAILURE_MS = 5 * 60 * 1000;

export interface MarketRates {
  /** Units of each currency per 1 `base` (the API quotes against USD). */
  rates: Record<string, Money>;
  base: string;
  /** The provider's own timestamp, e.g. "2026-08-25 12:43:00+00". */
  date: string;
  /** When we fetched it (ms since epoch); the refresh interval runs from here. */
  fetchedAt: number;
  /** True when a refresh failed and an expired cache entry is being shown instead. */
  stale: boolean;
}

type Cached = Omit<MarketRates, "stale">;

function readCache(): Cached | null {
  try {
    const raw = localStorage.getItem(CACHE_KEY);
    if (!raw) return null;
    const v = JSON.parse(raw) as Cached;
    return typeof v.fetchedAt === "number" && v.rates && typeof v.rates === "object" ? v : null;
  } catch {
    return null;
  }
}

function writeCache(v: Cached) {
  try {
    localStorage.setItem(CACHE_KEY, JSON.stringify(v));
  } catch {
    /* storage unavailable (private mode, quota): skip caching */
  }
}

let inflight: Promise<Cached> | null = null; // collapses concurrent callers into one request
let lastFailureAt = 0;

async function fetchFresh(key: string): Promise<Cached> {
  const res = await fetch(`${API_URL}?apikey=${encodeURIComponent(key)}&symbols=${SYMBOLS}`);
  if (!res.ok) throw new Error(`CurrencyFreaks responded ${res.status}`);
  const body = (await res.json()) as { date?: string; base?: string; rates?: Record<string, unknown> };
  const rates: Record<string, Money> = {};
  for (const [code, v] of Object.entries(body.rates ?? {})) {
    if ((CURRENCIES as string[]).includes(code) && (typeof v === "string" || typeof v === "number") && Number(v) > 0) rates[code] = String(v);
  }
  if (Object.keys(rates).length === 0) throw new Error("CurrencyFreaks returned no usable rates");
  const fresh: Cached = { rates, base: body.base ?? "USD", date: body.date ?? "", fetchedAt: Date.now() };
  writeCache(fresh);
  return fresh;
}

/** True when the cached rates are still inside the refresh interval (so no network call is needed). */
export function isFresh(): boolean {
  const c = readCache();
  return c !== null && Date.now() - c.fetchedAt < REFRESH_MS;
}

/**
 * Fresh cache -> use it. Otherwise fetch; if that fails, fall back to the expired cache (flagged stale).
 * `force` skips both the cache and the post-failure back-off (used by the Refresh button).
 */
export async function loadRates(opts: { force?: boolean } = {}): Promise<MarketRates> {
  const cached = readCache();
  if (!opts.force && cached && Date.now() - cached.fetchedAt < REFRESH_MS) return { ...cached, stale: false };

  if (!API_KEY) {
    if (cached) return { ...cached, stale: true };
    throw new Error("Set VITE_CURRENCYFREAKS_KEY in frontend/.env.local to load market rates.");
  }
  if (!opts.force && cached && Date.now() - lastFailureAt < RETRY_AFTER_FAILURE_MS) return { ...cached, stale: true };

  try {
    inflight ??= fetchFresh(API_KEY).finally(() => {
      inflight = null;
    });
    const fresh = await inflight;
    lastFailureAt = 0;
    return { ...fresh, stale: false };
  } catch (e) {
    lastFailureAt = Date.now();
    if (cached) return { ...cached, stale: true };
    throw e instanceof Error ? e : new Error(String(e));
  }
}
