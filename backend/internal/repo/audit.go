package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Adiamant229/phillip_ledger/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
)

func (s *Store) InsertRate(ctx context.Context, base, quote string, rate decimal.Decimal) (domain.Rate, error) {
	var r domain.Rate
	err := s.pool.QueryRow(ctx, `INSERT INTO exchange_rates (base, quote, rate) VALUES ($1, $2, $3)
		RETURNING base, quote, rate, effective_at`, base, quote, rate).Scan(&r.Base, &r.Quote, &r.Rate, &r.EffectiveAt)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23503" {
		return r, domain.ErrNotFound // unknown currency
	}
	return r, err
}

func (r Reader) sumByCurrency(ctx context.Context, q string) (map[string]decimal.Decimal, error) {
	rows, err := r.q.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]decimal.Decimal{}
	for rows.Next() {
		var c string
		var d decimal.Decimal
		if err := rows.Scan(&c, &d); err != nil {
			return nil, err
		}
		m[c] = d
	}
	return m, rows.Err()
}

func (r Reader) BalanceTotals(ctx context.Context) (map[string]decimal.Decimal, error) {
	return r.sumByCurrency(ctx, `SELECT currency, SUM(balance) FROM accounts GROUP BY currency`)
}

func (r Reader) EntryTotals(ctx context.Context) (map[string]decimal.Decimal, error) {
	return r.sumByCurrency(ctx, `SELECT currency, SUM(amount) FROM ledger_entries GROUP BY currency`)
}

var integrityChecks = []struct{ name, query string }{
	{"unbalanced_transaction", `SELECT format('transaction %s nets to %s %s (must be 0)', transaction_id, SUM(amount), currency)
		FROM ledger_entries GROUP BY transaction_id, currency HAVING SUM(amount) <> 0`},
	{"balance_mismatch", `SELECT format('account %s: stored balance %s but its entries sum to %s', a.id, a.balance, COALESCE(s.total, 0))
		FROM accounts a
		LEFT JOIN (SELECT account_id, SUM(amount) AS total FROM ledger_entries GROUP BY account_id) s ON s.account_id = a.id
		WHERE a.balance <> COALESCE(s.total, 0)`},
	{"running_balance_break", `SELECT format('entry %s on account %s: balance_after %s but running sum is %s', id, account_id, balance_after, running)
		FROM (SELECT id, account_id, balance_after,
		             SUM(amount) OVER (PARTITION BY account_id ORDER BY id) AS running FROM ledger_entries) x
		WHERE balance_after <> running`},
	{"overdrawn_account", `SELECT format('account %s is overdrawn: %s', id, balance) FROM accounts WHERE balance < 0 AND NOT allow_negative`},
	{"currency_mismatch", `SELECT format('entry %s is in %s but account %s is %s', e.id, e.currency, a.id, a.currency)
		FROM ledger_entries e JOIN accounts a ON a.id = e.account_id WHERE e.currency <> a.currency`},
	{"bad_reversal", `SELECT format('reversal %s does not exactly cancel transaction %s', t.id, t.reverses_transaction_id)
		FROM transactions t WHERE t.kind = 'reversal' AND EXISTS (
			SELECT 1 FROM ledger_entries e WHERE e.transaction_id IN (t.id, t.reverses_transaction_id)
			GROUP BY e.account_id HAVING SUM(e.amount) <> 0)`},
	{"transaction_without_entries", `SELECT format('transaction %s has no ledger entries', t.id) FROM transactions t
		WHERE NOT EXISTS (SELECT 1 FROM ledger_entries e WHERE e.transaction_id = t.id)`},
}

