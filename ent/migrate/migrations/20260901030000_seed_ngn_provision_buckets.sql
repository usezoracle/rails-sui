-- Seed the NGN provision buckets (data migration; idempotent, non-destructive).
--
-- Every currency's *_institutions.sql seeds three provision buckets alongside
-- its banks — except NGN. 20240613143010_ngn_institutions.sql predates that
-- pattern (it only inserts institutions), so NGN is the one currency with zero
-- buckets, and nothing since has added them.
--
-- That gap is not cosmetic. sui_event_indexer.go looks the bucket up by
-- (amount, currency), logs "no provision bucket … will not be matched" and
-- deliberately carries on with a nil bucket; priority_queue.go then reads
-- order.ProvisionBucket.Edges.Currency.Code with no nil guard. A missing
-- bucket is therefore a nil-pointer panic in the matching engine, not a
-- skipped match.
--
-- Ranges mirror every other currency (0-1000 / 1001-5000 / 5001-50000).
-- NOTE: these are naira, so the top of the range is ~₦50,000. Orders above
-- that find no bucket and hit the path described above until the ranges are
-- widened.
--
-- ids follow the existing convention here: explicit, continuing from the
-- current MAX. They stay well below the table's identity START WITH
-- (42949672960, ent's global-unique-ID range), so the sequence is untouched.

DO $$
DECLARE
    ngn_currency_id UUID;
    last_bucket_id  BIGINT;
BEGIN
    SELECT "id" INTO ngn_currency_id
    FROM "fiat_currencies"
    WHERE "code" = 'NGN';

    IF ngn_currency_id IS NULL THEN
        RAISE EXCEPTION 'NGN fiat currency missing — 20260901020000_seed_ngn_currency_and_link_institutions.sql must run first';
    END IF;

    -- Already seeded? Nothing to do.
    IF EXISTS (
        SELECT 1 FROM "provision_buckets"
        WHERE "fiat_currency_provision_buckets" = ngn_currency_id
    ) THEN
        RETURN;
    END IF;

    SELECT COALESCE(MAX("id"), 0) INTO last_bucket_id FROM "provision_buckets";

    INSERT INTO "provision_buckets" ("id", "min_amount", "max_amount", "created_at", "fiat_currency_provision_buckets")
    VALUES
        (last_bucket_id + 1, 0,    1000,  now(), ngn_currency_id),
        (last_bucket_id + 2, 1001, 5000,  now(), ngn_currency_id),
        (last_bucket_id + 3, 5001, 50000, now(), ngn_currency_id);
END$$;
