// route_a_cctp.go — the direct-CCTP bridge fallback for Route A.
//
// Everything fallback-specific lives in this file + services/cctp +
// services/evm/cctp.go. The LiFi path in route_a_dispatcher.go is
// untouched except for three branch points:
//
//  1. advancePending: after cctpFallbackAfter consecutive LiFi quote
//     failures, eligible orders are bridged here instead of burning
//     more retries toward FAILED.
//  2. advanceBridging: orders with bridge_provider="cctp" poll Circle
//     (pollOneCCTP) instead of LiFi /status.
//  3. advanceUncertain: same split for the late-recovery loop.
//
// Orders with bridge_provider="lifi" (the default, including every
// pre-existing row) never enter any code in this file — if a fallback
// bridge misbehaves, the blast radius is the fallback.
//
// Why direct CCTP and not Wormhole's own products: the Gateway dispatch
// needs NATIVE Circle USDC on Base. Wormhole's token bridge (WTT)
// delivers wrapped USDC (unusable downstream), and Wormhole's CCTP /
// Settlement products are this same Circle burn-and-mint rail plus a
// relayer layer we don't need — the dispatcher already runs a funded
// Base signer, so we redeem the mint ourselves. Fewer moving parts,
// each step inspectable: burn digest on Suiscan, attestation at Circle,
// mint receipt on Basescan.
//
// Scope: USDC-source orders only. Native-SUI orders need a swap leg,
// which is exactly the job LiFi exists for — they keep the original
// retry-then-fail behavior.
package services

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	suimodels "github.com/block-vision/sui-go-sdk/models"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/routeaorder"
	"github.com/usezoracle/rails-sui/services/cctp"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// cctpFallbackAfter is how many consecutive LiFi quote failures an
// order accrues before the dispatcher tries the CCTP fallback. Ticks
// are one minute apart, so 3 ≈ three minutes of LiFi being down —
// long enough to skip transient blips, short enough that card
// settlements don't sit for the full 10-retry death march.
const cctpFallbackAfter = 3

// routeAProviderCCTP is the bridge_provider value marking orders that
// bridged via the direct-CCTP fallback. Default rows are "lifi".
const routeAProviderCCTP = "cctp"

// cctpQuoteAvailable reports whether a quote-time CCTP fallback rate
// (1:1, fee-free) is valid for this source token: the fallback must be
// enabled, the destination chain must have a CCTP deployment, and the
// token must be the canonical native Sui USDC that deposit_for_burn
// accepts. Used by QuoteSuiTokenToNgn when LiFi can't quote.
func cctpQuoteAvailable(conf *config.OrderConfiguration, fromToken string) bool {
	if !conf.CCTPFallbackEnabled {
		return false
	}
	net, ok := cctp.ForBaseChainID(conf.BaseChainID)
	return ok && fromToken == net.SuiUSDCCoinType
}

// cctpReady reports whether the fallback can run at all, with a reason
// for the log when it can't. Checked per attempt, not at boot, so a
// config fix applies on the next tick without a restart.
func (d *RouteADispatcher) cctpReady() (string, bool) {
	switch {
	case !d.conf.CCTPFallbackEnabled:
		return "CCTP_FALLBACK_ENABLED=false", false
	case !d.cctpNetOK:
		return fmt.Sprintf("no CCTP deployment for chain id %d", d.conf.BaseChainID), false
	case d.evm == nil:
		return "Base EVM client not configured (needed for the mint leg)", false
	case d.conf.BaseAggregatorAddress == "":
		return "BASE_AGGREGATOR_ADDRESS not set", false
	default:
		return "", true
	}
}

// startBridgeForProvider is advancePending's entry point: orders
// already tagged bridge_provider="cctp" (quoted via CCTP because LiFi
// was down at order time) start directly on the CCTP rail — their 1:1
// quote must not be filled by a fee-taking LiFi bridge. Everything
// else takes the unchanged LiFi path.
func (d *RouteADispatcher) startBridgeForProvider(ctx context.Context, order *ent.RouteAOrder) error {
	if order.BridgeProvider == routeAProviderCCTP {
		return d.startCCTPBridge(ctx, order, 0)
	}
	return d.startBridge(ctx, order)
}

