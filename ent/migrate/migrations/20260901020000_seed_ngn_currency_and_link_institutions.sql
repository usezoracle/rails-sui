-- Seed the NGN fiat currency and link the NGN institutions to it
-- (data migration; idempotent, non-destructive).
--
-- 20240613143010_ngn_institutions.sql inserts the 32 NGN banks with
-- "fiat_currency_institutions" read from fiat_currencies WHERE "code" = 'NGN',
-- but no migration ever inserts that currency row — it was created by hand on
-- the original database. On a database built purely from migrations the lookup
-- yields NULL, every NGN bank lands unlinked, and GET /institutions/:currency
-- (which filters on the fiat-currency edge) returns [] for NGN — "No banks
-- found" in the payout bank picker. NGN must also be is_enabled = true: the
-- Tap-card flows query fiatcurrency.CodeEQ("NGN") + IsEnabledEQ(true).
--
-- market_rate is a placeholder only. ComputeMarketRate overwrites it on the
-- next cron tick from live sources, for enabled currencies.

INSERT INTO "fiat_currencies" (
    "id", "code", "short_name", "decimals", "symbol", "name",
    "market_rate", "is_enabled", "created_at", "updated_at"
)
SELECT gen_random_uuid(), 'NGN', 'Naira', 2, '₦', 'Nigerian Naira', 1500.00, true, now(), now()
WHERE NOT EXISTS (SELECT 1 FROM "fiat_currencies" WHERE "code" = 'NGN');

-- Backfill the edge for the banks seeded by 20240613143010 that never got one.
-- Scoped to that exact code list (not "every unlinked row") so a hand-added
-- institution is never silently relabelled as NGN.
UPDATE "institutions" i
SET "fiat_currency_institutions" = f."id",
    "updated_at" = now()
FROM "fiat_currencies" f
WHERE f."code" = 'NGN'
  AND i."fiat_currency_institutions" IS NULL
  AND i."code" IN (
      'ABNGNGLA', 'DBLNNGLA', 'FIDTNGLA', 'FCMBNGLA', 'FBNINGLA', 'GTBINGLA',
      'PRDTNGLA', 'UBNINGLA', 'UNAFNGLA', 'CITINGLA', 'ECOCNGLA', 'HBCLNGLA',
      'PLNINGLA', 'SBICNGLA', 'SCBLNGLA', 'NAMENGLA', 'ICITNGLA', 'SUTGNGLA',
      'PROVNGLA', 'KDHLNGLA', 'GMBLNGLA', 'FSDHNGLA', 'FIRNNGLA', 'JAIZNGLA',
      'ZEIBNGLA', 'WEMANGLA', 'KUDANGPC', 'OPAYNGPC', 'MONINGPC', 'PALMNGPC',
      'SAHVNGPC', 'PAYTNGPC'
  );