func (r Reader) IntegrityIssues(ctx context.Context) ([]domain.Issue, error) {
	issues := []domain.Issue{}
	for _, c := range integrityChecks {
		rows, err := r.q.Query(ctx, c.query)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var msg string
			if err := rows.Scan(&msg); err != nil {
				rows.Close()
				return nil, err
			}
			issues = append(issues, domain.Issue{Check: c.name, Message: msg})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return issues, nil
}

// ReconcileRows computes, per account, opening balance, credits, debits and closing balance for [start, end)
// straight from the journal, plus the balance_after recorded on the last entry before end.
func (r Reader) ReconcileRows(ctx context.Context, start, end time.Time) ([]domain.ReconLine, error) {
	rows, err := r.q.Query(ctx, `
		SELECT a.id, a.name, a.currency, a.balance,
		  COALESCE(SUM(e.amount) FILTER (WHERE e.created_at < $1), 0),
		  COALESCE(SUM(e.amount) FILTER (WHERE e.created_at >= $1 AND e.created_at < $2 AND e.amount > 0), 0),
		  COALESCE(-SUM(e.amount) FILTER (WHERE e.created_at >= $1 AND e.created_at < $2 AND e.amount < 0), 0),
		  COALESCE(SUM(e.amount) FILTER (WHERE e.created_at < $2), 0),
		  COUNT(e.id) FILTER (WHERE e.created_at >= $2),
		  COALESCE((SELECT e2.balance_after FROM ledger_entries e2
		            WHERE e2.account_id = a.id AND e2.created_at < $2 ORDER BY e2.id DESC LIMIT 1), 0)
		FROM accounts a LEFT JOIN ledger_entries e ON e.account_id = a.id
		WHERE a.created_at < $2
		GROUP BY a.id ORDER BY a.id`, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.ReconLine{}
	for rows.Next() {
		var l domain.ReconLine
		var after int64
		if err := rows.Scan(&l.AccountID, &l.AccountName, &l.Currency, &l.CurrentBalance,
			&l.Opening, &l.Credits, &l.Debits, &l.Closing, &after, &l.RecordedClosing); err != nil {
			return nil, err
		}
		l.CheckedAgainstCurrent = after == 0 // nothing posted after the period, so closing must equal today's balance
		l.Discrepancies = []string{}
		out = append(out, l)
	}
	return out, rows.Err()
}

type reconBody struct {
	Lines  []domain.ReconLine `json:"lines"`
	Issues []domain.Issue     `json:"issues"`
}

func (s *Store) SaveReconciliation(ctx context.Context, rec *domain.Reconciliation) error {
	body, err := json.Marshal(reconBody{Lines: rec.Lines, Issues: rec.Issues})
	if err != nil {
		return err
	}
	return s.pool.QueryRow(ctx, `INSERT INTO reconciliation_runs
		(period_start, period_end, status, accounts_checked, discrepancy_count, report)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at`,
		rec.PeriodStart, rec.PeriodEnd, rec.Status, rec.AccountsChecked, rec.DiscrepancyCount, body,
	).Scan(&rec.ID, &rec.CreatedAt)
}

func (s *Store) ListReconciliations(ctx context.Context) ([]domain.Reconciliation, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, period_start, period_end, status, accounts_checked, discrepancy_count, created_at
		FROM reconciliation_runs ORDER BY id DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Reconciliation{}
	for rows.Next() {
		var x domain.Reconciliation
		if err := rows.Scan(&x.ID, &x.PeriodStart, &x.PeriodEnd, &x.Status, &x.AccountsChecked, &x.DiscrepancyCount, &x.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) GetReconciliation(ctx context.Context, id int64) (domain.Reconciliation, error) {
	var x domain.Reconciliation
	var body []byte
	err := s.pool.QueryRow(ctx, `SELECT id, period_start, period_end, status, accounts_checked, discrepancy_count, report, created_at
		FROM reconciliation_runs WHERE id = $1`, id).
		Scan(&x.ID, &x.PeriodStart, &x.PeriodEnd, &x.Status, &x.AccountsChecked, &x.DiscrepancyCount, &body, &x.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return x, domain.ErrNotFound
	}
	if err != nil {
		return x, err
	}
	var b reconBody
	if err := json.Unmarshal(body, &b); err != nil {
		return x, err
	}
	x.Lines, x.Issues = b.Lines, b.Issues
	return x, nil
}
