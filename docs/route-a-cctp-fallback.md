# Route A: direct-CCTP bridge fallback

When LiFi can't quote, USDC-source Route-A orders no longer death-march
to FAILED — after **3 consecutive quote failures** the dispatcher
bridges them itself over Circle's CCTP v1 (the same burn-and-mint rail
Wormhole's CCTP product and LiFi's `cctp` tool ride on, minus their
relayer: we redeem on Base with the aggregator signer we already run).

## Quote-time fallback (rates without LiFi)

`QuoteSuiTokenToNgn` no longer hard-depends on LiFi for native-USDC
orders. If LiFi errors (or returns an unusable `ToAmountMin`), the
Sui→Base leg is priced the way CCTP actually executes it: **1:1, zero
fee, zero slippage** — `usdcEquivalent = bridgedAmount` — then the
usual sender-fee factor and aggregator NGN/USDC rate apply. The result
carries `QuotedVia="cctp"`, and order creation persists that as
`bridge_provider`, so the dispatcher executes those orders directly on
the CCTP rail (`startCCTPBridge`, never LiFi). That pairing is the
correctness invariant: a 1:1 quote must never be filled by a
fee-taking LiFi bridge, or the Gateway dispatch could come up short.

Native-SUI quotes still require LiFi (CCTP has no swap leg) and fail
exactly as before when it's down.

## Why this design

- The Gateway dispatch needs **native** Circle USDC on Base. Wormhole's
  token bridge delivers wrapped USDC (unusable downstream); Mayan /
  Wormhole Settlement needs their TypeScript SDK (a Node sidecar).
  Direct CCTP is the only pure-Go path that delivers native USDC.
- Isolation by construction: all fallback code lives in
  `services/cctp/`, `services/evm/cctp.go`, and
  `services/route_a_cctp.go`. The dispatcher has exactly three branch
  points, each gated on state only the fallback creates
  (`bridge_provider="cctp"` or the LiFi failure counter). Orders with
  `bridge_provider="lifi"` — every pre-existing row — execute the
  same code they always did.
- 1:1 amounts, no slippage, no quote, no tool selection. Three steps,
  each independently inspectable:
  1. **Burn** on Sui (`deposit_for_burn`) — digest on Suiscan.
  2. **Attest** — `GET {iris}/v1/messages/8/{digest}` at Circle.
  3. **Mint** on Base (`receiveMessage`) — receipt on Basescan.

## Scope

- Source coin must be canonical native Sui USDC (mainnet
  `0xdba3…::usdc::USDC`, testnet `0xa1ec…::usdc::USDC`). Native-SUI
  orders need a swap leg — that's LiFi's job; they keep the original
  retry-then-fail behavior.
- Sui is CCTP **v1 only** (domain 8 → Base domain 6). All package /
  object / contract constants are baked into `services/cctp/network.go`
  (Circle's v1 deployments are frozen); the chain pair is selected by
  `BASE_CHAIN_ID` (8453 ↔ Sui mainnet, 84532 ↔ Sui testnet).

## State machine (reuses the existing one)

```
pending (bridge_provider=cctp, quoted via CCTP) ──────────────┐
pending → [LiFi quote fails ×3, eligible] → CCTP burn submitted
        → bridging (bridge_provider=cctp, lifi_tool=cctp, bridge_tx_sui=digest)
        → poll Iris each tick:
            not indexed past 20 min  → bridge_uncertain (24h Iris re-poll, then failed)
            attestation pending      → keep polling
            attested                 → usedNonces check → receiveMessage on Base
        → bridged (bridged_amount = burn amount, bridge_tx_dest = mint tx)
        → ... unchanged dispatch path (approve + createOrder / treasury)
```

Crash-safe by statelessness: the persisted burn digest is the only key;
message, attestation, amount, and replay status are re-derived from
Circle + chain every tick. If the process dies after the mint tx but
before persisting, the next tick sees `usedNonces=true` and just
records the outcome. The mint refuses to run if the attested message's
destination domain or mint recipient don't match our config — a wrong
constant surfaces as a loud refusal, never a misdirected mint.

## Config

| Env | Default | Meaning |
| --- | --- | --- |
| `CCTP_FALLBACK_ENABLED` | `true` | Kill switch. `false` restores exact pre-fallback behavior. |
| `CCTP_IRIS_URL` | per-network Circle host | Attestation host override (tests/proxies). |

No other new config: Sui-side gas comes from the aggregator's existing
SUI balance (budget 0.05 SUI/burn), Base-side mint gas from the
existing Base signer, recipient is `BASE_AGGREGATOR_ADDRESS`.

## Failure modes

- Fallback ineligible/broken → logged reason, LiFi counter continues to
  10 → order fails exactly as before. The fallback can only add a
  recovery path, never remove one.
- Burn submitted but persist failed → loud log with digest; ops
  resolves (same exposure as the LiFi submit-then-persist window).
- Circle attestation never arrives → bridge_uncertain after 20 min,
  failed after 24 h (same policy as LiFi tools).
