# Ledger

A multi-currency ledger with double-entry accounting.

## Tech Stack
Frontend: React + Vite
Backend: Golang
Database: Postgres

## Setup

```bash
docker compose up -d                      # Postgres 16 on :5432
cd backend && go mod tidy && go run ./cmd/server
cd frontend && npm install && npm run dev          # UI on :5173
```

Tests (need a database; they only create new accounts):

```bash
cd backend && TEST_DATABASE_URL='postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable' go test -race ./...
```

## .env
```
VITE_CURRENCYFREAKS_KEY=insert_api_key
VITE_FX_REFRESH_MINUTES=time_in_minutes
```

`DATABASE_URL`, `PORT` (default 8080). Requires Go 1.22+ (uses the stdlib router's method/path patterns).

## API

| Method & path | Purpose |
|---|---|
| `POST /accounts` `{name, currency, initial_balance?}` | Open an account. A non-zero opening balance is a journaled deposit. |
| `GET /accounts`, `GET /accounts/{id}` | List / current balance |
| `GET /accounts/{id}/transactions?limit&cursor&order=asc\|desc` | Journal with running balance, ordered by time (default oldest first) |
| `POST /accounts/{id}/deposit` `{amount}` | Add funds from the currency's funding account |
| `POST /transactions` `{source_account_id, destination_account_id, amount, exchange_rate?}` | Transfer (`exchange_rate` = 1 source in destination currency, cross-currency only). `201` + `transaction_id`, or `200` with `idempotent_replay: true` |
| `POST /transactions/{id}/reverse` | Post a reversal |
| `GET /transactions/{id}` | One transaction |
| `GET /exchange-rates`, `POST /exchange-rates` `{quote, rate}` | Stored fallback rates, always against USD (1 USD = rate quote). Upsert: `200`, one row per currency |
| `GET /audit/integrity` | Ledger-wide invariant check |
| `POST /reconciliations` `{period: "2026-09"}`, `GET /reconciliations[/{id}]` | Month-end reconciliation, persisted |

Errors: `400` invalid input, `404` unknown id, `409` key reused i.e. duplicate request, `422` insufficient funds / no exchange rate. Body: `{"error": {"code", "message"}}`.

# Considerations

## Precision and Correctness

Atomic Transactions: handled by Postgres transactions as all SQL commands are bundled together and only committed in an all-or-nothing operation. Transactions are identified with a key to prevent duplicates. Schema prevents transactions with negative balance if business logic fails.
Mathematical precision: monetary values are calculated and stored in decimal, and in http requests are stored as strings.

## Concurrency

Multiple requests can be sent to the backend and handled through multithreading as each API call creates a goroutine thread. Database access also benefits from concurrency through the Postgres connection pool.
Requests to the same account lock the row to prevent write-write conflicts, and transactions to the same accounts are queued. Global lock order to prevent deadlock
Autocommit mode prevents dirty reads.

## Assumptions

1. Single account so authorisation is not implemented
2. Accounts are single-currency and cannot go negative and system accounts are not included.
3. Money can be arbitrarily deposited into the accounts.
4. Only 30 currencies are included at this point.
5. Assume that cross-currency conversion using USD as base is lossless.
6. Single-currency accounts

## Design choices beyond the core (and tradeoffs)

- Balance as the source of truth and transaction history for audit purposes instead of transactions or balance only results in tradeoff of more memory for paper trail.
- Convert currency to target currency instead of at the end, trading accuracy in calculation due to slight rounding errors for amortization of calculation as well as reflecting balance.
- Service Repo architecture to separate business logic from frontend and database for testing.
- React + Vite for integrated toolchain and faster development
- Golang for concurrency, simple development and strong typing.
- Postgres supports transactions and concurrency out of the box
  Postgres because the locking and constraints are the behaviour worth testing; mocks would prove nothing there.
- **Embedded SQL migrator** keeps the demo to one binary. Use goose/atlas/migrate for real deployments.
- **Not built:** auth, pagination beyond a simple cursor, partial reversals, scheduled reconciliation, an
  outbox/event stream, multi-hop FX.