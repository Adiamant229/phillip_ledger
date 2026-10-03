-- Ledger schema. Applied once by the embedded migrator (see repo/db.go).

CREATE TABLE currencies (
    code     TEXT PRIMARY KEY CHECK (code ~ '^[A-Z]{3}$'),
    exponent SMALLINT NOT NULL CHECK (exponent BETWEEN 0 AND 8)   -- minor-unit digits (USD 2, JPY 0)
);
INSERT INTO currencies VALUES ('USD', 2), ('EUR', 2), ('GBP', 2), ('SGD', 2), ('JPY', 0);

CREATE TABLE accounts (
    id             BIGSERIAL PRIMARY KEY,
    name           TEXT NOT NULL,
    currency       TEXT NOT NULL REFERENCES currencies (code),
    balance        NUMERIC(28, 8) NOT NULL DEFAULT 0,             -- cached; always == SUM(ledger_entries.amount)
    is_system      BOOLEAN NOT NULL DEFAULT FALSE,
    system_role    TEXT CHECK (system_role IN ('funding', 'fx')),
    allow_negative BOOLEAN NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CHECK (allow_negative OR balance >= 0),                       -- last line of defence against overdrafts
    CHECK (is_system = (system_role IS NOT NULL))
);
CREATE UNIQUE INDEX accounts_one_system_role ON accounts (currency, system_role) WHERE is_system;

-- One funding account (money enters/leaves the ledger) and one FX clearing account per currency.
INSERT INTO accounts (name, currency, is_system, system_role, allow_negative)
SELECT 'SYSTEM funding ' || code, code, TRUE, 'funding', TRUE FROM currencies;
INSERT INTO accounts (name, currency, is_system, system_role, allow_negative)
SELECT 'SYSTEM fx clearing ' || code, code, TRUE, 'fx', TRUE FROM currencies;

CREATE TABLE exchange_rates (
    id           BIGSERIAL PRIMARY KEY,
    base         TEXT NOT NULL REFERENCES currencies (code),
    quote        TEXT NOT NULL REFERENCES currencies (code),
    rate         NUMERIC(28, 12) NOT NULL CHECK (rate > 0),       -- 1 base = rate quote
    effective_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CHECK (base <> quote)
);
CREATE INDEX exchange_rates_lookup ON exchange_rates (base, quote, effective_at DESC, id DESC);
-- Sample rates so the demo works out of the box. Not real market data.
INSERT INTO exchange_rates (base, quote, rate) VALUES
    ('USD', 'EUR', 0.92), ('USD', 'SGD', 1.30), ('USD', 'JPY', 150), ('GBP', 'USD', 1.25);

CREATE TABLE transactions (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key         TEXT NOT NULL UNIQUE,
    request_hash            TEXT NOT NULL,
    kind                    TEXT NOT NULL CHECK (kind IN ('transfer', 'deposit', 'reversal')),
    source_account_id       BIGINT NOT NULL REFERENCES accounts (id),
    destination_account_id  BIGINT NOT NULL REFERENCES accounts (id),
    source_amount           NUMERIC(28, 8) NOT NULL CHECK (source_amount > 0),
    source_currency         TEXT NOT NULL REFERENCES currencies (code),
    destination_amount      NUMERIC(28, 8) NOT NULL CHECK (destination_amount > 0),
    destination_currency    TEXT NOT NULL REFERENCES currencies (code),
    fx_rate                 NUMERIC(28, 12),
    reverses_transaction_id UUID UNIQUE REFERENCES transactions (id),   -- UNIQUE: a transaction is reversed at most once
    created_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CHECK (source_account_id <> destination_account_id),
    CHECK ((kind = 'reversal') = (reverses_transaction_id IS NOT NULL))
);

CREATE TABLE ledger_entries (
    id             BIGSERIAL PRIMARY KEY,
    transaction_id UUID NOT NULL REFERENCES transactions (id),
    account_id     BIGINT NOT NULL REFERENCES accounts (id),
    currency       TEXT NOT NULL REFERENCES currencies (code),
    amount         NUMERIC(28, 8) NOT NULL CHECK (amount <> 0),        -- signed: + credit, - debit
    balance_after  NUMERIC(28, 8) NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX ledger_entries_account ON ledger_entries (account_id, id);
CREATE INDEX ledger_entries_tx ON ledger_entries (transaction_id);

-- The ledger is append-only: corrections are new (reversal) transactions.
CREATE FUNCTION forbid_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% on % is not allowed: the ledger is append-only', TG_OP, TG_TABLE_NAME;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER entries_append_only BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER transactions_append_only BEFORE UPDATE OR DELETE ON transactions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Double-entry guard, checked at COMMIT: every transaction's entries must net to zero in every currency.
CREATE FUNCTION check_transaction_balanced() RETURNS trigger AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM ledger_entries WHERE transaction_id = NEW.transaction_id
               GROUP BY currency HAVING SUM(amount) <> 0) THEN
        RAISE EXCEPTION 'transaction % is unbalanced', NEW.transaction_id;
    END IF;
    RETURN NULL;
END $$ LANGUAGE plpgsql;
CREATE CONSTRAINT TRIGGER entries_balanced AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_transaction_balanced();

CREATE TABLE reconciliation_runs (
    id                BIGSERIAL PRIMARY KEY,
    period_start      TIMESTAMPTZ NOT NULL,
    period_end        TIMESTAMPTZ NOT NULL,
    status            TEXT NOT NULL CHECK (status IN ('clean', 'discrepancies')),
    accounts_checked  INT NOT NULL,
    discrepancy_count INT NOT NULL,
    report            JSONB NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
