package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/dylan/ledger/internal/domain"
	"github.com/dylan/ledger/internal/repo"
	"github.com/shopspring/decimal"
)

// CheckIntegrity verifies the ledger's invariants against one consistent snapshot. The headline one: because
// every transaction nets to zero in each currency, the sum of ALL balances (user + system accounts) in each
// currency must be exactly zero. Anything else is reported, never auto-corrected.
func (s *Service) CheckIntegrity(ctx context.Context) (domain.IntegrityReport, error) {
	rep := domain.IntegrityReport{CheckedAt: s.now().UTC()}
	err := s.store.WithSnapshot(ctx, func(r repo.Reader) error {
		var err error
		if rep.CurrencyTotals, err = currencyTotals(ctx, r); err != nil {
			return err
		}
		rep.Issues, err = r.IntegrityIssues(ctx)
		return err
	})
	if err != nil {
		return rep, err
	}
	rep.OK = len(rep.Issues) == 0
	for _, t := range rep.CurrencyTotals {
		rep.OK = rep.OK && t.OK
	}
	return rep, nil
}

func currencyTotals(ctx context.Context, r repo.Reader) ([]domain.CurrencyTotal, error) {
	bal, err := r.BalanceTotals(ctx)
	if err != nil {
		return nil, err
	}
	ent, err := r.EntryTotals(ctx)
	if err != nil {
		return nil, err
	}
	out := []domain.CurrencyTotal{}
	for cur, b := range bal {
		e := ent[cur] // zero if the currency has no entries yet
		out = append(out, domain.CurrencyTotal{
			Currency: cur, BalanceSum: b, EntriesSum: e, Expected: decimal.Zero,
			OK: b.IsZero() && e.IsZero(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out, nil
}

// Reconcile checks one UTC calendar month ("2026-09"): for every account it rebuilds opening/closing balances
// from the journal and compares them with (a) the balance_after recorded on the period's last entry and
// (b) today's stored balance when nothing was posted after the period. It also confirms each currency still
// nets to zero at period end and re-runs the integrity checks on the same snapshot. The run is persisted.
func (s *Service) Reconcile(ctx context.Context, period string) (domain.Reconciliation, error) {
	start, err := time.Parse("2006-01", period)
	if err != nil {
		return domain.Reconciliation{}, invalid("period must look like 2026-09")
	}
	if start.After(s.now()) {
		return domain.Reconciliation{}, invalid("period %s has not started yet", period)
	}
	rec := domain.Reconciliation{PeriodStart: start, PeriodEnd: start.AddDate(0, 1, 0), Lines: []domain.ReconLine{}, Issues: []domain.Issue{}}

	err = s.store.WithSnapshot(ctx, func(r repo.Reader) error {
		var err error
		if rec.Lines, err = r.ReconcileRows(ctx, rec.PeriodStart, rec.PeriodEnd); err != nil {
			return err
		}
		rec.Issues, err = r.IntegrityIssues(ctx)
		return err
	})
	if err != nil {
		return rec, err
	}

	count := len(rec.Issues)
	closingByCurrency := map[string]decimal.Decimal{}
	for i := range rec.Lines {
		l := &rec.Lines[i]
		closingByCurrency[l.Currency] = closingByCurrency[l.Currency].Add(l.Closing)
		if !l.Closing.Equal(l.RecordedClosing) {
			l.Discrepancies = append(l.Discrepancies,
				fmt.Sprintf("closing balance from entries is %s but the last recorded balance is %s", l.Closing, l.RecordedClosing))
		}
		if l.CheckedAgainstCurrent && !l.Closing.Equal(l.CurrentBalance) {
			l.Discrepancies = append(l.Discrepancies,
				fmt.Sprintf("closing balance from entries is %s but the stored balance is %s", l.Closing, l.CurrentBalance))
		}
		count += len(l.Discrepancies)
	}
	for cur, sum := range closingByCurrency {
		if !sum.IsZero() {
			rec.Issues = append(rec.Issues, domain.Issue{Check: "period_end_total",
				Message: fmt.Sprintf("%s: all closing balances sum to %s (must be 0)", cur, sum)})
			count++
		}
	}

	rec.AccountsChecked, rec.DiscrepancyCount, rec.Status = len(rec.Lines), count, "clean"
	if count > 0 {
		rec.Status = "discrepancies"
	}
	return rec, s.store.SaveReconciliation(ctx, &rec)
}

func (s *Service) ListReconciliations(ctx context.Context) ([]domain.Reconciliation, error) {
	return s.store.ListReconciliations(ctx)
}

func (s *Service) GetReconciliation(ctx context.Context, id int64) (domain.Reconciliation, error) {
	return s.store.GetReconciliation(ctx, id)
}
