// Package domain holds the types and sentinel errors shared by every layer.
package domain

import (
	"errors"
	"regexp"
	"time"

	"github.com/shopspring/decimal"
)

var (
	ErrNotFound            = errors.New("not found")
	ErrInvalid             = errors.New("invalid request")
	ErrInsufficientFunds   = errors.New("insufficient funds")
	ErrIdempotencyConflict = errors.New("idempotency key already used with a different request")
	ErrAlreadyReversed     = errors.New("transaction already reversed")
	ErrCannotReverse       = errors.New("transaction cannot be reversed")
	ErrNoRate              = errors.New("no exchange rate available")
)

const (
	KindTransfer = "transfer"
	KindDeposit  = "deposit"
	KindReversal = "reversal"

	RoleFunding = "funding"
	RoleFX      = "fx"
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func IsUUID(s string) bool { return uuidRe.MatchString(s) }

type Account struct {
	ID            int64           `json:"id"`
	Name          string          `json:"name"`
	Currency      string          `json:"currency"`
	Balance       decimal.Decimal `json:"balance"`
	IsSystem      bool            `json:"is_system"`
	AllowNegative bool            `json:"allow_negative"`
	CreatedAt     time.Time       `json:"created_at"`
}

type Currency struct {
	Code     string `json:"code"`
	Exponent int    `json:"exponent"`
}

type Transaction struct {
	ID                      string           `json:"id"`
	Kind                    string           `json:"kind"`
	SourceAccountID         int64            `json:"source_account_id"`
	DestinationAccountID    int64            `json:"destination_account_id"`
	SourceAmount            decimal.Decimal  `json:"source_amount"`
	SourceCurrency          string           `json:"source_currency"`
	DestinationAmount       decimal.Decimal  `json:"destination_amount"`
	DestinationCurrency     string           `json:"destination_currency"`
	FXRate                  *decimal.Decimal `json:"fx_rate,omitempty"`
	ReversesTransactionID   *string          `json:"reverses_transaction_id,omitempty"`
	ReversedByTransactionID *string          `json:"reversed_by_transaction_id,omitempty"`
	CreatedAt               time.Time        `json:"created_at"`
}

// Leg is one signed movement on one account; a transaction is a set of legs that net to zero per currency.
type Leg struct {
	AccountID int64
	Currency  string
	Amount    decimal.Decimal
}

// Entry is one row of an account's journal.
type Entry struct {
	EntryID              int64           `json:"entry_id"`
	TransactionID        string          `json:"transaction_id"`
	Kind                 string          `json:"kind"`
	Amount               decimal.Decimal `json:"amount"` // signed: + credit, - debit
	Currency             string          `json:"currency"`
	BalanceAfter         decimal.Decimal `json:"balance_after"`
	SourceAccountID      int64           `json:"source_account_id"`
	DestinationAccountID int64           `json:"destination_account_id"`
	ReversedBy           *string         `json:"reversed_by_transaction_id,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
}

type Rate struct {
	Base        string          `json:"base"`
	Quote       string          `json:"quote"`
	Rate        decimal.Decimal `json:"rate"`
	EffectiveAt time.Time       `json:"effective_at"`
}

type Issue struct {
	Check   string `json:"check"`
	Message string `json:"message"`
}

type CurrencyTotal struct {
	Currency   string          `json:"currency"`
	BalanceSum decimal.Decimal `json:"balance_sum"`
	EntriesSum decimal.Decimal `json:"entries_sum"`
	Expected   decimal.Decimal `json:"expected"`
	OK         bool            `json:"ok"`
}

type IntegrityReport struct {
	CheckedAt      time.Time       `json:"checked_at"`
	OK             bool            `json:"ok"`
	CurrencyTotals []CurrencyTotal `json:"currency_totals"`
	Issues         []Issue         `json:"issues"`
}

type ReconLine struct {
	AccountID             int64           `json:"account_id"`
	AccountName           string          `json:"account_name"`
	Currency              string          `json:"currency"`
	Opening               decimal.Decimal `json:"opening"`
	Credits               decimal.Decimal `json:"credits"`
	Debits                decimal.Decimal `json:"debits"`
	Closing               decimal.Decimal `json:"closing"`          // opening + credits - debits, from entries
	RecordedClosing       decimal.Decimal `json:"recorded_closing"` // balance_after of the last entry in the period
	CurrentBalance        decimal.Decimal `json:"current_balance"`
	CheckedAgainstCurrent bool            `json:"checked_against_current"`
	Discrepancies         []string        `json:"discrepancies"`
}

type Reconciliation struct {
	ID               int64       `json:"id"`
	PeriodStart      time.Time   `json:"period_start"`
	PeriodEnd        time.Time   `json:"period_end"`
	Status           string      `json:"status"`
	AccountsChecked  int         `json:"accounts_checked"`
	DiscrepancyCount int         `json:"discrepancy_count"`
	Lines            []ReconLine `json:"lines,omitempty"`
	Issues           []Issue     `json:"issues,omitempty"`
	CreatedAt        time.Time   `json:"created_at"`
}
