-- Seed Sui networks + USDC token (data migration; idempotent, non-destructive).
-- Prod was built from migrations, which seed currencies/institutions but NOT
-- networks/tokens (those only existed in the destructive seed script). Without a
-- USDC token row, provider rate config fails with "Token not supported".

-- Networks (identifier is unique; insert only when missing).
INSERT INTO "networks" ("created_at", "updated_at", "chain_id", "identifier", "rpc_endpoint", "is_testnet", "fee")
SELECT now(), now(), 0, 'sui-mainnet', 'https://fullnode.mainnet.sui.io:443', false, 0
WHERE NOT EXISTS (SELECT 1 FROM "networks" WHERE "identifier" = 'sui-mainnet');

INSERT INTO "networks" ("created_at", "updated_at", "chain_id", "identifier", "rpc_endpoint", "is_testnet", "fee")
SELECT now(), now(), 0, 'sui-testnet', 'https://fullnode.testnet.sui.io:443', true, 0
WHERE NOT EXISTS (SELECT 1 FROM "networks" WHERE "identifier" = 'sui-testnet');

-- USDC on each Sui network (insert only when missing for that network).
INSERT INTO "tokens" ("created_at", "updated_at", "symbol", "contract_address", "decimals", "is_enabled", "network_tokens")
SELECT now(), now(), 'USDC', '0xdba34672e30cb065b1f93e3ab55318768fd6fef66c15942c9f7cb846e2f900e7::usdc::USDC', 6, true, n."id"
FROM "networks" n
WHERE n."identifier" = 'sui-mainnet'
  AND NOT EXISTS (SELECT 1 FROM "tokens" t WHERE t."symbol" = 'USDC' AND t."network_tokens" = n."id");

INSERT INTO "tokens" ("created_at", "updated_at", "symbol", "contract_address", "decimals", "is_enabled", "network_tokens")
SELECT now(), now(), 'USDC', '0xa1ec7fc00a6f40db9693ad1415d0c193ad3906494428cf252621037bd7117e29::usdc::USDC', 6, true, n."id"
FROM "networks" n
WHERE n."identifier" = 'sui-testnet'
  AND NOT EXISTS (SELECT 1 FROM "tokens" t WHERE t."symbol" = 'USDC' AND t."network_tokens" = n."id");