// tryCCTPFallback attempts to take over a pending LiFi order whose
// quotes keep failing. Returns nil when the burn tx is submitted and
// the order has moved to bridging under bridge_provider="cctp";
// any error leaves the order exactly where the LiFi retry loop had it.
func (d *RouteADispatcher) tryCCTPFallback(ctx context.Context, order *ent.RouteAOrder, lifiFailures int) error {
	return d.startCCTPBridge(ctx, order, lifiFailures)
}

// startCCTPBridge burns the order's USDC on Sui via deposit_for_burn
// and moves the order to bridging. Reached two ways: directly for
// orders quoted via CCTP (lifiFailures=0), or as the fallback after
// repeated LiFi quote failures (lifiFailures>0).
func (d *RouteADispatcher) startCCTPBridge(ctx context.Context, order *ent.RouteAOrder, lifiFailures int) (err error) {
	if reason, ok := d.cctpReady(); !ok {
		return fmt.Errorf("cctp fallback unavailable: %s", reason)
	}

	po := order.Edges.PaymentOrder
	if po == nil || po.Edges.Token == nil {
		return fmt.Errorf("order %d missing payment_order/token edge", order.ID)
	}
	tok := po.Edges.Token

	// Eligibility gate: the source coin must be the canonical native
	// USDC on Sui — that's the only asset CCTP can burn. Anything else
	// (native SUI, bridged wUSDC) stays on the LiFi path.
	if tok.ContractAddress != d.cctpNet.SuiUSDCCoinType {
		return fmt.Errorf("source coin %s is not native Sui USDC — CCTP can't bridge it", tok.ContractAddress)
	}

	timer := Time(ctx, order.ID, StepBridgeSubmit, ActorDispatcher).
		With("provider", routeAProviderCCTP)
	if lifiFailures > 0 {
		timer.With("lifi_failures_before_fallback", lifiFailures)
	}
	defer timer.End(&err)

	amountSubunits, err := usdcSubunitsUint64(po.Amount, int(tok.Decimals))
	if err != nil {
		return fmt.Errorf("order %d amount: %w", order.ID, err)
	}

	// Same pre-flight as the LiFi path (and for the same incident-
	// shaped reason — see startBridge): never burn before the deposit
	// has actually self-settled to the aggregator wallet.
	balResp, err := d.suiClient.SuiXGetBalance(ctx, suimodels.SuiXGetBalanceRequest{
		Owner:    d.signer.Address,
		CoinType: tok.ContractAddress,
	})
	if err != nil {
		return fmt.Errorf("check aggregator balance: %w", err)
	}
	have, ok := new(big.Int).SetString(balResp.TotalBalance, 10)
	if !ok {
		return fmt.Errorf("parse aggregator balance %q", balResp.TotalBalance)
	}
	need := new(big.Int).SetUint64(amountSubunits)
	if order.BridgeStatus == routeaorder.BridgeStatusAwaitingFunds &&
		!hasSuccessfulRouteAEvent(ctx, order.ID, StepSelfSettle) {
		timer.With("aggregator_have", have.String()).
			With("need", need.String()).
			With("coin_type", tok.ContractAddress)
		return ErrAwaitingDepositAtAggregator
	}
	if have.Cmp(need) < 0 {
		logger.Infof("🤔 route-a: order %d awaiting funds (cctp) — aggregator has %s, need %s subunits; will recheck next tick",
			order.ID, have, need)
		timer.With("aggregator_have", have.String()).
			With("need", need.String()).
			With("coin_type", tok.ContractAddress)
		return ErrAwaitingDepositAtAggregator
	}

	recipient := ethcommon.HexToAddress(d.conf.BaseAggregatorAddress)

	digest, err := cctp.SubmitBurn(ctx, d.suiClient, d.signer, cctp.BurnRequest{
		Net:            d.cctpNet,
		AmountSubunits: amountSubunits,
		MintRecipient:  recipient,
	})
	if err != nil {
		return fmt.Errorf("cctp burn: %w", err)
	}

	if _, perr := order.Update().
		SetBridgeProvider(routeAProviderCCTP).
		SetLifiTool("cctp"). // reuses the per-tool stale window + admin display
		SetBridgeTxSui(digest).
		SetBridgeStatus(routeaorder.BridgeStatusBridging).
		Save(ctx); perr != nil {
		// Burn is on-chain but the row still says pending — the next
		// tick would double-bridge. Surface loudly; ops resolves with
		// the digest (same persist-after-submit exposure the LiFi path
		// has always had, no wider).
		logger.Errorf("❌ route-a: CCTP burn submitted (tx=%s) but persist failed for %d: %v — "+
			"manual intervention needed to avoid double-bridge", digest, order.ID, perr)
		err = fmt.Errorf("persist cctp bridging state: %w", perr)
		return err
	}
	timer.With("bridge_tx_sui", digest).
		With("amount_subunits", amountSubunits).
		With("mint_recipient", recipient.Hex())
	logger.Infof("✅ route-a: CCTP fallback bridge initiated order=%d tx=%s (after %d LiFi quote failures)",
		order.ID, digest, lifiFailures)
	return nil
}

