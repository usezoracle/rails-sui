# Admin Console API

Operator endpoints under `/v1/admin/`. All gated by the shared-secret
`X-Admin-Token` header (`cards.AdminTokenMiddleware`, value = `ADMIN_API_TOKEN`).
Every **write** appends an immutable row to `admin_audit_logs` (actor = request
IP, until RBAC lands).

> Money-movement and refunds are **dry-run by default** — they require
> `"confirm": true` in the body to execute. Without it they return the plan.

## Transactions — full step-by-step timeline
| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/admin/transactions?page=&limit=&status=` | List orders (newest first). |
| GET | `/v1/admin/transactions/:id` | **Every lifecycle step** of one order, chronological, merged across order status, transaction log, Route A bridge events, and Route B lock order + fulfillments. |

Each step: `{source, step, status, at, actor?, network?, tx_hash?, error?, detail}`.
`source` ∈ `payment_order | transaction_log | route_a | lock_order | fulfillment`.

## Funding
| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/admin/funding/balances` | Base aggregator (ETH+USDC), Sui aggregator (gas), Safe Haven main float + LP sub-accounts. Each source degrades gracefully + low-balance flags. |
| POST | `/v1/admin/funding/transfer` | Safe Haven payout. **Money movement.** Body: `debit_account?`, `beneficiary_bank_code`, `beneficiary_account`, `amount`, `narration?`, `reference` (idempotency), `confirm`. Dry-run resolves the name + plan; `confirm:true` executes (idempotent on `reference`). |

## Config
| Method | Path | Purpose |
|---|---|---|
| GET / PATCH | `/v1/admin/config/currencies[/:id]` | List; set `market_rate` / `is_enabled`. |
| GET / PATCH | `/v1/admin/config/tokens[/:id]` | List; toggle `is_enabled`. |
| GET | `/v1/admin/config/networks` | List networks. |
| GET / PATCH | `/v1/admin/config/providers[/:id]` | List LPs; set `is_active` / `is_kyb_verified` / `safehaven_account_number`. |
| GET | `/v1/admin/config/params` | Static env params (read-only; redeploy to change). |

## Refunds
| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/admin/orders/:id/refund` | Mark a payment order refunded (reconciliation after an out-of-band fund return). Body: `justification`, `confirm`. Idempotent; does **not** itself move funds — use `/funding/transfer` or on-chain for the actual return. |

## Notes / follow-ups
- Auth is a shared secret. Before this grows, replace with **RBAC + per-operator
  identity** (the audit `actor` becomes a user id).
- `/funding/transfer` currently covers the Safe Haven (NGN) rail. On-chain
  sweeps/top-ups (Base/Sui aggregator) are a follow-up.
- The refund endpoint records the decision + flips status; wiring it to the
  actual on-chain `refund_order` / Safe Haven reversal is the next step.
