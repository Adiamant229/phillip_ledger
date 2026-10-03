// Package handler is the HTTP edge: decode, call the service, map errors to status codes. No business rules.
package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/Adiamant229/phillip_ledger/internal/domain"
	"github.com/Adiamant229/phillip_ledger/internal/service"
	"github.com/shopspring/decimal"
)

type Handler struct{ svc *service.Service }

func New(svc *service.Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })

	mux.HandleFunc("POST /accounts", h.createAccount)
	mux.HandleFunc("GET /accounts", h.listAccounts)
	mux.HandleFunc("GET /accounts/{id}", h.getAccount)
	mux.HandleFunc("GET /accounts/{id}/transactions", h.accountTransactions)
	mux.HandleFunc("POST /accounts/{id}/deposit", h.deposit)

	mux.HandleFunc("POST /transactions", h.transfer)
	mux.HandleFunc("GET /transactions/{id}", h.getTransaction)
	mux.HandleFunc("POST /transactions/{id}/reverse", h.reverse)

	mux.HandleFunc("GET /exchange-rates", h.listRates)
	mux.HandleFunc("POST /exchange-rates", h.setRate)

	mux.HandleFunc("GET /audit/integrity", h.integrity)
	mux.HandleFunc("POST /reconciliations", h.reconcile)
	mux.HandleFunc("GET /reconciliations", h.listReconciliations)
	mux.HandleFunc("GET /reconciliations/{id}", h.getReconciliation)
	return logRequests(mux)
}

// ---- accounts -------------------------------------------------------------------------------------------

func (h *Handler) createAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name           string          `json:"name"`
		Currency       string          `json:"currency"`
		InitialBalance decimal.Decimal `json:"initial_balance"`
	}
	if !decode(w, r, &in) {
		return
	}
	a, err := h.svc.CreateAccount(r.Context(), in.Name, in.Currency, in.InitialBalance)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (h *Handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListAccounts(r.Context(), r.URL.Query().Get("include_system") == "true")
	respond(w, list, err)
}

func (h *Handler) getAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a, err := h.svc.GetAccount(r.Context(), id)
	respond(w, a, err)
}

func (h *Handler) accountTransactions(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	cursor, _ := strconv.ParseInt(q.Get("cursor"), 10, 64)
	entries, err := h.svc.History(r.Context(), id, limit, cursor, q.Get("order") == "desc")
	respond(w, entries, err)
}

func (h *Handler) deposit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Amount decimal.Decimal `json:"amount"`
	}
	if !decode(w, r, &in) {
		return
	}
	res, err := h.svc.Deposit(r.Context(), r.Header.Get("Idempotency-Key"), id, in.Amount)
	writeResult(w, res, err)
}

// ---- transactions ---------------------------------------------------------------------------------------

func (h *Handler) transfer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SourceAccountID      int64           `json:"source_account_id"`
		DestinationAccountID int64           `json:"destination_account_id"`
		Amount               decimal.Decimal `json:"amount"`
	}
	if !decode(w, r, &in) {
		return
	}
	res, err := h.svc.Transfer(r.Context(), service.TransferRequest{
		Key: r.Header.Get("Idempotency-Key"), SourceAccountID: in.SourceAccountID,
		DestinationAccountID: in.DestinationAccountID, Amount: in.Amount,
	})
	writeResult(w, res, err)
}

func (h *Handler) reverse(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.Reverse(r.Context(), r.Header.Get("Idempotency-Key"), r.PathValue("id"))
	writeResult(w, res, err)
}

func (h *Handler) getTransaction(w http.ResponseWriter, r *http.Request) {
	t, err := h.svc.GetTransaction(r.Context(), r.PathValue("id"))
	respond(w, t, err)
}

// writeResult: 201 for a newly recorded transaction, 200 when an identical earlier request is replayed.
func writeResult(w http.ResponseWriter, res service.Result, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"transaction_id":    res.Transaction.ID,
		"status":            "confirmed",
		"idempotent_replay": res.Replayed,
		"transaction":       res.Transaction,
	})
}

// ---- rates / audit --------------------------------------------------------------------------------------

func (h *Handler) listRates(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListRates(r.Context())
	respond(w, list, err)
}

func (h *Handler) setRate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Base  string          `json:"base"`
		Quote string          `json:"quote"`
		Rate  decimal.Decimal `json:"rate"`
	}
	if !decode(w, r, &in) {
		return
	}
	rate, err := h.svc.SetRate(r.Context(), in.Base, in.Quote, in.Rate)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rate)
}

func (h *Handler) integrity(w http.ResponseWriter, r *http.Request) {
	rep, err := h.svc.CheckIntegrity(r.Context())
	respond(w, rep, err)
}

func (h *Handler) reconcile(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Period string `json:"period"`
	}
	if !decode(w, r, &in) {
		return
	}
	rec, err := h.svc.Reconcile(r.Context(), in.Period)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (h *Handler) listReconciliations(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListReconciliations(r.Context())
	respond(w, list, err)
}

func (h *Handler) getReconciliation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rec, err := h.svc.GetReconciliation(r.Context(), id)
	respond(w, rec, err)
}

// ---- helpers --------------------------------------------------------------------------------------------

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request", "malformed JSON body: "+err.Error()))
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request", "id must be a positive integer"))
		return 0, false
	}
	return id, true
}

func respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func errBody(code, msg string) map[string]any {
	return map[string]any{"error": map[string]string{"code": code, "message": msg}}
}

func writeErr(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal_error"
	switch {
	case errors.Is(err, domain.ErrInvalid):
		status, code = http.StatusBadRequest, "invalid_request"
	case errors.Is(err, domain.ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, domain.ErrNoRate):
		status, code = http.StatusUnprocessableEntity, "no_exchange_rate"
	case errors.Is(err, domain.ErrInsufficientFunds):
		status, code = http.StatusUnprocessableEntity, "insufficient_funds"
	case errors.Is(err, domain.ErrIdempotencyConflict):
		status, code = http.StatusConflict, "idempotency_key_reused"
	case errors.Is(err, domain.ErrAlreadyReversed):
		status, code = http.StatusConflict, "already_reversed"
	case errors.Is(err, domain.ErrCannotReverse):
		status, code = http.StatusConflict, "cannot_reverse"
	}
	msg := err.Error()
	if status == http.StatusInternalServerError {
		log.Printf("internal error: %v", err) // details stay in the log, not the response
		msg = "internal error"
	}
	writeJSON(w, status, errBody(code, msg))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
