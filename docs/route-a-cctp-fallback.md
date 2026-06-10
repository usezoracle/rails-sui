# Route A: bridge rails — CCTP primary (USDC), LiFi fallback

Native-USDC orders bridge Sui→Base over Circle's CCTP v1 as the
**primary** rail (the same burn-and-mint rail Wormhole's CCTP product
and LiFi's `cctp` tool ride on, minus their relayer: we redeem on Base
with the aggregator signer we already run). LiFi is the USDC
**fallback** — after 3 consecutive CCTP failures, gated by a fit-guard
— and remains the only rail for native SUI, which needs a swap leg.

## Quote-time pricing

For native-USDC, `QuoteSuiTokenToNgn` prices the Sui→Base leg the way
CCTP executes it — **1:1, zero fee, zero slippage**
(`usdcEquivalent = bridgedAmount`) — with no LiFi call at all; then the
usual sender-fee factor and aggregator NGN/USDC rate apply. The result
carries `QuotedVia="cctp"`, persisted as `bridge_provider` at order
creation.

The correctness invariant: **a 1:1 quote must never be silently filled
by a fee-taking bridge.** If CCTP keeps failing at bridge time, the
LiFi fallback (`tryLiFiFallback`) first probes LiFi's `ToAmountMin` and
the live settlement rate, and only takes over when the guaranteed
minimum delivery still covers the order's NGN target plus sender fee
(`lifiCoversQuote`). Otherwise the order stays on CCTP retries →
failed → refund, with funds still safe on Sui.

Native-SUI quotes still go through LiFi (CCTP has no swap leg). USDC
quotes also revert to LiFi when `CCTP_FALLBACK_ENABLED=false` or the
chain has no CCTP deployment.

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
pending (USDC, CCTP-eligible) → CCTP burn submitted            [primary]
pending → [CCTP fails ×3 + LiFi fit-guard passes] → LiFi path  [fallback]
pending → [LiFi fails ×3, USDC-eligible] → CCTP burn submitted [reverse fallback]
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
