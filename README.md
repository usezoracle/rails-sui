# Rails

**Sui-native B2B settlement infrastructure.** Converts Sui stablecoins (USDC) into local fiat via a decentralized liquidity-provider protocol and optional cross-chain bridging.

Two settlement routes:

- **Route B (Sui LP):** Sui USDC locked in an on-chain Gateway escrow; LPs deposit fiat into virtual bank accounts they control; matching engine pairs orders with LPs; aggregator releases the coin to the LP's wallet when the LP's fiat payout to the merchant is confirmed.
- **Route A (Bridging):** Sui USDC bridged to BSC via LiFi, then either re-entered into the existing EVM Gateway/LP system or settled from a centralized treasury — caller's choice per transaction.

The aggregator never custodies user funds: LPs own their fiat (held by a BaaS partner under a mandate), and the on-chain Gateway holds escrow released only by the smart contract itself.

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
