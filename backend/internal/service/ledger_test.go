package service_test

// Integration tests: need a real Postgres. Run with
//   TEST_DATABASE_URL=postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable go test -race ./...
// They create new accounts and overwrite a few USD exchange rates (JPY, SGD, PKR, EUR), so use a dev database.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dylan/ledger/internal/domain"
	"github.com/dylan/ledger/internal/repo"
	"github.com/dylan/ledger/internal/service"
	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func setup(t *testing.T) (*service.Service, context.Context) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	store, err := repo.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return service.New(store), ctx
}

func key(t *testing.T, n any) string {
	return fmt.Sprintf("%s-%d-%v", t.Name(), time.Now().UnixNano(), n)
}

func mustAccount(t *testing.T, s *service.Service, ctx context.Context, cur, initial string) domain.Account {
	t.Helper()
	a, err := s.CreateAccount(ctx, "test "+t.Name(), cur, dec(initial))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func balance(t *testing.T, s *service.Service, ctx context.Context, id int64) decimal.Decimal {
	t.Helper()
	a, err := s.GetAccount(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return a.Balance
}

func requireIntegrity(t *testing.T, s *service.Service, ctx context.Context) {
	t.Helper()
	rep, err := s.CheckIntegrity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("integrity check failed: %+v", rep)
	}
}

func TestConcurrentTransfersLoseNothing(t *testing.T) {
	s, ctx := setup(t)
	a, b := mustAccount(t, s, ctx, "USD", "1000.00"), mustAccount(t, s, ctx, "USD", "0")

	var wg sync.WaitGroup
	var unexpected atomic.Int32
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			from, to, amt := a.ID, b.ID, "10.00"
			if i%2 == 1 {
				from, to, amt = b.ID, a.ID, "3.00"
			}
			_, err := s.Transfer(ctx, service.TransferRequest{Key: key(t, i), SourceAccountID: from, DestinationAccountID: to, Amount: dec(amt)})
			if err != nil && !errors.Is(err, domain.ErrInsufficientFunds) { // b->a may legitimately run before b is funded
				t.Errorf("transfer %d: %v", i, err)
				unexpected.Add(1)
			}
		}(i)
	}
	wg.Wait()

	ba, bb := balance(t, s, ctx, a.ID), balance(t, s, ctx, b.ID)
	if !ba.Add(bb).Equal(dec("1000")) || ba.IsNegative() || bb.IsNegative() {
		t.Fatalf("money not conserved: a=%s b=%s", ba, bb)
	}
	requireIntegrity(t, s, ctx)
}

func TestSameKeyConcurrentlyAppliesOnce(t *testing.T) {
	s, ctx := setup(t)
	a, b := mustAccount(t, s, ctx, "USD", "100.00"), mustAccount(t, s, ctx, "USD", "0")
	k := key(t, 0)

	var wg sync.WaitGroup
	var created, replayed atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.Transfer(ctx, service.TransferRequest{Key: k, SourceAccountID: a.ID, DestinationAccountID: b.ID, Amount: dec("40.00")})
			switch {
			case err != nil:
				t.Errorf("transfer: %v", err)
			case res.Replayed:
				replayed.Add(1)
			default:
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 1 || replayed.Load() != 19 {
		t.Fatalf("created=%d replayed=%d", created.Load(), replayed.Load())
	}
	if got := balance(t, s, ctx, b.ID); !got.Equal(dec("40")) {
		t.Fatalf("destination = %s, want 40", got)
	}
}

func TestIdempotencyKeyReuseWithDifferentRequestIsRejected(t *testing.T) {
	s, ctx := setup(t)
	a, b := mustAccount(t, s, ctx, "USD", "100.00"), mustAccount(t, s, ctx, "USD", "0")
	k := key(t, 0)
	if _, err := s.Transfer(ctx, service.TransferRequest{Key: k, SourceAccountID: a.ID, DestinationAccountID: b.ID, Amount: dec("10")}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Transfer(ctx, service.TransferRequest{Key: k, SourceAccountID: a.ID, DestinationAccountID: b.ID, Amount: dec("11")})
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("got %v", err)
	}
}

func TestInsufficientFundsRejectedAndNothingChanges(t *testing.T) {
	s, ctx := setup(t)
	a, b := mustAccount(t, s, ctx, "USD", "5.00"), mustAccount(t, s, ctx, "USD", "0")
	_, err := s.Transfer(ctx, service.TransferRequest{Key: key(t, 0), SourceAccountID: a.ID, DestinationAccountID: b.ID, Amount: dec("5.01")})
	if !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("got %v", err)
	}
	if !balance(t, s, ctx, a.ID).Equal(dec("5")) || !balance(t, s, ctx, b.ID).IsZero() {
		t.Fatal("balances changed after rejected transfer")
	}
}

