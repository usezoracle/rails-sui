# Rails

**Sui-native settlement infrastructure.** Rails converts stablecoins into local fiat in emerging markets through a decentralized LP settlement system built on Sui. It is designed for fintechs, consumer apps, DeFi protocols, and cross-border payment systems that need fast, composable settlement without taking custody of user funds.

Rails supports native SUI, USDC, and USDT and can settle into local currencies such as KES, NGN, TZS, and MWK. Settlement happens on Sui through on-chain escrow plus liquidity-provider rails: an order is created, LP capacity is matched, fiat is paid out from the LP rail, and the escrowed value is released only after payout is confirmed.

Tapp is the proof of work for how products can be built on Rails. It combines the cardholder and merchant experiences into one product flow, showing how users can pay, merchants can accept, and settlement can move through Rails with a Sui wallet and local payout rails underneath.

Rails is built for speed and composability. We have had transactions settle in minutes, and the system is designed so new payment experiences can be composed on top of the same settlement layer without changing the core rails.

The system never custodies user funds long term: LPs own their fiat, the on-chain gateway holds escrow until settlement conditions are met, and Rails coordinates the lifecycle without becoming the asset holder.

## Layout

```
contracts/gateway/    Sui Move package (Gateway + Order escrow + events)
services/             Go services (RPC client, event indexer, deposit watcher, matching engine)
controllers/          HTTP handlers (Gin)
ent/                  Database schema (Ent + Postgres)
config/               Environment + config loaders
docs/                 Architecture + per-component specs + handoff notes
tasks/                Cron jobs + StartCronJobs entrypoint
```

## Development

Prereqs: Go 1.22+, Sui CLI, Postgres 13+, Redis.

```bash
# clone
git clone git@github.com:usezoracle/rails-sui.git
cd rails-sui

# config
cp .env.example .env
# edit .env with your local DB, Redis, Sui RPC, etc.

# database
createdb rails
go run -mod=mod entgo.io/ent/cmd/ent generate --feature sql/versioned-migration --feature sql/upsert ./ent/schema/

# redis
brew services start redis

# build + test the Move package
cd contracts/gateway && sui move build && sui move test
cd ../..

# build + run the Go backend
go build ./...
air     # or: go run main.go
```

Server listens at `http://localhost:8000`.

### Seeding

```bash
go run scripts/seed/main.go
```

## Architecture

Read these in order:

1. **`docs/rails-architecture.md`** — system overview, custody model, deposit paths.
2. **`docs/sui-gateway-spec.md`** — Move package design (per-order shared `Order<T>` objects, aggregator-gated settle/refund).
3. **`docs/route-a-spec.md`** — LiFi bridge integration, treasury vs LP-on-BSC dispatch.
4. **`docs/ceiling-rate-spec.md`** — dynamic rate ceiling tied to off-chain spot medians.
5. **`docs/virtual-accounts-spec.md`** — LP onboarding via BaaS virtual accounts; wallet ownership proof.
6. **`docs/b2b-api-spec.md`** — public REST API surface, webhooks, idempotency.
7. **`docs/handoff-2026-05-21.md`** — current build status + critical-path checklist to first live transaction.

## Testing

```bash
go test ./...                  # Go unit tests
cd contracts/gateway && sui move test    # Move package tests (9/9 pass)
```

## License

[GNU Affero General Public License v3.0](LICENSE).
