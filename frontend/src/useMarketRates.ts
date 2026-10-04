import { useCallback, useEffect, useRef, useState } from "react";
import { isFresh, loadRates, type MarketRates } from "./fxSuggest";

const CHECK_MS = 30 * 1000; // how often we look at the clock; a network call only happens once the data is due

/**
 * Keeps market rates loaded and up to date: loads on mount, re-checks every 30s, refreshes when the interval has
 * elapsed, and also when the tab becomes visible again (timers are throttled in background tabs). `now` ticks
 * with each check so "updated 5 min ago" labels stay current.
 */
export function useMarketRates() {
  const [data, setData] = useState<MarketRates | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [refreshing, setRefreshing] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const alive = useRef(true);

  const refresh = useCallback(async (force = false) => {
    setNow(Date.now());
    const willFetch = force || !isFresh(); // a fresh cache is returned without any network call
    if (willFetch) setRefreshing(true);
    try {
      const d = await loadRates({ force });
      if (alive.current) {
        // Keep the same object when nothing changed so the (large) table is not re-rendered on every check.
        setData((prev) => (prev && prev.fetchedAt === d.fetchedAt && prev.stale === d.stale ? prev : d));
        setError(null);
      }
    } catch (e) {
      if (alive.current) setError(e instanceof Error ? e.message : String(e));
    } finally {
      if (alive.current && willFetch) setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    alive.current = true;
    void refresh(); // initial load: shows the cache if fresh, fetches if not
    const tick = () => {
      if (!document.hidden) void refresh();
    };
    const id = window.setInterval(tick, CHECK_MS);
    document.addEventListener("visibilitychange", tick);
    return () => {
      alive.current = false;
      window.clearInterval(id);
      document.removeEventListener("visibilitychange", tick);
    };
  }, [refresh]);

  return { data, error, refreshing, now, refresh: () => refresh(true) };
}