func TestReversal(t *testing.T) {
	s, ctx := setup(t)
	a, b := mustAccount(t, s, ctx, "USD", "100.00"), mustAccount(t, s, ctx, "USD", "0")
	res, err := s.Transfer(ctx, service.TransferRequest{Key: key(t, 0), SourceAccountID: a.ID, DestinationAccountID: b.ID, Amount: dec("25.50")})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := s.Reverse(ctx, key(t, 1), res.Transaction.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !balance(t, s, ctx, a.ID).Equal(dec("100")) || !balance(t, s, ctx, b.ID).IsZero() {
		t.Fatal("reversal did not restore balances")
	}
	if _, err := s.Reverse(ctx, key(t, 2), res.Transaction.ID); !errors.Is(err, domain.ErrAlreadyReversed) {
		t.Fatalf("second reversal: %v", err)
	}
	if _, err := s.Reverse(ctx, key(t, 3), rev.Transaction.ID); !errors.Is(err, domain.ErrCannotReverse) {
		t.Fatalf("reversing a reversal: %v", err)
	}
	requireIntegrity(t, s, ctx)
}

// Stored rate, no exchange_rate in the request: the destination amount comes from the stored USD rate.
func TestCrossCurrencyTransfer(t *testing.T) {
	s, ctx := setup(t)
	if _, err := s.SetRate(ctx, "JPY", dec("150.123456")); err != nil {
		t.Fatal(err)
	}
	usd, jpy := mustAccount(t, s, ctx, "USD", "100.00"), mustAccount(t, s, ctx, "JPY", "0")
	res, err := s.Transfer(ctx, service.TransferRequest{Key: key(t, 0), SourceAccountID: usd.ID, DestinationAccountID: jpy.ID, Amount: dec("10.01")})
	if err != nil {
		t.Fatal(err)
	}
	// 10.01 * 150.123456 = 1502.73579456 -> 1503 JPY (0 decimal places, half-even)
	if want := dec("1503"); !res.Transaction.DestinationAmount.Equal(want) || !balance(t, s, ctx, jpy.ID).Equal(want) {
		t.Fatalf("destination amount = %s, want %s", res.Transaction.DestinationAmount, want)
	}
	if !balance(t, s, ctx, usd.ID).Equal(dec("89.99")) {
		t.Fatalf("source = %s", balance(t, s, ctx, usd.ID))
	}
	requireIntegrity(t, s, ctx)
}

func TestReconcileCurrentMonthIsClean(t *testing.T) {
	s, ctx := setup(t)
	a, b := mustAccount(t, s, ctx, "USD", "50.00"), mustAccount(t, s, ctx, "USD", "0")
	if _, err := s.Transfer(ctx, service.TransferRequest{Key: key(t, 0), SourceAccountID: a.ID, DestinationAccountID: b.ID, Amount: dec("20")}); err != nil {
		t.Fatal(err)
	}
	rec, err := s.Reconcile(ctx, time.Now().UTC().Format("2006-01"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != "clean" {
		t.Fatalf("reconciliation: %+v", rec)
	}
}

// ---- exchange rates: fixed USD-based table, per-transfer override, derivation by division --------------------

func ptr(d decimal.Decimal) *decimal.Decimal { return &d }

func rateTx(t *testing.T, s *service.Service, ctx context.Context, i int, src, dst int64, amount string, rate *decimal.Decimal) (service.Result, error) {
	t.Helper()
	return s.Transfer(ctx, service.TransferRequest{Key: key(t, i), SourceAccountID: src, DestinationAccountID: dst, Amount: dec(amount), ExchangeRate: rate})
}

// A rate sent with the transfer wins over the stored one, is recorded on the transaction, and leaves the stored
// rate untouched.
func TestSuppliedRateOverridesStoredRate(t *testing.T) {
	s, ctx := setup(t)
	if _, err := s.SetRate(ctx, "EUR", dec("0.92")); err != nil {
		t.Fatal(err)
	}
	usd, eur := mustAccount(t, s, ctx, "USD", "100.00"), mustAccount(t, s, ctx, "EUR", "0")
	res, err := rateTx(t, s, ctx, 0, usd.ID, eur.ID, "10.00", ptr(dec("0.95")))
	if err != nil {
		t.Fatal(err)
	}
	if fx := res.Transaction.FXRate; fx == nil || !fx.Equal(dec("0.95")) {
		t.Fatalf("recorded rate = %v, want 0.95", fx)
	}
	if want := dec("9.5"); !res.Transaction.DestinationAmount.Equal(want) || !balance(t, s, ctx, eur.ID).Equal(want) {
		t.Fatalf("destination = %s, want %s (10.00 x 0.95)", balance(t, s, ctx, eur.ID), want)
	}
	rates, err := s.ListRates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rates {
		if r.Quote == "EUR" && !r.Rate.Equal(dec("0.92")) {
			t.Fatalf("stored EUR rate changed to %s", r.Rate)
		}
	}
	requireIntegrity(t, s, ctx)
}

// With no rate in the request, any pair is derived from the stored USD rates by one division, rounded once to 12 dp.
func TestStoredRatesAreDividedForAnyPair(t *testing.T) {
	s, ctx := setup(t)
	for cur, r := range map[string]string{"SGD": "1.30", "PKR": "280"} {
		if _, err := s.SetRate(ctx, cur, dec(r)); err != nil {
			t.Fatal(err)
		}
	}
	sgd, pkr, usd := mustAccount(t, s, ctx, "SGD", "100.00"), mustAccount(t, s, ctx, "PKR", "5000.00"), mustAccount(t, s, ctx, "USD", "0")
	for i, c := range []struct {
		from, to     int64
		amount, rate string
		got          string
	}{
		// 280 / 1.30 = 215.384615384615; 10.00 x that = 2153.84615384615 -> 2153.85 PKR
		{sgd.ID, pkr.ID, "10.00", "215.384615384615", "2153.85"},
		// 1.30 / 280 = 0.004642857143; 1000.00 x that = 4.642857143 -> 4.64 SGD
		{pkr.ID, sgd.ID, "1000.00", "0.004642857143", "4.64"},
		// 1 / 280 = 0.003571428571; 1000.00 x that = 3.571428571 -> 3.57 USD
		{pkr.ID, usd.ID, "1000.00", "0.003571428571", "3.57"},
	} {
		res, err := rateTx(t, s, ctx, i, c.from, c.to, c.amount, nil)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if fx := res.Transaction.FXRate; fx == nil || !fx.Equal(dec(c.rate)) {
			t.Fatalf("case %d: rate = %v, want %s", i, fx, c.rate)
		}
		if !res.Transaction.DestinationAmount.Equal(dec(c.got)) {
			t.Fatalf("case %d: amount = %s, want %s", i, res.Transaction.DestinationAmount, c.got)
		}
	}
	// USD -> X uses the stored rate unchanged (the USD account now holds the 3.57 received above): 3.00 x 1.30 = 3.90.
	res, err := rateTx(t, s, ctx, 9, usd.ID, sgd.ID, "3.00", nil)
	if err != nil {
		t.Fatal(err)
	}
	if fx := res.Transaction.FXRate; fx == nil || !fx.Equal(dec("1.3")) || !res.Transaction.DestinationAmount.Equal(dec("3.9")) {
		t.Fatalf("USD->SGD: rate = %v, amount = %s, want 1.3 and 3.9", fx, res.Transaction.DestinationAmount)
	}
	requireIntegrity(t, s, ctx)
}

// A currency with no stored rate cannot be converted unless the request brings its own rate.
func TestMissingRateNeedsSuppliedRate(t *testing.T) {
	s, ctx := setup(t)
	rates, err := s.ListRates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rates {
		if r.Quote == "THB" {
			t.Skip("this database already has a THB rate")
		}
	}
	thb, usd := mustAccount(t, s, ctx, "THB", "1000.00"), mustAccount(t, s, ctx, "USD", "0")
	if _, err := rateTx(t, s, ctx, 0, thb.ID, usd.ID, "100.00", nil); !errors.Is(err, domain.ErrNoRate) {
		t.Fatalf("without a rate: got %v, want ErrNoRate", err)
	}
	res, err := rateTx(t, s, ctx, 1, thb.ID, usd.ID, "100.00", ptr(dec("0.028")))
	if err != nil {
		t.Fatal(err)
	}
	if want := dec("2.8"); !res.Transaction.DestinationAmount.Equal(want) {
		t.Fatalf("amount = %s, want %s", res.Transaction.DestinationAmount, want)
	}
	requireIntegrity(t, s, ctx)
}

func TestRateInputValidation(t *testing.T) {
	s, ctx := setup(t)
	usd, eur, usd2 := mustAccount(t, s, ctx, "USD", "100.00"), mustAccount(t, s, ctx, "EUR", "0"), mustAccount(t, s, ctx, "USD", "0")
	for name, r := range map[string]decimal.Decimal{
		"zero": dec("0"), "negative": dec("-1"), "13 decimals": dec("0.9200000000001"), "absurdly large": dec("1000000000000"),
	} {
		if _, err := rateTx(t, s, ctx, 0, usd.ID, eur.ID, "1.00", ptr(r)); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("supplied rate %s: got %v, want ErrInvalid", name, err)
		}
		if _, err := s.SetRate(ctx, "EUR", r); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("SetRate %s: got %v, want ErrInvalid", name, err)
		}
	}
	if _, err := rateTx(t, s, ctx, 1, usd.ID, usd2.ID, "1.00", ptr(dec("1"))); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("rate on a same-currency transfer: got %v, want ErrInvalid", err)
	}
	if _, err := s.SetRate(ctx, "USD", dec("1")); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("SetRate USD: got %v, want ErrInvalid", err)
	}
	if _, err := s.SetRate(ctx, "XXX", dec("1")); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("SetRate unknown currency: got %v, want ErrInvalid", err)
	}
}

