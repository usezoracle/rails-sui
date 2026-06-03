# Refactor Plan — Separation of Concerns

Staged, independently-shippable PRs. Each phase builds, deploys, and is reviewable
on its own. We never big-bang a live fintech backend.

## Findings (baseline)

- **Controllers reach the DB directly** — `storage.Client`/ent used in ~10
  controller files; no repository/service boundary. Transport + business logic +
  persistence are fused.
- **God files** — `controllers/index.go` (1390), `services/route_a_dispatcher.go`
  (1100), `services/sui_event_indexer.go` (1064), `controllers/sender/sender.go`
  (1036), `tasks/tasks.go` (858), `controllers/provider/provider.go` (856), …
- **Global config singletons** — `var xConf = config.XConfig()` in 42+ spots,
  loaded at import via a shared viper global. Hidden coupling; breaks hermetic tests.
- **Thin tests** — 15 test files for 130 source files.
- **Pre-existing breakage on `main`** (independent of this work):
  - `scripts/` has multiple `package main` / `func main()` → `go build ./...`
    fails for that package. (The `chore/wip-snapshot` branch relocates them.)
  - `utils/crypto/crypto_test.go` references removed `cryptoConf.HDWalletMnemonic`.
  - `controllers/accounts/auth_test.go` references removed `token.GenerateRefreshJWT`.
  - `SSL_MODE=disable` default; `JWT_SIGNING_KEY` falls back to `SECRET`.

## Phases

### Phase 1 — Hygiene & safety  ✅ (this PR)
- Untrack committed binaries + log (`main`, `rails-sui`, `server.log`) + gitignore.
- Complete `.env.example` (all vars, testnet/mainnet tags, no secrets).
- Fix `utils/http.go` non-constant format string (vet).

### Phase 2 — Config DI
- Load config once into a struct; pass it down. Remove package-level
  `var xConf = config.XConfig()` singletons. Keep `config` free of side-effecting
  `init()`; construct at composition root. Unblocks hermetic tests.

### Phase 3 — Repository layer
- Introduce `repo/` (or `store/`) wrapping ent. Controllers/services depend on
  repo interfaces, not `storage.Client` directly. Enables mocking + isolates schema.

### Phase 4 — Decompose god files
- Split `controllers/index.go`, `route_a_dispatcher.go`, `sui_event_indexer.go`,
  `sender.go` by responsibility (handlers vs orchestration vs IO). One file/PR.

### Phase 5 — Test uplift
- Fix/replace the dead tests (`crypto_test`, `auth_test`), add coverage for the
  money paths (Route A/B/C, settlement, webhooks) as the layers above land.

### Cross-cutting hardening
- `SSL_MODE=require` in prod; distinct `JWT_SIGNING_KEY`; add a `Dockerfile`;
  resolve the `scripts/` package (coordinate with `chore/wip-snapshot`).

## Branch / merge notes
- In-progress work parked on `chore/wip-snapshot` (Sui gRPC + route_a/controllers).
  It overlaps Phases 3–4 heavily — sequence: land/merge that first, or rebase these
  phases on top, to avoid painful conflicts in the god files it also edits.
