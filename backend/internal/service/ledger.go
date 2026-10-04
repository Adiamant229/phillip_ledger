// Package service holds all business rules. It decides *what* must be true (validation, FX, overdraft,
// idempotency, reversals); the repo layer knows *how* to persist it.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dylan/ledger/internal/domain"
	"github.com/dylan/ledger/internal/repo"
	"github.com/shopspring/decimal"
)

type Service struct {
	store *repo.Store
	now   func() time.Time
}

func New(store *repo.Store) *Service { return &Service{store: store, now: time.Now} }

type TransferRequest struct {
	Key                  string
	SourceAccountID      int64
	DestinationAccountID int64
	Amount               decimal.Decimal // in the source account's currency
	// ExchangeRate is "1 source currency = ExchangeRate destination currency", chosen by the caller (the UI
	// suggests one and lets the user edit it). Optional: when nil, the stored USD rates are divided instead.
	// Must be nil for same-currency transfers.
	ExchangeRate *decimal.Decimal
}

type Result struct {
	Transaction domain.Transaction
	Replayed    bool // true when an earlier request with the same idempotency key is being returned
}

var maxAmount = decimal.RequireFromString("1000000000000") // 1e12, far below the NUMERIC(28,8) limit

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", domain.ErrInvalid, fmt.Sprintf(format, args...))
}

func validateKey(key string) error {
	if k := strings.TrimSpace(key); k == "" || len(k) > 255 || k != key {
		return invalid("Idempotency-Key is required (1-255 chars, no surrounding spaces)")
	}
	return nil
}

func checkAmount(a decimal.Decimal, exponent int) error {
	if !a.IsPositive() {
		return invalid("amount must be greater than zero")
	}
	if a.GreaterThanOrEqual(maxAmount) {
		return invalid("amount is too large")
	}
	if !a.Truncate(int32(exponent)).Equal(a) {
		return invalid("amount has more than %d decimal places for this currency", exponent)
	}
	return nil
}

func hashParts(parts ...any) string {
	h := sha256.Sum256([]byte(fmt.Sprint(parts...)))
	return hex.EncodeToString(h[:])
}

// posting is a fully-planned transaction: header + legs. Building it is read-only; committing it takes locks.
type posting struct {
	tr   domain.Transaction
	legs []domain.Leg
}

// ---- Accounts -------------------------------------------------------------------------------------------

func (s *Service) CreateAccount(ctx context.Context, name, currency string, initial decimal.Decimal) (domain.Account, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return domain.Account{}, invalid("name is required (max 100 chars)")
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	cur, err := s.store.Currency(ctx, currency)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Account{}, invalid("unsupported currency %q", currency)
	} else if err != nil {
		return domain.Account{}, err
	}
	if initial.IsNegative() {
		return domain.Account{}, invalid("initial balance cannot be negative")
	}
	if initial.IsPositive() {
		if err := checkAmount(initial, cur.Exponent); err != nil {
			return domain.Account{}, err
		}
	}

	var acct domain.Account
	err = s.store.WithTx(ctx, func(tx *repo.Tx) error {
		a, err := tx.CreateAccount(ctx, name, currency)
		if err != nil {
			return err
		}
		if initial.IsPositive() { // opening balance is a normal, journaled deposit from the funding account
			key := fmt.Sprintf("open-account:%d", a.ID)
			hash := hashParts(domain.KindDeposit, a.ID, initial.StringFixed(8))
			if _, err := s.executeIn(ctx, tx, key, hash, func() (*posting, error) { return s.buildDeposit(ctx, tx, a.ID, initial) }); err != nil {
				return err
			}
		}
		acct, err = tx.GetAccount(ctx, a.ID)
		return err
	})
	return acct, err
}

func (s *Service) GetAccount(ctx context.Context, id int64) (domain.Account, error) {
	return s.store.GetAccount(ctx, id)
}

func (s *Service) ListAccounts(ctx context.Context, includeSystem bool) ([]domain.Account, error) {
	return s.store.ListAccounts(ctx, includeSystem)
}

