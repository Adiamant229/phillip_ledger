package service_test

// Integration tests: need a real Postgres. Run with
//   TEST_DATABASE_URL=postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable go test -race ./...
// They only create new accounts, so they can run against a dev database.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Adiamant229/phillip_ledger/internal/domain"
	"github.com/Adiamant229/phillip_ledger/internal/repo"
	"github.com/Adiamant229/phillip_ledger/internal/service"
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

func key(t *testing.T, n any) string { return fmt.Sprintf("%s-%d-%v", t.Name(), time.Now().UnixNano(), n) }

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

func TestCrossCurrencyTransfer(t *testing.T) {
	s, ctx := setup(t)
	if _, err := s.SetRate(ctx, "USD", "JPY", dec("150.123456")); err != nil {
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