// The table holds at most one rate per non-USD currency, always against USD; setting again overwrites.
func TestRateTableIsFixedSize(t *testing.T) {
	s, ctx := setup(t)
	for _, r := range []string{"0.91", "0.93", "0.92"} {
		if _, err := s.SetRate(ctx, "EUR", dec(r)); err != nil {
			t.Fatal(err)
		}
	}
	rates, err := s.ListRates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eur := 0
	for _, r := range rates {
		if r.Base != "USD" || r.Quote == "USD" {
			t.Fatalf("unexpected rate row %+v", r)
		}
		if r.Quote == "EUR" {
			eur++
			if !r.Rate.Equal(dec("0.92")) {
				t.Fatalf("EUR rate = %s, want the latest (0.92)", r.Rate)
			}
		}
	}
	if eur != 1 {
		t.Fatalf("%d EUR rows, want exactly 1", eur)
	}
	if len(rates) > 29 {
		t.Fatalf("%d rows, the table is capped at one per non-USD currency (29)", len(rates))
	}
}

// The rate is part of the request: replaying it is safe, changing it under the same key is a conflict.
func TestIdempotencyIncludesExchangeRate(t *testing.T) {
	s, ctx := setup(t)
	usd, eur := mustAccount(t, s, ctx, "USD", "100.00"), mustAccount(t, s, ctx, "EUR", "0")
	k := key(t, 0)
	send := func(rate *decimal.Decimal) (service.Result, error) {
		return s.Transfer(ctx, service.TransferRequest{Key: k, SourceAccountID: usd.ID, DestinationAccountID: eur.ID, Amount: dec("10.00"), ExchangeRate: rate})
	}
	if _, err := send(ptr(dec("0.95"))); err != nil {
		t.Fatal(err)
	}
	again, err := send(ptr(dec("0.95")))
	if err != nil || !again.Replayed {
		t.Fatalf("identical retry: replayed=%v err=%v", again.Replayed, err)
	}
	if _, err := send(ptr(dec("0.96"))); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("different rate, same key: got %v, want ErrIdempotencyConflict", err)
	}
	if _, err := send(nil); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("no rate, same key: got %v, want ErrIdempotencyConflict", err)
	}
	if !balance(t, s, ctx, eur.ID).Equal(dec("9.5")) {
		t.Fatalf("destination = %s, want 9.5 (applied once)", balance(t, s, ctx, eur.ID))
	}
}
