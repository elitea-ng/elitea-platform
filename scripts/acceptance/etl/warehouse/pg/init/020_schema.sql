-- ETL acceptance warehouse schema (PostgreSQL). Test fixture; mirrored by ../../mysql/init/020_schema.sql.
-- Keys make every load idempotent per run: staging by (run_id, batch_no, order_seq), fact and gold by run_id.
CREATE SCHEMA etl;

CREATE TABLE etl.bronze_files (
  listing_sha256 char(64) PRIMARY KEY,
  batch_count    integer NOT NULL CHECK (batch_count BETWEEN 1 AND 64),
  run_id         varchar(64) NOT NULL,
  landed_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE etl.staging_orders (
  run_id         varchar(64) NOT NULL,
  batch_no       integer NOT NULL,
  order_id       varchar(16) NOT NULL,
  order_seq      integer NOT NULL,
  customer_id    varchar(16) NOT NULL,
  country        char(2) NOT NULL,
  currency       char(3) NOT NULL,
  amount         numeric(14,2) NOT NULL,
  discount_pct   numeric(5,1) NOT NULL,
  net_amount_usd numeric(14,2) NOT NULL,
  ordered_at     timestamptz NOT NULL,
  order_month    char(7) NOT NULL,
  ship_date      date,
  channel        varchar(16) NOT NULL,
  sku            varchar(32) NOT NULL,
  qty            integer NOT NULL CHECK (qty > 0),
  PRIMARY KEY (run_id, batch_no, order_seq)
);

CREATE TABLE etl.staging_batches (
  run_id    varchar(64) NOT NULL,
  batch_no  integer NOT NULL,
  rows      integer NOT NULL,
  staged_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, batch_no)
);

CREATE TABLE etl.fact_orders (
  run_id         varchar(64) NOT NULL,
  order_id       varchar(16) NOT NULL,
  order_seq      integer NOT NULL,
  customer_id    varchar(16) NOT NULL,
  country        char(2) NOT NULL,
  currency       char(3) NOT NULL,
  amount         numeric(14,2) NOT NULL,
  discount_pct   numeric(5,1) NOT NULL,
  net_amount_usd numeric(14,2) NOT NULL,
  ordered_at     timestamptz NOT NULL,
  order_month    char(7) NOT NULL,
  ship_date      date,
  channel        varchar(16) NOT NULL,
  sku            varchar(32) NOT NULL,
  qty            integer NOT NULL,
  PRIMARY KEY (run_id, order_id)
);

CREATE TABLE etl.dim_customer (
  customer_id    varchar(16) PRIMARY KEY,
  name           varchar(64) NOT NULL,
  country        char(2) NOT NULL,
  segment        varchar(16) NOT NULL,
  created_at     date NOT NULL,
  updated_run_id varchar(64) NOT NULL
);

CREATE TABLE etl.etl_runs (
  run_id     varchar(64) PRIMARY KEY,
  status     varchar(16) NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE etl.gold_sales_daily (
  run_id varchar(64) NOT NULL, order_date date NOT NULL,
  orders bigint NOT NULL, qty bigint NOT NULL, net_amount_usd numeric(16,2) NOT NULL,
  PRIMARY KEY (run_id, order_date)
);
CREATE TABLE etl.gold_sales_by_country (
  run_id varchar(64) NOT NULL, country char(2) NOT NULL,
  orders bigint NOT NULL, qty bigint NOT NULL, net_amount_usd numeric(16,2) NOT NULL,
  PRIMARY KEY (run_id, country)
);
CREATE TABLE etl.gold_sales_by_channel (
  run_id varchar(64) NOT NULL, channel varchar(16) NOT NULL,
  orders bigint NOT NULL, qty bigint NOT NULL, net_amount_usd numeric(16,2) NOT NULL,
  PRIMARY KEY (run_id, channel)
);

-- The DB node's effect-marker table (T-5E §3.3): written in the same transaction as each write node's statements.
CREATE TABLE etl.elitea_effects (
  effect_id    varchar(128) PRIMARY KEY,
  committed_at timestamptz NOT NULL DEFAULT now()
);

GRANT USAGE ON SCHEMA etl TO etl_loader, etl_reader;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA etl TO etl_loader;
GRANT SELECT ON ALL TABLES IN SCHEMA etl TO etl_reader;
