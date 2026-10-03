package repo

import (
	"context"
	"errors"
	"sort"

	"github.com/Adiamant229/phillip_ledger/backend/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

func (t *Tx) CreateAccount(ctx context.Context, name, currency string) (domain.Account, error) {
	return scanAccount(t.tx.QueryRow(ctx,
		`INSERT INTO accounts (name, currency) VALUES ($1, $2) RETURNING `+accountCols, name, currency))
}

// LockAccounts takes row locks (SELECT ... FOR UPDATE) one at a time in ascending id order and returns the
// freshly-read rows. A single global lock order means two transfers can never wait on each other in a cycle,
// and every balance check afterwards is made against rows nobody else can change until we commit.
func (t *Tx) LockAccounts(ctx context.Context, ids []int64) (map[int64]domain.Account, error) {
	seen := map[int64]bool{}
	sorted := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			sorted = append(sorted, id)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	out := make(map[int64]domain.Account, len(sorted))
	for _, id := range sorted {
		a, err := scanAccount(t.tx.QueryRow(ctx, `SELECT `+accountCols+` FROM accounts WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return nil, err
		}
		out[id] = a
	}
	return out, nil
}

// InsertTransaction inserts the header row. inserted=false means the idempotency key already exists
// (possibly committed by a concurrent request a moment ago); nothing was written in that case.
func (t *Tx) InsertTransaction(ctx context.Context, tr *domain.Transaction, key, hash string) (bool, error) {
	err := t.tx.QueryRow(ctx, `INSERT INTO transactions
		(idempotency_key, request_hash, kind, source_account_id, destination_account_id,
		 source_amount, source_currency, destination_amount, destination_currency, fx_rate, reverses_transaction_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::text::uuid)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id::text, created_at`,
		key, hash, tr.Kind, tr.SourceAccountID, tr.DestinationAccountID,
		tr.SourceAmount, tr.SourceCurrency, tr.DestinationAmount, tr.DestinationCurrency, tr.FXRate, tr.ReversesTransactionID,
	).Scan(&tr.ID, &tr.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if code, constraint := pgErrInfo(err); code == "23505" && constraint == "transactions_reverses_transaction_id_key" {
		return false, domain.ErrAlreadyReversed
	}
	return err == nil, err
}

// ApplyLeg moves one account's balance and appends the matching journal entry in the same transaction.
func (t *Tx) ApplyLeg(ctx context.Context, txID string, l domain.Leg) error {
	var after decimal.Decimal
	err := t.tx.QueryRow(ctx, `UPDATE accounts SET balance = balance + $2 WHERE id = $1 RETURNING balance`,
		l.AccountID, l.Amount).Scan(&after)
	if code, _ := pgErrInfo(err); code == "23514" { // CHECK (allow_negative OR balance >= 0)
		return domain.ErrInsufficientFunds
	}
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx, `INSERT INTO ledger_entries (transaction_id, account_id, currency, amount, balance_after)
		VALUES ($1::text::uuid, $2, $3, $4, $5)`, txID, l.AccountID, l.Currency, l.Amount, after)
	return err
}