// pollOneCCTP advances one bridging CCTP order. Stateless against our
// DB: the persisted burn digest is the only key — message, attestation,
// amount, and replay status are all re-derived from Circle + chain
// every tick, so crashes at any point self-heal.
//
//	burn digest → Iris message+attestation → receiveMessage on Base → bridged
func (d *RouteADispatcher) pollOneCCTP(ctx context.Context, order *ent.RouteAOrder) {
	var err error
	timer := Time(ctx, order.ID, StepBridgePoll, ActorDispatcher).
		With("provider", routeAProviderCCTP).
		With("bridge_tx_sui", order.BridgeTxSui)
	defer timer.End(&err)

	att, ierr := d.cctpIris.MessageFor(ctx, d.cctpNet.SuiDomain, order.BridgeTxSui)
	switch {
	case errors.Is(ierr, cctp.ErrAttestationPending):
		timer.With("cctp_status", "attestation_pending")
		return
	case errors.Is(ierr, cctp.ErrNotIndexed):
		timer.With("cctp_status", "not_indexed")
		// Same stale policy as the LiFi path: give Circle the per-tool
		// window, then park in bridge_uncertain for the 24h recovery
		// loop rather than failing an order whose burn IS on-chain.
		staleAfter := bridgeStaleTimeoutFor("cctp")
		if time.Since(order.UpdatedAt) > staleAfter {
			reason := fmt.Sprintf("Circle hasn't indexed burn tx %s after %s — transitioned to bridge_uncertain",
				order.BridgeTxSui, staleAfter)
			if _, uerr := order.Update().
				SetBridgeStatus(routeaorder.BridgeStatusBridgeUncertain).
				SetFailureReason(reason).
				Save(ctx); uerr != nil {
				logger.Errorf("❌ route-a: persist bridge_uncertain for %d: %v", order.ID, uerr)
				err = uerr
				return
			}
			logger.Infof("🤔 route-a: order %d → bridge_uncertain (cctp, stale_after=%s)", order.ID, staleAfter)
			timer.With("transitioned", "bridge_uncertain")
		}
		return
	case ierr != nil:
		logger.Errorf("❌ route-a: iris poll for %d (tx=%s): %v", order.ID, order.BridgeTxSui, ierr)
		err = ierr
		return
	}

	if err = d.finishCCTPMint(ctx, order, att, timer); err != nil {
		logger.Errorf("❌ route-a: cctp mint for %d: %v", order.ID, err)
	}
}

