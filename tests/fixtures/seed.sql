-- Synthetic dataset used by Unth Recover integration tests and the quickstart.
-- It contains NO real data. It exercises the object types that the
-- verification engine compares: tables, constraints (PK/FK/UNIQUE/CHECK/
-- EXCLUDE), indexes, sequences, enum and domain types, views, materialized
-- views, functions, triggers and a non-public schema.

CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TYPE order_status AS ENUM ('new', 'paid', 'shipped', 'cancelled');
CREATE DOMAIN email_address AS text CHECK (VALUE ~ '^[^@\s]+@[^@\s]+$');

CREATE TABLE customers (
    id          bigserial PRIMARY KEY,
    email       email_address NOT NULL UNIQUE,
    full_name   text NOT NULL,
    active      boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT '2026-01-01 00:00:00+00'
);

CREATE TABLE products (
    id          serial PRIMARY KEY,
    sku         text NOT NULL UNIQUE,
    name        text NOT NULL,
    price_cents integer NOT NULL CHECK (price_cents >= 0),
    attributes  jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE TABLE orders (
    id           bigserial PRIMARY KEY,
    customer_id  bigint NOT NULL REFERENCES customers(id) ON DELETE RESTRICT,
    status       order_status NOT NULL DEFAULT 'new',
    total_cents  bigint NOT NULL CHECK (total_cents >= 0),
    placed_at    timestamptz NOT NULL,
    notes        text
);
CREATE INDEX orders_customer_idx ON orders (customer_id, placed_at DESC);

CREATE TABLE order_items (
    order_id    bigint NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    product_id  integer NOT NULL REFERENCES products(id),
    quantity    integer NOT NULL CHECK (quantity > 0),
    unit_cents  integer NOT NULL,
    PRIMARY KEY (order_id, product_id)
);

CREATE SCHEMA billing;
CREATE TABLE billing.invoices (
    id          uuid PRIMARY KEY,
    order_id    bigint NOT NULL UNIQUE REFERENCES public.orders(id),
    amount      numeric(12,2) NOT NULL,
    issued_on   date NOT NULL,
    payload     bytea
);

CREATE TABLE room_bookings (
    id      serial PRIMARY KEY,
    room    integer NOT NULL,
    during  tstzrange NOT NULL,
    EXCLUDE USING gist (room WITH =, during WITH &&)
);

CREATE TABLE audit_log (
    id         bigserial PRIMARY KEY,
    table_name text NOT NULL,
    row_id     bigint NOT NULL,
    action     text NOT NULL
);

CREATE FUNCTION log_order_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO audit_log(table_name, row_id, action) VALUES (TG_TABLE_NAME, NEW.id, TG_OP);
    RETURN NEW;
END $$;
CREATE TRIGGER orders_audit AFTER INSERT OR UPDATE ON orders
    FOR EACH ROW EXECUTE FUNCTION log_order_change();

CREATE FUNCTION customer_lifetime_value(cid bigint) RETURNS bigint LANGUAGE sql STABLE AS $$
    SELECT COALESCE(sum(total_cents), 0)::bigint FROM orders WHERE customer_id = cid AND status <> 'cancelled'
$$;

-- Deterministic synthetic data (no random()).
INSERT INTO customers (email, full_name, active, created_at)
SELECT 'user' || g || '@example.test', 'Customer ' || g, g % 7 <> 0,
       '2026-01-01 00:00:00+00'::timestamptz + (g || ' hours')::interval
FROM generate_series(1, 2000) g;

INSERT INTO products (sku, name, price_cents, attributes)
SELECT 'SKU-' || lpad(g::text, 5, '0'), 'Product ' || g, (g * 137) % 50000,
       jsonb_build_object('color', (ARRAY['red','green','blue'])[1 + g % 3], 'weight', g * 0.25)
FROM generate_series(1, 300) g;

INSERT INTO orders (customer_id, status, total_cents, placed_at, notes)
SELECT 1 + (g * 7919) % 2000,
       (ARRAY['new','paid','shipped','cancelled']::order_status[])[1 + g % 4],
       (g * 9973) % 1000000,
       '2026-02-01 00:00:00+00'::timestamptz + (g || ' minutes')::interval,
       CASE WHEN g % 5 = 0 THEN 'note ' || g || E' with ''quotes'' and \\ backslash' END
FROM generate_series(1, 8000) g;

INSERT INTO order_items (order_id, product_id, quantity, unit_cents)
SELECT o, 1 + (o * 31 + k) % 300, 1 + (o + k) % 5, (o * k) % 10000
FROM generate_series(1, 8000) o, generate_series(1, 3) k;

INSERT INTO billing.invoices (id, order_id, amount, issued_on, payload)
SELECT md5('invoice' || g)::uuid, g, (g * 1.37)::numeric(12,2), DATE '2026-03-01' + (g % 200),
       decode(md5('payload' || g), 'hex')
FROM generate_series(1, 4000) g;

INSERT INTO room_bookings (room, during)
SELECT 1 + g % 20, tstzrange('2026-04-01'::timestamptz + (g * 2 || ' hours')::interval,
                             '2026-04-01'::timestamptz + (g * 2 + 1 || ' hours')::interval)
FROM generate_series(1, 500) g;

CREATE VIEW active_customers AS SELECT id, email FROM customers WHERE active;
CREATE MATERIALIZED VIEW revenue_by_status AS
    SELECT status, count(*) AS orders, sum(total_cents) AS revenue FROM orders GROUP BY status;

-- Advance a sequence without using it so last_value differs from row count.
SELECT setval('products_id_seq', 1000);

ANALYZE;
