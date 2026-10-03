import { useCallback, useEffect, useState } from "react";
import { api, ApiError, fmt, newKey } from "./api.js";

const CURRENCIES = ["USD", "EUR", "GBP", "SGD", "JPY"];
const when = (iso) => new Date(iso).toLocaleString();
const shortId = (id) => id.slice(0, 8);

function Notice({ state }) {
  if (!state) return null;
  return (
    <div className={`notice ${state.ok ? "ok" : "err"}`} role={state.ok ? "status" : "alert"}>
      {state.text}
    </div>
  );
}

function useAction() {
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState(null);
  const run = async (fn, okText) => {
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
  };
  return { busy, notice, run, clear: () => setNotice(null) };
}

function NewAccount({ onCreated }) {
  const [name, setName] = useState("");
  const [currency, setCurrency] = useState("USD");
  const [initial, setInitial] = useState("");
  const { busy, notice, run } = useAction();

  const submit = async (e) => {
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
        <label>Name<input value={name} onChange={(e) => setName(e.target.value)} required maxLength={100} /></label>
        <div className="fields">
          <label>Currency
            <select value={currency} onChange={(e) => setCurrency(e.target.value)}>
              {CURRENCIES.map((c) => <option key={c}>{c}</option>)}
            </select>
          </label>
          <label>Opening balance
            <input className="num" inputMode="decimal" placeholder="0.00" value={initial} onChange={(e) => setInitial(e.target.value)} />
          </label>
        </div>
        <button disabled={busy}>Open account</button>
      </form>
      <Notice state={notice} />
    </section>
  );
}

