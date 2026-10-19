-- ETL acceptance warehouse schema (MySQL 8.4, database `warehouse`). Test fixture; mirrors
-- ../../pg/init/020_schema.sql. Timestamps are UTC (the server runs with default_time_zone '+00:00').

CREATE TABLE bronze_files (
  listing_sha256 CHAR(64) PRIMARY KEY,
  batch_count    INT NOT NULL CHECK (batch_count BETWEEN 1 AND 64),
  run_id         VARCHAR(64) NOT NULL,
  landed_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
);

CREATE TABLE staging_orders (
  run_id         VARCHAR(64) NOT NULL,
  batch_no       INT NOT NULL,
  order_id       VARCHAR(16) NOT NULL,
  order_seq      INT NOT NULL,
  customer_id    VARCHAR(16) NOT NULL,
  country        CHAR(2) NOT NULL,
  currency       CHAR(3) NOT NULL,
  amount         DECIMAL(14,2) NOT NULL,
  discount_pct   DECIMAL(5,1) NOT NULL,
  net_amount_usd DECIMAL(14,2) NOT NULL,
  ordered_at     DATETIME NOT NULL,
  order_month    CHAR(7) NOT NULL,
  ship_date      DATE NULL,
  channel        VARCHAR(16) NOT NULL,
  sku            VARCHAR(32) NOT NULL,
  qty            INT NOT NULL CHECK (qty > 0),
  PRIMARY KEY (run_id, batch_no, order_seq)
);

CREATE TABLE staging_batches (
  run_id    VARCHAR(64) NOT NULL,
  batch_no  INT NOT NULL,
  `rows`    INT NOT NULL,
  staged_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (run_id, batch_no)
);

CREATE TABLE fact_orders (
  run_id         VARCHAR(64) NOT NULL,
  order_id       VARCHAR(16) NOT NULL,
  order_seq      INT NOT NULL,
  customer_id    VARCHAR(16) NOT NULL,
  country        CHAR(2) NOT NULL,
  currency       CHAR(3) NOT NULL,
  amount         DECIMAL(14,2) NOT NULL,
  discount_pct   DECIMAL(5,1) NOT NULL,
  net_amount_usd DECIMAL(14,2) NOT NULL,
  ordered_at     DATETIME NOT NULL,
  order_month    CHAR(7) NOT NULL,
  ship_date      DATE NULL,
  channel        VARCHAR(16) NOT NULL,
  sku            VARCHAR(32) NOT NULL,
  qty            INT NOT NULL,
  PRIMARY KEY (run_id, order_id)
);

CREATE TABLE dim_customer (
  customer_id    VARCHAR(16) PRIMARY KEY,
  name           VARCHAR(64) NOT NULL,
  country        CHAR(2) NOT NULL,
  segment        VARCHAR(16) NOT NULL,
  created_at     DATE NOT NULL,
  updated_run_id VARCHAR(64) NOT NULL
);

CREATE TABLE etl_runs (
  run_id     VARCHAR(64) PRIMARY KEY,
  status     VARCHAR(16) NOT NULL,
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)
);

CREATE TABLE gold_sales_daily (
  run_id VARCHAR(64) NOT NULL, order_date DATE NOT NULL,
  orders BIGINT NOT NULL, qty BIGINT NOT NULL, net_amount_usd DECIMAL(16,2) NOT NULL,
  PRIMARY KEY (run_id, order_date)
);
CREATE TABLE gold_sales_by_country (
  run_id VARCHAR(64) NOT NULL, country CHAR(2) NOT NULL,
  orders BIGINT NOT NULL, qty BIGINT NOT NULL, net_amount_usd DECIMAL(16,2) NOT NULL,
  PRIMARY KEY (run_id, country)
);
CREATE TABLE gold_sales_by_channel (
  run_id VARCHAR(64) NOT NULL, channel VARCHAR(16) NOT NULL,
  orders BIGINT NOT NULL, qty BIGINT NOT NULL, net_amount_usd DECIMAL(16,2) NOT NULL,
  PRIMARY KEY (run_id, channel)
);

-- The DB node's effect-marker table (T-5E §3.3).
CREATE TABLE elitea_effects (
  effect_id    VARCHAR(128) PRIMARY KEY,
  committed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
);
