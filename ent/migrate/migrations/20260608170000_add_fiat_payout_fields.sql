-- Modify "lock_payment_orders" table
ALTER TABLE "lock_payment_orders" ADD COLUMN "fiat_payout_reference" character varying NULL, ADD COLUMN "fiat_payout_session_id" character varying NULL, ADD COLUMN "fiat_payout_status" character varying NOT NULL DEFAULT 'none', ADD COLUMN "fiat_payout_error" character varying NULL;
