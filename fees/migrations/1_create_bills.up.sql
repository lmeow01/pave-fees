CREATE TABLE bills (
    id              TEXT PRIMARY KEY,
    customer_id     TEXT        NOT NULL,
    currency        TEXT        NOT NULL CHECK (currency IN ('GEL', 'USD')),
    status          TEXT        NOT NULL CHECK (status IN ('open', 'closed')),
    period_end      TIMESTAMPTZ NOT NULL,
    total_minor     BIGINT      CHECK (total_minor >= 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at       TIMESTAMPTZ,
    idempotency_key TEXT        NOT NULL UNIQUE,

    -- A bill has a total and a closing time exactly when it is closed.
    CHECK ((status = 'closed') = (total_minor IS NOT NULL)),
    CHECK ((status = 'closed') = (closed_at IS NOT NULL)),

    -- Target of the line_items foreign key that pins every item to its
    -- bill's currency.
    UNIQUE (id, currency)
);

-- A customer's bills are listed and paged by ID, which is time-ordered.
CREATE INDEX bills_by_customer ON bills (customer_id, id);

CREATE TABLE line_items (
    id              TEXT PRIMARY KEY,
    bill_id         TEXT        NOT NULL,
    description     TEXT        NOT NULL,
    amount_minor    BIGINT      NOT NULL CHECK (amount_minor > 0),
    currency        TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    idempotency_key TEXT        NOT NULL,

    FOREIGN KEY (bill_id, currency) REFERENCES bills (id, currency),
    UNIQUE (bill_id, idempotency_key)
);

-- A bill's line items are listed and paged by ID, which is time-ordered.
CREATE INDEX line_items_by_bill ON line_items (bill_id, id);
