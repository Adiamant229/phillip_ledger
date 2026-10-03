package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/Adiamant229/phillip_ledger/backend/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

const accountCols = `id, name, currency, balance, is_system, allow_negative, created_at`

func scanAccount(row pgx.Row) (domain.Account, error) {
	var a domain.Account
	err := row.Scan(&a.ID, &a.Name, &a.Currency, &a.Balance, &a.IsSystem, &a.AllowNegative, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, domain.ErrNotFound
	}
	return a, err
}

func (r Reader) GetAccount(ctx context.Context, id int64) (domain.Account, error) {
	return scanAccount(r.q.QueryRow(ctx, `SELECT `+accountCols+` FROM accounts WHERE id = $1`, id))
}

func (r Reader) ListAccounts(ctx context.Context, includeSystem bool) ([]domain.Account, error) {
	rows, err := r.q.Query(ctx, `SELECT `+accountCols+` FROM accounts WHERE ($1 OR NOT is_system) ORDER BY id`, includeSystem)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r Reader) SystemAccount(ctx context.Context, currency, role string) (domain.Account, error) {
	return scanAccount(r.q.QueryRow(ctx,
		`SELECT `+accountCols+` FROM accounts WHERE is_system AND currency = $1 AND system_role = $2`, currency, role))
}

func (r Reader) Currency(ctx context.Context, code string) (domain.Currency, error) {
	var c domain.Currency
	err := r.q.QueryRow(ctx, `SELECT code, exponent FROM currencies WHERE code = $1`, code).Scan(&c.Code, &c.Exponent)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, domain.ErrNotFound
	}
	return c, err
}

// LatestRate returns the newest direct base->quote rate, if any.
func (r Reader) LatestRate(ctx context.Context, base, quote string) (decimal.Decimal, bool, error) {
	var rate decimal.Decimal
	err := r.q.QueryRow(ctx, `SELECT rate FROM exchange_rates WHERE base = $1 AND quote = $2
		ORDER BY effective_at DESC, id DESC LIMIT 1`, base, quote).Scan(&rate)
	if errors.Is(err, pgx.ErrNoRows) {
		return rate, false, nil
	}
	return rate, err == nil, err
}

func (r Reader) ListRates(ctx context.Context) ([]domain.Rate, error) {
	rows, err := r.q.Query(ctx, `SELECT DISTINCT ON (base, quote) base, quote, rate, effective_at
		FROM exchange_rates ORDER BY base, quote, effective_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Rate{}
	for rows.Next() {
		var x domain.Rate
		if err := rows.Scan(&x.Base, &x.Quote, &x.Rate, &x.EffectiveAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

const txSelect = `SELECT t.id::text, t.kind, t.source_account_id, t.destination_account_id,
	t.source_amount, t.source_currency, t.destination_amount, t.destination_currency, t.fx_rate,
	t.reverses_transaction_id::text, rb.id::text, t.created_at, t.request_hash
	FROM transactions t LEFT JOIN transactions rb ON rb.reverses_transaction_id = t.id `

func scanTx(row pgx.Row) (domain.Transaction, string, error) {
	var t domain.Transaction
	var hash string
	err := row.Scan(&t.ID, &t.Kind, &t.SourceAccountID, &t.DestinationAccountID,
		&t.SourceAmount, &t.SourceCurrency, &t.DestinationAmount, &t.DestinationCurrency, &t.FXRate,
		&t.ReversesTransactionID, &t.ReversedByTransactionID, &t.CreatedAt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, "", domain.ErrNotFound
	}
	return t, hash, err
}

func (r Reader) GetTransaction(ctx context.Context, id string) (domain.Transaction, error) {
	t, _, err := scanTx(r.q.QueryRow(ctx, txSelect+`WHERE t.id = $1::text::uuid`, id))
	return t, err
}

// GetTransactionByKey also returns the stored request hash so callers can detect key reuse.
func (r Reader) GetTransactionByKey(ctx context.Context, key string) (domain.Transaction, string, error) {
	return scanTx(r.q.QueryRow(ctx, txSelect+`WHERE t.idempotency_key = $1`, key))
}

func (r Reader) TransactionLegs(ctx context.Context, txID string) ([]domain.Leg, error) {
	rows, err := r.q.Query(ctx, `SELECT account_id, currency, amount FROM ledger_entries
		WHERE transaction_id = $1::text::uuid ORDER BY id`, txID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Leg
	for rows.Next() {
		var l domain.Leg
		if err := rows.Scan(&l.AccountID, &l.Currency, &l.Amount); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ListEntries returns an account's journal ordered by entry id. Entry ids are assigned while the account row
// is locked, so id order is exactly the order balances were applied (and matches timestamp order).
func (r Reader) ListEntries(ctx context.Context, accountID int64, limit int, cursor int64, desc bool) ([]domain.Entry, error) {
	q := `SELECT e.id, e.transaction_id::text, t.kind, e.amount, e.currency, e.balance_after,
		t.source_account_id, t.destination_account_id, rb.id::text, e.created_at
		FROM ledger_entries e
		JOIN transactions t ON t.id = e.transaction_id
		LEFT JOIN transactions rb ON rb.reverses_transaction_id = t.id
		WHERE e.account_id = $1`
	args := []any{accountID}
	order := "ASC"
	if desc {
		order = "DESC"
	}
	if cursor > 0 {
		op := ">"
		if desc {
			op = "<"
		}
		q += " AND e.id " + op + " $2"
		args = append(args, cursor)
	}
	q += fmt.Sprintf(" ORDER BY e.id %s LIMIT %d", order, limit)

	rows, err := r.q.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Entry{}
	for rows.Next() {
		var e domain.Entry
		if err := rows.Scan(&e.EntryID, &e.TransactionID, &e.Kind, &e.Amount, &e.Currency, &e.BalanceAfter,
			&e.SourceAccountID, &e.DestinationAccountID, &e.ReversedBy, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