// finishCCTPMint validates the attested message, submits (or skips, if
// already mined) the Base receiveMessage, and marks the order bridged.
func (d *RouteADispatcher) finishCCTPMint(ctx context.Context, order *ent.RouteAOrder, att *cctp.AttestedMessage, timer *Timer) error {
	msg, err := cctp.ParseBurnMessage(att.Message)
	if err != nil {
		return err
	}

	// Sanity-assert the message routes where we think it does. A
	// mismatch means our network constants are wrong — exactly the
	// failure class the fallback should surface loudly, not act on.
	wantRecipient := ethcommon.HexToAddress(d.conf.BaseAggregatorAddress)
	if msg.DestinationDomain != d.cctpNet.BaseDomain {
		return fmt.Errorf("message destination domain %d, want %d — refusing to mint", msg.DestinationDomain, d.cctpNet.BaseDomain)
	}
	if got := ethcommon.Address(msg.MintRecipientEVM()); got != wantRecipient {
		return fmt.Errorf("message mint recipient %s, want %s — refusing to mint", got.Hex(), wantRecipient.Hex())
	}

	transmitter := d.evm.CCTPTransmitter(ethcommon.HexToAddress(d.cctpNet.BaseMessageTransmitter))

	// Replay check first: if a previous tick (or crashed process)
	// already minted, this poll just records the outcome.
	used, err := transmitter.NonceUsed(ctx, msg.SourceDomain, msg.Nonce)
	if err != nil {
		return fmt.Errorf("usedNonces: %w", err)
	}

	destTxHash := ""
	if !used {
		receipt, rerr := transmitter.ReceiveMessage(ctx, att.Message, att.Attestation)
		if rerr != nil {
			return fmt.Errorf("receiveMessage: %w", rerr)
		}
		destTxHash = receipt.TxHash.Hex()
	} else {
		timer.With("mint", "already_received")
	}

	bridgedAmount := decimal.NewFromBigInt(msg.Amount, -int32(d.conf.BaseUSDCDecimals))
	update := order.Update().
		SetBridgeStatus(routeaorder.BridgeStatusBridged).
		SetBridgedAmount(bridgedAmount).
		SetFailureReason("") // clear any uncertain marker
	if destTxHash != "" {
		update = update.SetBridgeTxDest(destTxHash)
	}
	if _, perr := update.Save(ctx); perr != nil {
		return fmt.Errorf("persist bridged: %w", perr)
	}

	timer.With("bridge_tx_dest", destTxHash).
		With("bridged_amount", bridgedAmount.String()).
		With("cctp_nonce", msg.Nonce)
	logger.Infof("✅ route-a: CCTP bridge DONE order=%d amount=%s USDC dest_tx=%s",
		order.ID, bridgedAmount.String(), destTxHash)
	LogOnce(ctx, order.ID, StepBridgeDone, StatusSucceeded, ActorDispatcher,
		map[string]any{
			"provider":       routeAProviderCCTP,
			"bridge_tx_dest": destTxHash,
			"cctp_nonce":     msg.Nonce,
		}, "", "")
	return nil
}

// recoverUncertainCCTP is the bridge_uncertain handler for CCTP orders:
// keep re-asking Circle (the burn is on-chain; the attestation WILL
// exist eventually unless the tx itself failed), mint when it lands,
// and fail only after the same 24h window the LiFi path uses.
func (d *RouteADispatcher) recoverUncertainCCTP(ctx context.Context, order *ent.RouteAOrder) {
	var err error
	timer := Time(ctx, order.ID, StepBridgeUncertain, ActorDispatcher).
		With("provider", routeAProviderCCTP).
		With("bridge_tx_sui", order.BridgeTxSui).
		With("age", time.Since(order.UpdatedAt).String())
	defer timer.End(&err)

	att, ierr := d.cctpIris.MessageFor(ctx, d.cctpNet.SuiDomain, order.BridgeTxSui)
	if ierr == nil {
		if err = d.finishCCTPMint(ctx, order, att, timer); err == nil {
			timer.With("recovered_via", "late_attestation")
			return
		}
		logger.Errorf("❌ route-a: late cctp mint for %d: %v", order.ID, err)
		return
	}
	timer.With("iris_error", ierr.Error())

	if time.Since(order.UpdatedAt) > uncertainRecoveryWindow {
		reason := fmt.Sprintf("uncertain past %s window; Circle still has no attestation for %s",
			uncertainRecoveryWindow, order.BridgeTxSui)
		if _, uerr := order.Update().
			SetBridgeStatus(routeaorder.BridgeStatusFailed).
			SetFailureReason(reason).
			Save(ctx); uerr != nil {
			logger.Errorf("❌ route-a: persist window-expired FAILED for %d: %v", order.ID, uerr)
		}
		timer.With("recovered_via", "window_expired_to_failed")
	}
}

// usdcSubunitsUint64 converts a decimal USDC amount to uint64 subunits
// (the Move u64 deposit_for_burn takes).
func usdcSubunitsUint64(amount decimal.Decimal, decimals int) (uint64, error) {
	shifted := amount.Shift(int32(decimals)).Truncate(0)
	n, ok := new(big.Int).SetString(shifted.String(), 10)
	if !ok || n.Sign() <= 0 || !n.IsUint64() {
		return 0, fmt.Errorf("amount %s (decimals=%d) is not a positive u64 subunit value", amount, decimals)
	}
	return n.Uint64(), nil
}
