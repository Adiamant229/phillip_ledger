import { useCallback, useEffect, useMemo, useState, type ChangeEvent, type Dispatch, type FormEvent, type SetStateAction } from "react";
import { api, ApiError, fmt, newKey } from "./api";
import { REFRESH_MS } from "./fxSuggest";
import { checkRateInput, convert, crossRate, shortRate } from "./fxMath";
import { useMarketRates } from "./useMarketRates";
import { CURRENCIES, type Account, type Currency, type Entry, type IntegrityReport, type Reconciliation } from "./types";
const when = (iso: string) => new Date(iso).toLocaleString();
const shortId = (id: string) => id.slice(0, 8);

interface NoticeState {
  ok: boolean;
  text: string;
}

function Notice({ state }: { state: NoticeState | null }) {
  if (!state) return null;
  return (
    <div className={`notice ${state.ok ? "ok" : "err"}`} role={state.ok ? "status" : "alert"}>
      {state.text}
    </div>
  );
}

/** Wraps an async API call with busy/notice state. Returns null when the call failed. */
function useAction() {
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<NoticeState | null>(null);

  async function run<T>(fn: () => Promise<T>, okText: string | ((out: T) => string)): Promise<T | null> {
    setBusy(true);
    setNotice(null);
    try {
      const out = await fn();
      setNotice({ ok: true, text: typeof okText === "function" ? okText(out) : okText });
      return out;
    } catch (e) {
      setNotice({ ok: false, text: e instanceof ApiError ? e.message : String(e) });
      return null;
    } finally {
      setBusy(false);
    }
  }
  return { busy, notice, run };
}

// A form field setter that also rotates the idempotency key: a changed form is a different request.
const editing =
  (set: Dispatch<SetStateAction<string>>, rotateKey: () => void) =>
  (e: ChangeEvent<HTMLInputElement | HTMLSelectElement>) => {
    set(e.target.value);
    rotateKey();
  };

function NewAccount({ onCreated }: { onCreated: (a: Account) => void }) {
  const [name, setName] = useState("");
  const [currency, setCurrency] = useState<Currency>("USD");
  const [initial, setInitial] = useState("");
  const { busy, notice, run } = useAction();

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const a = await run(() => api.createAccount(name, currency, initial.trim()), "Account created.");
    if (a) {
      setName("");
      setInitial("");
      onCreated(a);
    }
  };
  return (
    <section className="card">
      <h2>Open an account</h2>
      <form className="row" onSubmit={submit}>
        <label>
          Name
          <input value={name} onChange={(e) => setName(e.target.value)} required maxLength={100} />
        </label>
        <div className="fields">
          <label>
            Currency
            <select value={currency} onChange={(e) => setCurrency(e.target.value as Currency)}>
              {CURRENCIES.map((c) => (
                <option key={c}>{c}</option>
              ))}
            </select>
          </label>
          <label>
            Opening balance
            <input className="num" inputMode="decimal" placeholder="0.00" value={initial} onChange={(e) => setInitial(e.target.value)} />
          </label>
        </div>
        <button disabled={busy}>Open account</button>
      </form>
      <Notice state={notice} />
    </section>
  );
}

