-- Task 6.6: what the reconciliation job remembers about closed months (LedgerMonths), so each
-- run scans only the open months of ledger_entries instead of the whole ledger. Derived data:
-- it holds sums of ledger entries, never money of its own, and the job rebuilds nothing from it
-- without checking. A month closes a day after it ends, when no posting can still be writing
-- into it (a posting's entries take its transaction's start time).

-- One row per closed month: when it closed and when its totals were last checked against its
-- partition (one month per run, the one checked longest ago).
CREATE TABLE ledger_months_closed (
    month       DATE PRIMARY KEY CHECK (month = date_trunc('month', month)),
    entries     BIGINT NOT NULL,
    closed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    verified_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Each account's sum over a closed month. NUMERIC, as the job's own sums are.
CREATE TABLE ledger_month_totals (
    month      DATE NOT NULL REFERENCES ledger_months_closed (month),
    account_id BIGINT NOT NULL REFERENCES accounts (account_id),
    total      NUMERIC NOT NULL,
    PRIMARY KEY (month, account_id)
);
-- Every closed month's totals per account, summed by the job's account check.
CREATE INDEX ledger_month_totals_account_idx ON ledger_month_totals (account_id) INCLUDE (total);