func (s *Service) History(ctx context.Context, accountID int64, limit int, cursor int64, desc bool) ([]domain.Entry, error) {
	if _, err := s.store.GetAccount(ctx, accountID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.store.ListEntries(ctx, accountID, limit, cursor, desc)
}

func (s *Service) GetTransaction(ctx context.Context, id string) (domain.Transaction, error) {
	if !domain.IsUUID(id) {
		return domain.Transaction{}, domain.ErrNotFound
	}
	return s.store.GetTransaction(ctx, strings.ToLower(id))
}

// ---- Money movement -------------------------------------------------------------------------------------

func (s *Service) Transfer(ctx context.Context, req TransferRequest) (Result, error) {
	if err := validateKey(req.Key); err != nil {
		return Result{}, err
	}
	if req.SourceAccountID == req.DestinationAccountID {
		return Result{}, invalid("source and destination accounts must differ")
	}
	if !req.Amount.IsPositive() {
		return Result{}, invalid("amount must be greater than zero")
	}
	rateKey := "rate=none" // the rate is part of the request: same key + different rate must not be a replay
	if req.ExchangeRate != nil {
		rateKey = "rate=" + req.ExchangeRate.StringFixed(12)
	}
	hash := hashParts(domain.KindTransfer, req.SourceAccountID, req.DestinationAccountID, req.Amount.StringFixed(8), rateKey)
	return s.execute(ctx, req.Key, hash, func(tx *repo.Tx) (*posting, error) { return s.buildTransfer(ctx, tx, req) })
}

func (s *Service) Deposit(ctx context.Context, key string, accountID int64, amount decimal.Decimal) (Result, error) {
	if err := validateKey(key); err != nil {
		return Result{}, err
	}
	if !amount.IsPositive() {
		return Result{}, invalid("amount must be greater than zero")
	}
	hash := hashParts(domain.KindDeposit, accountID, amount.StringFixed(8))
	return s.execute(ctx, key, hash, func(tx *repo.Tx) (*posting, error) { return s.buildDeposit(ctx, tx, accountID, amount) })
}

func (s *Service) Reverse(ctx context.Context, key, txID string) (Result, error) {
	if err := validateKey(key); err != nil {
		return Result{}, err
	}
	if !domain.IsUUID(txID) {
		return Result{}, domain.ErrNotFound
	}
	txID = strings.ToLower(txID)
	hash := hashParts(domain.KindReversal, txID)
	return s.execute(ctx, key, hash, func(tx *repo.Tx) (*posting, error) { return s.buildReversal(ctx, tx, txID) })
}

func (s *Service) execute(ctx context.Context, key, hash string, build func(*repo.Tx) (*posting, error)) (Result, error) {
	var res Result
	err := s.store.WithTx(ctx, func(tx *repo.Tx) error {
		var err error
		res, err = s.executeIn(ctx, tx, key, hash, func() (*posting, error) { return build(tx) })
		return err
	})
	return res, err
}

// executeIn is the single path every money movement takes:
//  1. idempotency lookup  2. plan (read-only)  3. lock accounts in id order  4. balance checks
//  5. insert header (unique key is the final idempotency arbiter)  6. apply legs.
func (s *Service) executeIn(ctx context.Context, tx *repo.Tx, key, hash string, build func() (*posting, error)) (Result, error) {
	if r, ok, err := s.replay(ctx, tx, key, hash); ok || err != nil {
		return r, err
	}
	p, err := build()
	if err != nil {
		return Result{}, err
	}

	ids := make([]int64, 0, len(p.legs))
	for _, l := range p.legs {
		ids = append(ids, l.AccountID)
	}
	locked, err := tx.LockAccounts(ctx, ids)
	if err != nil {
		return Result{}, err
	}
	net := map[int64]decimal.Decimal{}
	for _, l := range p.legs {
		net[l.AccountID] = net[l.AccountID].Add(l.Amount)
	}
	for id, n := range net {
		a := locked[id] // balances here are post-lock, so nobody can change them before we commit
		if !a.AllowNegative && a.Balance.Add(n).IsNegative() {
			return Result{}, fmt.Errorf("%w: account %d has %s %s, needs %s more", domain.ErrInsufficientFunds,
				id, a.Balance.String(), a.Currency, a.Balance.Add(n).Neg().String())
		}
	}

	inserted, err := tx.InsertTransaction(ctx, &p.tr, key, hash)
	if err != nil {
		return Result{}, err
	}
	if !inserted { // lost a race with an identical request that committed first; nothing has been modified
		r, ok, err := s.replay(ctx, tx, key, hash)
		if err == nil && !ok {
			err = errors.New("idempotency key conflict could not be resolved")
		}
		return r, err
	}
	for _, l := range p.legs {
		if err := tx.ApplyLeg(ctx, p.tr.ID, l); err != nil {
			return Result{}, err
		}
	}
	return Result{Transaction: p.tr}, nil
}

func (s *Service) replay(ctx context.Context, tx *repo.Tx, key, hash string) (Result, bool, error) {
	t, stored, err := tx.GetTransactionByKey(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if stored != hash {
		return Result{}, false, domain.ErrIdempotencyConflict
	}
	return Result{Transaction: t, Replayed: true}, true, nil
}

func (s *Service) buildTransfer(ctx context.Context, tx *repo.Tx, req TransferRequest) (*posting, error) {
	src, err := tx.GetAccount(ctx, req.SourceAccountID)
	if err != nil {
		return nil, fmt.Errorf("source account: %w", err)
	}
	dst, err := tx.GetAccount(ctx, req.DestinationAccountID)
	if err != nil {
		return nil, fmt.Errorf("destination account: %w", err)
	}
	if src.IsSystem || dst.IsSystem {
		return nil, invalid("system accounts cannot be used in transfers")
	}
	srcCur, err := tx.Currency(ctx, src.Currency)
	if err != nil {
		return nil, err
	}
	if err := checkAmount(req.Amount, srcCur.Exponent); err != nil {
		return nil, err
	}

	p := &posting{tr: domain.Transaction{
		Kind: domain.KindTransfer, SourceAccountID: src.ID, DestinationAccountID: dst.ID,
		SourceAmount: req.Amount, SourceCurrency: src.Currency,
		DestinationAmount: req.Amount, DestinationCurrency: dst.Currency,
	}}
	if src.Currency == dst.Currency {
		if req.ExchangeRate != nil {
			return nil, invalid("exchange_rate must be omitted when both accounts use the same currency")
		}
		p.legs = []domain.Leg{
			{AccountID: src.ID, Currency: src.Currency, Amount: req.Amount.Neg()},
			{AccountID: dst.ID, Currency: dst.Currency, Amount: req.Amount},
		}
		return p, nil
	}

	// Cross-currency: route through the per-currency FX clearing accounts so every currency still nets to zero.
	//   src  -A  (src ccy)      fxSrc +A (src ccy)
	//   fxDst -B (dst ccy)      dst   +B (dst ccy)         where B = round_half_even(A * rate, dst exponent)
	rate, err := s.transferRate(ctx, tx, req.ExchangeRate, src.Currency, dst.Currency)
	if err != nil {
		return nil, err
	}
	dstCur, err := tx.Currency(ctx, dst.Currency)
	if err != nil {
		return nil, err
	}
	converted := req.Amount.Mul(rate).RoundBank(int32(dstCur.Exponent))
	if !converted.IsPositive() {
		return nil, invalid("amount converts to zero %s at rate %s", dst.Currency, rate.String())
	}
	fxSrc, err := tx.SystemAccount(ctx, src.Currency, domain.RoleFX)
	if err != nil {
		return nil, err
	}
	fxDst, err := tx.SystemAccount(ctx, dst.Currency, domain.RoleFX)
	if err != nil {
		return nil, err
	}
	p.tr.DestinationAmount = converted
	p.tr.FXRate = &rate
	p.legs = []domain.Leg{
		{AccountID: src.ID, Currency: src.Currency, Amount: req.Amount.Neg()},
		{AccountID: fxSrc.ID, Currency: src.Currency, Amount: req.Amount},
		{AccountID: fxDst.ID, Currency: dst.Currency, Amount: converted.Neg()},
		{AccountID: dst.ID, Currency: dst.Currency, Amount: converted},
	}
	return p, nil
}

func (s *Service) buildDeposit(ctx context.Context, tx *repo.Tx, accountID int64, amount decimal.Decimal) (*posting, error) {
	acct, err := tx.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if acct.IsSystem {
		return nil, invalid("cannot deposit into a system account")
	}
	cur, err := tx.Currency(ctx, acct.Currency)
	if err != nil {
		return nil, err
	}
	if err := checkAmount(amount, cur.Exponent); err != nil {
		return nil, err
	}
	funding, err := tx.SystemAccount(ctx, acct.Currency, domain.RoleFunding)
	if err != nil {
		return nil, err
	}
	return &posting{
		tr: domain.Transaction{
			Kind: domain.KindDeposit, SourceAccountID: funding.ID, DestinationAccountID: acct.ID,
			SourceAmount: amount, SourceCurrency: acct.Currency, DestinationAmount: amount, DestinationCurrency: acct.Currency,
		},
		legs: []domain.Leg{
			{AccountID: funding.ID, Currency: acct.Currency, Amount: amount.Neg()},
			{AccountID: acct.ID, Currency: acct.Currency, Amount: amount},
		},
	}, nil
}

// buildReversal posts the exact negation of every original leg (same accounts, same amounts, original FX
// rate). The original rows are never touched.
func (s *Service) buildReversal(ctx context.Context, tx *repo.Tx, origID string) (*posting, error) {
	orig, err := tx.GetTransaction(ctx, origID)
	if err != nil {
		return nil, err
	}
	if orig.Kind == domain.KindReversal {
		return nil, fmt.Errorf("%w: a reversal cannot itself be reversed; post a new transfer instead", domain.ErrCannotReverse)
	}
	if orig.ReversedByTransactionID != nil {
		return nil, domain.ErrAlreadyReversed
	}
	legs, err := tx.TransactionLegs(ctx, origID)
	if err != nil {
		return nil, err
	}
	p := &posting{tr: domain.Transaction{
		Kind:                  domain.KindReversal,
		SourceAccountID:       orig.DestinationAccountID,
		DestinationAccountID:  orig.SourceAccountID,
		SourceAmount:          orig.DestinationAmount,
		SourceCurrency:        orig.DestinationCurrency,
		DestinationAmount:     orig.SourceAmount,
		DestinationCurrency:   orig.SourceCurrency,
		FXRate:                orig.FXRate,
		ReversesTransactionID: &origID,
	}}
	for _, l := range legs {
		p.legs = append(p.legs, domain.Leg{AccountID: l.AccountID, Currency: l.Currency, Amount: l.Amount.Neg()})
	}
	return p, nil
}

// ---- Exchange rates -------------------------------------------------------------------------------------

// pivotCurrency is the one currency every stored rate is quoted against: "1 USD = r X".
const pivotCurrency = "USD"

// checkRate validates a rate that came from a caller: positive, not absurdly large, and at most 12 decimal places
// (the precision stored on the transaction), so the rate used for the conversion is exactly the rate on record.
func checkRate(r decimal.Decimal, what string) error {
	if !r.IsPositive() {
		return invalid("%s must be greater than zero", what)
	}
	if r.GreaterThanOrEqual(maxAmount) {
		return invalid("%s is too large", what)
	}
	if !r.Truncate(12).Equal(r) {
		return invalid("%s has more than 12 decimal places", what)
	}
	return nil
}

// transferRate picks the rate for "1 base = ? quote": the caller's own rate when given (it is trusted: the amount
// the destination receives is derived from it here, so the ledger balances whatever it is), otherwise the stored
// USD rates divided.
func (s *Service) transferRate(ctx context.Context, tx *repo.Tx, supplied *decimal.Decimal, base, quote string) (decimal.Decimal, error) {
	if supplied != nil {
		if err := checkRate(*supplied, "exchange_rate"); err != nil {
			return decimal.Zero, err
		}
		return *supplied, nil
	}
	return s.storedRate(ctx, tx, base, quote)
}

// storedRate derives "1 base = ? quote" from the stored USD rates, assuming triangulation through USD loses
// nothing: (quote per USD) / (base per USD), rounded ONCE to 12 dp. USD itself is exactly 1 per USD, so USD->X is
// the stored rate unchanged and X->USD is its inverse.
func (s *Service) storedRate(ctx context.Context, tx *repo.Tx, base, quote string) (decimal.Decimal, error) {
	perBase, err := s.perPivot(ctx, tx, base)
	if err != nil {
		return decimal.Zero, err
	}
	perQuote, err := s.perPivot(ctx, tx, quote)
	if err != nil {
		return decimal.Zero, err
	}
	rate := perQuote.DivRound(perBase, 12)
	if !rate.IsPositive() {
		return decimal.Zero, fmt.Errorf("%w: %s -> %s is too small to represent at 12 decimal places", domain.ErrNoRate, base, quote)
	}
	return rate, nil
}

// perPivot returns how many units of cur equal 1 USD.
func (s *Service) perPivot(ctx context.Context, tx *repo.Tx, cur string) (decimal.Decimal, error) {
	if cur == pivotCurrency {
		return decimal.NewFromInt(1), nil
	}
	r, ok, err := tx.UsdRate(ctx, cur)
	if err != nil {
		return decimal.Zero, err
	}
	if !ok {
		return decimal.Zero, fmt.Errorf("%w for %s: set a USD->%s rate, or include exchange_rate in the request", domain.ErrNoRate, cur, cur)
	}
	return r, nil
}

// SetRate stores "1 USD = rate quote". There is at most one rate per currency (setting it again overwrites), and
// USD has none, so the table can never hold more than one row per non-USD currency.
func (s *Service) SetRate(ctx context.Context, quote string, rate decimal.Decimal) (domain.Rate, error) {
	quote = strings.ToUpper(strings.TrimSpace(quote))
	if quote == pivotCurrency {
		return domain.Rate{}, invalid("rates are quoted against %s, so %s itself has no rate", pivotCurrency, pivotCurrency)
	}
	if err := checkRate(rate, "rate"); err != nil {
		return domain.Rate{}, err
	}
	r, err := s.store.UpsertRate(ctx, quote, rate)
	if errors.Is(err, domain.ErrNotFound) {
		return r, invalid("unsupported currency %q", quote)
	}
	return r, err
}

func (s *Service) ListRates(ctx context.Context) ([]domain.Rate, error) {
	return s.store.ListRates(ctx)
}