function AccountList({ accounts, selected, onSelect }) {
  return (
    <section className="card">
      <h2>Accounts</h2>
      {accounts.length === 0 && <p className="empty">No accounts yet. Open one to get started.</p>}
      <ul className="accounts">
        {accounts.map((a) => (
          <li key={a.id}>
            <button aria-current={a.id === selected} onClick={() => onSelect(a.id)}>
              <span>{a.name}<br /><span className="meta">#{a.id} · {a.currency}</span></span>
              <span className="num">{fmt(a.balance, a.currency)}</span>
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}

function TransferForm({ accounts, onDone, defaultSource }) {
  const [src, setSrc] = useState(defaultSource ?? "");
  const [dst, setDst] = useState("");
  const [amount, setAmount] = useState("");
  // One key per distinct request. It only changes when the user edits the form or the request succeeds, so a
  // retry after a timeout re-sends the same key and cannot double-spend.
  const [key, setKey] = useState(newKey);
  const { busy, notice, run } = useAction();

  useEffect(() => { if (defaultSource) setSrc(String(defaultSource)); }, [defaultSource]);
  const edit = (set) => (e) => { set(e.target.value); setKey(newKey()); };

  const from = accounts.find((a) => String(a.id) === String(src));
  const to = accounts.find((a) => String(a.id) === String(dst));

  const submit = async (e) => {
    e.preventDefault();
    const res = await run(
      () => api.transfer(Number(src), Number(dst), amount.trim(), key),
      (r) => r.idempotent_replay
        ? `Already recorded earlier (transaction ${shortId(r.transaction_id)}). Nothing was moved twice.`
        : `Transfer confirmed. Transaction ${shortId(r.transaction_id)}.`,
    );
    if (res) { setAmount(""); setKey(newKey()); onDone(); }
  };

  return (
    <section className="card">
      <h2>Transfer</h2>
      <form className="row" onSubmit={submit}>
        <div className="fields">
          <label>From
            <select value={src} onChange={edit(setSrc)} required>
              <option value="">Choose…</option>
              {accounts.map((a) => <option key={a.id} value={a.id}>#{a.id} {a.name} ({a.currency})</option>)}
            </select>
          </label>
          <label>To
            <select value={dst} onChange={edit(setDst)} required>
              <option value="">Choose…</option>
              {accounts.filter((a) => String(a.id) !== String(src)).map((a) => <option key={a.id} value={a.id}>#{a.id} {a.name} ({a.currency})</option>)}
            </select>
          </label>
          <label>Amount{from ? ` (${from.currency})` : ""}
            <input className="num" inputMode="decimal" value={amount} onChange={edit(setAmount)} required placeholder="0.00" />
          </label>
          <button disabled={busy}>Send money</button>
        </div>
        {from && to && from.currency !== to.currency && (
          <p className="muted">Different currencies: the latest exchange rate is applied and the converted amount is shown in the history.</p>
        )}
      </form>
      <Notice state={notice} />
    </section>
  );
}

function Journal({ account, entries, onChanged }) {
  const { busy, notice, run } = useAction();
  const reverse = (txId) =>
    run(() => api.reverse(txId, newKey()), (r) => `Reversed. Reversal transaction ${shortId(r.transaction_id)}.`).then((r) => r && onChanged());

  return (
    <section className="card">
      <div className="balance">
        <span className="num">{fmt(account.balance, account.currency)}</span>
        <span className="cur">{account.currency}</span>
        <span className="muted">· {account.name} (#{account.id})</span>
      </div>
      <Deposit account={account} onDone={onChanged} />
      <h2 style={{ marginTop: "1.25rem" }}>Journal</h2>
      {entries.length === 0 ? <p className="empty">No activity yet.</p> : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Time</th><th>What</th><th className="r">Debit</th><th className="r">Credit</th><th className="r">Balance</th><th />
              </tr>
            </thead>
            <tbody>
              {entries.map((e) => {
                const debit = e.amount.startsWith("-");
                const counter = e.kind === "deposit" ? "funding" : e.source_account_id === account.id ? `#${e.destination_account_id}` : `#${e.source_account_id}`;
                const canReverse = e.kind !== "reversal" && !e.reversed_by_transaction_id;
                return (
                  <tr key={e.entry_id}>
                    <td className="sans">{when(e.created_at)}</td>
                    <td className="sans">
                      <span className={`tag ${e.kind === "reversal" ? "rev" : ""}`}>{e.kind}</span>{" "}
                      {account.is_system ? "" : `with ${counter}`}{" "}
                      <span className="mono muted">{shortId(e.transaction_id)}</span>
                      {e.reversed_by_transaction_id && <> <span className="tag rev">reversed</span></>}
                    </td>
                    <td className="num r debit">{debit ? fmt(e.amount.slice(1), e.currency) : ""}</td>
                    <td className="num r credit">{debit ? "" : fmt(e.amount, e.currency)}</td>
                    <td className="num r">{fmt(e.balance_after, e.currency)}</td>
                    <td>{canReverse && <button className="link" disabled={busy} onClick={() => reverse(e.transaction_id)}>Reverse</button>}</td>
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

function Deposit({ account, onDone }) {
  const [amount, setAmount] = useState("");
  const [key, setKey] = useState(newKey);
  const { busy, notice, run } = useAction();
  const submit = async (e) => {
    e.preventDefault();
    const r = await run(() => api.deposit(account.id, amount.trim(), key), "Deposit confirmed.");
    if (r) { setAmount(""); setKey(newKey()); onDone(); }
  };
  return (
    <>
      <form className="fields" onSubmit={submit}>
        <label>Add funds ({account.currency})
          <input className="num" inputMode="decimal" value={amount} placeholder="0.00" required
            onChange={(e) => { setAmount(e.target.value); setKey(newKey()); }} />
        </label>
        <button className="secondary" disabled={busy}>Deposit</button>
      </form>
      <Notice state={notice} />
    </>
  );
}

function Rates() {
  const [rates, setRates] = useState([]);
  const [f, setF] = useState({ base: "USD", quote: "EUR", rate: "" });
  const { busy, notice, run } = useAction();
  const load = useCallback(() => api.rates().then(setRates).catch(() => {}), []);
  useEffect(() => { load(); }, [load]);
  const submit = async (e) => {
    e.preventDefault();
    if (await run(() => api.setRate(f.base, f.quote, f.rate.trim()), "Rate saved. New cross-currency transfers use it.")) { setF({ ...f, rate: "" }); load(); }
  };
  return (
    <section className="card">
      <h2>Exchange rates</h2>
      <div className="table-wrap">
        <table>
          <thead><tr><th>Pair</th><th className="r">Rate</th></tr></thead>
          <tbody>{rates.map((r) => <tr key={r.base + r.quote}><td>1 {r.base} →</td><td className="num r">{r.rate} {r.quote}</td></tr>)}</tbody>
        </table>
      </div>
      <form className="fields" style={{ marginTop: ".75rem" }} onSubmit={submit}>
        <label>Base<select value={f.base} onChange={(e) => setF({ ...f, base: e.target.value })}>{CURRENCIES.map((c) => <option key={c}>{c}</option>)}</select></label>
        <label>Quote<select value={f.quote} onChange={(e) => setF({ ...f, quote: e.target.value })}>{CURRENCIES.map((c) => <option key={c}>{c}</option>)}</select></label>
        <label>Rate<input className="num" inputMode="decimal" value={f.rate} required onChange={(e) => setF({ ...f, rate: e.target.value })} /></label>
        <button className="secondary" disabled={busy}>Set rate</button>
      </form>
      <Notice state={notice} />
    </section>
  );
}

function Audit() {
  const [report, setReport] = useState(null);
  const [recon, setRecon] = useState(null);
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
        <label>Month<input type="month" value={period} onChange={(e) => setPeriod(e.target.value)} /></label>
        <button className="secondary" disabled={reconcile.busy} onClick={async () => setRecon(await reconcile.run(() => api.reconcile(period), "Reconciliation saved."))}>
          Reconcile month
        </button>
      </div>
      <Notice state={check.notice} />
      {report && (
        <div className={`notice ${report.ok ? "ok" : "err"}`}>
          {report.ok ? "Every currency sums to zero and every account matches its entries." : `${report.issues.length} problem(s) found.`}
          <table style={{ marginTop: ".5rem" }}>
            <thead><tr><th>Currency</th><th className="r">Sum of balances</th><th className="r">Expected</th></tr></thead>
            <tbody>{report.currency_totals.map((t) => <tr key={t.currency}><td>{t.currency}</td><td className="num r">{t.balance_sum}</td><td className="num r">{t.expected}</td></tr>)}</tbody>
          </table>
          <ul className="issues">{report.issues.map((i, n) => <li key={n}>{i.message}</li>)}</ul>
        </div>
      )}
      <Notice state={reconcile.notice} />
      {recon && (
        <div className={`notice ${recon.status === "clean" ? "ok" : "err"}`}>
          {recon.status === "clean"
            ? `${recon.accounts_checked} accounts reconciled for ${recon.period_start.slice(0, 7)} with no discrepancies.`
            : `${recon.discrepancy_count} discrepancies in ${recon.period_start.slice(0, 7)}.`}
          <ul className="issues">
            {recon.issues?.map((i, n) => <li key={"i" + n}>{i.message}</li>)}
            {recon.lines.flatMap((l) => l.discrepancies.map((d, n) => <li key={l.account_id + "-" + n}>Account #{l.account_id}: {d}</li>))}
          </ul>
        </div>
      )}
    </section>
  );
}

export default function App() {
  const [accounts, setAccounts] = useState([]);
  const [selected, setSelected] = useState(null);
  const [entries, setEntries] = useState([]);
  const [loadError, setLoadError] = useState(null);

  const refresh = useCallback(async () => {
    try {
      const list = await api.accounts();
      setAccounts(list);
      setLoadError(null);
      if (selected) setEntries(await api.history(selected));
    } catch (e) {
      setLoadError(e.message);
    }
  }, [selected]);

  useEffect(() => { refresh(); }, [refresh]);

  const account = accounts.find((a) => a.id === selected);

  return (
    <div className="app">
      <header className="top">
        <h1>Ledger</h1>
        <p>Every change is a balanced, permanent entry. Mistakes are fixed by reversal, never by edit.</p>
      </header>
      {loadError && <div className="notice err" role="alert">{loadError} Is the API running on port 8080?</div>}
      <div className="grid">
        <div className="stack">
          <AccountList accounts={accounts} selected={selected} onSelect={setSelected} />
          <NewAccount onCreated={(a) => { setSelected(a.id); refresh(); }} />
        </div>
        <div className="stack">
          <TransferForm accounts={accounts} defaultSource={selected} onDone={refresh} />
          {account ? <Journal account={account} entries={entries} onChanged={refresh} /> : <section className="card"><p className="empty">Select an account to see its balance and journal.</p></section>}
          <Rates />
          <Audit />
        </div>
      </div>
    </div>
  );
}