function AccountList({
  accounts,
  selected,
  onSelect,
}: {
  accounts: Account[];
  selected: number | null;
  onSelect: (id: number) => void;
}) {
  return (
    <section className="card">
      <h2>Accounts</h2>
      {accounts.length === 0 && <p className="empty">No accounts yet. Open one to get started.</p>}
      <ul className="accounts">
        {accounts.map((a) => (
          <li key={a.id}>
            <button aria-current={a.id === selected} onClick={() => onSelect(a.id)}>
              <span>
                {a.name}
                <br />
                <span className="meta">
                  #{a.id} · {a.currency}
                </span>
              </span>
              <span className="num">{fmt(a.balance, a.currency)}</span>
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}

function TransferForm({
  accounts,
  defaultSource,
  perUsd,
  onDone,
}: {
  accounts: Account[];
  defaultSource: number | null;
  perUsd: Record<string, string>;
  onDone: () => void;
}) {
  const [src, setSrc] = useState(defaultSource === null ? "" : String(defaultSource));
  const [dst, setDst] = useState("");
  const [amount, setAmount] = useState("");
  const [typedRate, setTypedRate] = useState("");
  const [rateTouched, setRateTouched] = useState(false); // true once the user typed their own rate
  // One key per distinct request. It rotates whenever anything that is sent changes (accounts, amount, rate) or the
  // request succeeds, so a retry after a timeout re-sends the same key and cannot double-spend.
  const [key, setKey] = useState(newKey);
  const { busy, notice, run } = useAction();
  const rotate = () => setKey(newKey());

  useEffect(() => {
    if (defaultSource !== null) setSrc(String(defaultSource));
  }, [defaultSource]);

  const from = accounts.find((a) => String(a.id) === src);
  const to = accounts.find((a) => String(a.id) === dst);
  const crossCurrency = from !== undefined && to !== undefined && from.currency !== to.currency;

  // The suggestion is a plain division of the two USD quotes; typing in the field overrides it.
  const suggestion = crossCurrency ? crossRate(perUsd, from.currency, to.currency) : null;
  const rate = rateTouched ? typedRate : (suggestion ?? "");
  const rateCheck = crossCurrency ? checkRateInput(rate) : null;
  const received = crossCurrency && rateCheck?.ok ? convert(amount, rateCheck.value, to.currency) : null;

  // A market refresh can change an untouched suggestion; that changes the request, so it gets a new key.
  useEffect(() => {
    setKey(newKey());
  }, [rate]);

  const pick = (set: (v: string) => void) => (e: ChangeEvent<HTMLSelectElement>) => {
    set(e.target.value);
    setRateTouched(false); // a different pair gets its own suggestion
    rotate();
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (crossCurrency && !rateCheck?.ok) return;
    const res = await run(
      () => api.transfer(Number(src), Number(dst), amount.trim(), key, rateCheck?.ok ? rateCheck.value : undefined),
      (r) =>
        r.idempotent_replay
          ? `Already recorded earlier (transaction ${shortId(r.transaction_id)}). Nothing was moved twice.`
          : `Transfer confirmed. Transaction ${shortId(r.transaction_id)}.`,
    );
    if (res) {
      setAmount("");
      setRateTouched(false);
      rotate();
      onDone();
    }
  };

  const option = (a: Account) => (
    <option key={a.id} value={a.id}>
      #{a.id} {a.name} ({a.currency})
    </option>
  );

  return (
    <section className="card">
      <h2>Transfer</h2>
      <form className="row" onSubmit={submit}>
        <div className="fields">
          <label>
            From
            <select value={src} onChange={pick(setSrc)} required>
              <option value="">Choose…</option>
              {accounts.map(option)}
            </select>
          </label>
          <label>
            To
            <select value={dst} onChange={pick(setDst)} required>
              <option value="">Choose…</option>
              {accounts.filter((a) => String(a.id) !== src).map(option)}
            </select>
          </label>
          <label>
            Amount{from ? ` (${from.currency})` : ""}
            <input
              className="num"
              inputMode="decimal"
              value={amount}
              onChange={(e) => {
                setAmount(e.target.value);
                rotate();
              }}
              required
              placeholder="0.00"
            />
          </label>
          {crossCurrency && (
            <label>
              Exchange rate (1 {from.currency} = ? {to.currency})
              <input
                className="num"
                inputMode="decimal"
                value={rate}
                aria-invalid={rateCheck?.ok === false}
                onChange={(e) => {
                  setRateTouched(true);
                  setTypedRate(e.target.value);
                }}
                required
                placeholder="enter a rate"
              />
            </label>
          )}
          <button disabled={busy || (crossCurrency && !rateCheck?.ok)}>Send money</button>
        </div>
        {crossCurrency && (
          <p className="muted" role="status">
            {suggestion === null && !rateTouched
              ? `No rate is available for ${from.currency} → ${to.currency} (no market rate is loaded for one of the currencies). Enter one manually. `
              : rateTouched
                ? "Using the rate you typed. "
                : "Suggested rate: the two USD rates divided (no loss assumed through USD). Edit it to override. "}
            {rateTouched && suggestion !== null && (
              <button type="button" className="link" onClick={() => setRateTouched(false)}>
                Use suggestion ({shortRate(suggestion)})
              </button>
            )}
            {rate.trim() !== "" && rateCheck?.ok === false && <span className="err-text"> {rateCheck.reason}</span>}
            {received !== null && (
              <>
                {" "}
                Recipient receives <strong className="num">{fmt(received, to.currency)} {to.currency}</strong> (rounded to the
                currency's decimals).
              </>
            )}
          </p>
        )}
      </form>
      <Notice state={notice} />
    </section>
  );
}

function Deposit({ account, onDone }: { account: Account; onDone: () => void }) {
  const [amount, setAmount] = useState("");
  const [key, setKey] = useState(newKey);
  const { busy, notice, run } = useAction();

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const r = await run(() => api.deposit(account.id, amount.trim(), key), "Deposit confirmed.");
    if (r) {
      setAmount("");
      setKey(newKey());
      onDone();
    }
  };
  return (
    <>
      <form className="fields" onSubmit={submit}>
        <label>
          Add funds ({account.currency})
          <input className="num" inputMode="decimal" value={amount} placeholder="0.00" required onChange={editing(setAmount, () => setKey(newKey()))} />
        </label>
        <button className="secondary" disabled={busy}>
          Deposit
        </button>
      </form>
      <Notice state={notice} />
    </>
  );
}

function Journal({ account, entries, onChanged }: { account: Account; entries: Entry[]; onChanged: () => void }) {
  const { busy, notice, run } = useAction();

  const reverse = async (txId: string) => {
    const r = await run(() => api.reverse(txId, newKey()), (x) => `Reversed. Reversal transaction ${shortId(x.transaction_id)}.`);
    if (r) onChanged();
  };

  return (
    <section className="card">
      <div className="balance">
        <span className="num">{fmt(account.balance, account.currency)}</span>
        <span className="cur">{account.currency}</span>
        <span className="muted">
          · {account.name} (#{account.id})
        </span>
      </div>
      <Deposit account={account} onDone={onChanged} />
      <h2 style={{ marginTop: "1.25rem" }}>Journal</h2>
      {entries.length === 0 ? (
        <p className="empty">No activity yet.</p>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Time</th>
                <th>What</th>
                <th className="r">Debit</th>
                <th className="r">Credit</th>
                <th className="r">Balance</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {entries.map((e) => {
                const debit = e.amount.startsWith("-");
                const counter =
                  e.kind === "deposit" ? "funding" : e.source_account_id === account.id ? `#${e.destination_account_id}` : `#${e.source_account_id}`;
                const canReverse = e.kind !== "reversal" && !e.reversed_by_transaction_id;
                return (
                  <tr key={e.entry_id}>
                    <td className="sans">{when(e.created_at)}</td>
                    <td className="sans">
                      <span className={`tag ${e.kind === "reversal" ? "rev" : ""}`}>{e.kind}</span> {account.is_system ? "" : `with ${counter}`}{" "}
                      <span className="mono muted">{shortId(e.transaction_id)}</span>
                      {e.reversed_by_transaction_id && (
                        <>
                          {" "}
                          <span className="tag rev">reversed</span>
                        </>
                      )}
                    </td>
                    <td className="num r debit">{debit ? fmt(e.amount.slice(1), e.currency) : ""}</td>
                    <td className="num r credit">{debit ? "" : fmt(e.amount, e.currency)}</td>
                    <td className="num r">{fmt(e.balance_after, e.currency)}</td>
                    <td>
                      {canReverse && (
                        <button className="link" disabled={busy} onClick={() => reverse(e.transaction_id)}>
                          Reverse
                        </button>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      <Notice state={notice} />
    </section>
  );
}

function ago(ms: number): string {
  const min = Math.floor(ms / 60000);
  if (min < 1) return "just now";
  if (min < 60) return `${min} min ago`;
  const h = Math.floor(min / 60);
  return `${h} h ${min % 60} min ago`;
}

function MarketTable({ state }: { state: ReturnType<typeof useMarketRates> }) {
  const { data, error, refreshing, now, refresh } = state;
  const [filter, setFilter] = useState("");

  // Only the ledger's currencies are requested, shown in the ledger's own order.
  const rows = useMemo(() => {
    if (!data) return [];
    const q = filter.trim().toUpperCase();
    return CURRENCIES.filter((c) => c.includes(q) && data.rates[c] !== undefined).map((c) => [c, data.rates[c] ?? ""] as const);
  }, [data, filter]);
  const missing = data ? CURRENCIES.filter((c) => data.rates[c] === undefined) : [];

  const everyMin = Math.round(REFRESH_MS / 60000);
  return (
    <div>
      <div className="toolbar">
        <h3>Market rates, per 1 {data?.base ?? "USD"}</h3>
        <div className="fields">
          <label>
            Filter
            <input value={filter} placeholder="e.g. SGD" maxLength={10} onChange={(e) => setFilter(e.target.value)} />
          </label>
          <button type="button" className="secondary" disabled={refreshing} onClick={() => void refresh()}>
            {refreshing ? "Refreshing…" : "Refresh now"}
          </button>
        </div>
      </div>
      <p className="muted" role="status">
        {data
          ? `Updated ${ago(now - data.fetchedAt)} (provider time ${data.date.slice(0, 16)} UTC). Refreshes automatically every ${everyMin} min.`
          : error
            ? ""
            : "Loading market rates…"}
        {data?.stale && " The last refresh failed, so these may be out of date."}
        {error && ` ${error}`}
        {missing.length > 0 && ` No market rate returned for: ${missing.join(", ")}.`}
      </p>
      {data && (
        <div className="scroll" role="region" aria-label="Market exchange rates" tabIndex={0}>
          <table>
            <thead>
              <tr>
                <th>Currency</th>
                <th className="r">Rate</th>
              </tr>
            </thead>
            <tbody>
              {rows.map(([code, v]) => (
                <tr key={code}>
                  <td>{code}</td>
                  <td className="num r" title={v}>
                    {shortRate(v)}
                  </td>
                </tr>
              ))}
              {rows.length === 0 && (
                <tr>
                  <td colSpan={2} className="empty">
                    No currency matches “{filter}”.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function Rates({ marketState }: { marketState: ReturnType<typeof useMarketRates> }) {
  return (
    <section className="card">
      <h2>Exchange rates</h2>
      <MarketTable state={marketState} />
    </section>
  );
}

function Audit() {
  const [report, setReport] = useState<IntegrityReport | null>(null);
  const [recon, setRecon] = useState<Reconciliation | null>(null);
  const [period, setPeriod] = useState(new Date().toISOString().slice(0, 7));
  const check = useAction();
  const reconcile = useAction();

  return (
    <section className="card">
      <h2>Ledger checks</h2>
      <div className="fields">
        <button className="secondary" disabled={check.busy} onClick={async () => setReport(await check.run(api.integrity, "Check finished."))}>
          Verify ledger totals
        </button>
        <label>
          Month
          <input type="month" value={period} onChange={(e) => setPeriod(e.target.value)} />
        </label>
        <button
          className="secondary"
          disabled={reconcile.busy}
          onClick={async () => setRecon(await reconcile.run(() => api.reconcile(period), "Reconciliation saved."))}
        >
          Reconcile month
        </button>
      </div>

      <Notice state={check.notice} />
      {report && (
        <div className={`notice ${report.ok ? "ok" : "err"}`}>
          {report.ok ? "Every currency sums to zero and every account matches its entries." : `${report.issues.length} problem(s) found.`}
          <table style={{ marginTop: ".5rem" }}>
            <thead>
              <tr>
                <th>Currency</th>
                <th className="r">Sum of balances</th>
                <th className="r">Expected</th>
              </tr>
            </thead>
            <tbody>
              {report.currency_totals.map((t) => (
                <tr key={t.currency}>
                  <td>{t.currency}</td>
                  <td className="num r">{t.balance_sum}</td>
                  <td className="num r">{t.expected}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <ul className="issues">
            {report.issues.map((i, n) => (
              <li key={n}>{i.message}</li>
            ))}
          </ul>
        </div>
      )}

      <Notice state={reconcile.notice} />
      {recon && (
        <div className={`notice ${recon.status === "clean" ? "ok" : "err"}`}>
          {recon.status === "clean"
            ? `${recon.accounts_checked} accounts reconciled for ${recon.period_start.slice(0, 7)} with no discrepancies.`
            : `${recon.discrepancy_count} discrepancies in ${recon.period_start.slice(0, 7)}.`}
          <ul className="issues">
            {recon.issues?.map((i, n) => (
              <li key={`i${n}`}>{i.message}</li>
            ))}
            {recon.lines?.flatMap((l) =>
              l.discrepancies.map((d, n) => (
                <li key={`${l.account_id}-${n}`}>
                  Account #{l.account_id}: {d}
                </li>
              )),
            )}
          </ul>
        </div>
      )}
    </section>
  );
}

export default function App() {
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [selected, setSelected] = useState<number | null>(null);
  const [entries, setEntries] = useState<Entry[]>([]);
  const [loadError, setLoadError] = useState<string | null>(null);
  const marketState = useMarketRates(); // one timer shared by the rates card and the transfer suggestion
  const perUsd = marketState.data?.rates ?? {}; // units per 1 USD; the transfer form divides two of these

  const refresh = useCallback(async () => {
    try {
      setAccounts(await api.accounts());
      setLoadError(null);
      if (selected !== null) setEntries(await api.history(selected));
    } catch (e) {
      setLoadError(e instanceof Error ? e.message : String(e));
    }
  }, [selected]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const account = accounts.find((a) => a.id === selected);

  return (
    <div className="app">
      <header className="top">
        <h1>Ledger</h1>
      </header>
      {loadError && (
        <div className="notice err" role="alert">
          {loadError} Is the API running on port 8080?
        </div>
      )}
      <div className="grid">
        <div className="stack">
          <AccountList accounts={accounts} selected={selected} onSelect={setSelected} />
          <NewAccount
            onCreated={(a) => {
              setSelected(a.id);
              void refresh();
            }}
          />
        </div>
        <div className="stack">
          <TransferForm accounts={accounts} defaultSource={selected} perUsd={perUsd} onDone={() => void refresh()} />
          {account ? (
            <Journal account={account} entries={entries} onChanged={() => void refresh()} />
          ) : (
            <section className="card">
              <p className="empty">Select an account to see its balance and journal.</p>
            </section>
          )}
          <Rates marketState={marketState} />
          <Audit />
        </div>
      </div>
    </div>
  );
}
