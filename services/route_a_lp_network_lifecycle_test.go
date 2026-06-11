package services

// route_a_lp_network_lifecycle_test.go — Route B end-to-end: a
// mode=lp_network order driven by the real Tick() from pending →
// LP matched + ledger debited → merchant paid from the pooled rail →
// the order's USDC delivered to the LP on Sui → settled. NO bridge,
// NO Paycrest — the whole point of Route B.

import (
	"context"
	"strings"
	"testing"
	"time"

	suisigner "github.com/block-vision/sui-go-sdk/signer"
	suisdk "github.com/block-vision/sui-go-sdk/sui"
	_ "github.com/mattn/go-sqlite3"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent/enttest"
	"github.com/usezoracle/rails-sui/ent/lpledgerentry"
	"github.com/usezoracle/rails-sui/ent/routeaevent"
	"github.com/usezoracle/rails-sui/ent/routeaorder"
	"github.com/usezoracle/rails-sui/services/baas"
	"github.com/usezoracle/rails-sui/services/lifi"
	"github.com/usezoracle/rails-sui/storage"
)

func TestRouteBLifecycleLpNetworkToSettled(t *testing.T) {
	ctx := context.Background()

	client := enttest.Open(t, "sqlite3", "file:route_b_lifecycle?mode=memory&cache=shared&_fk=1")
	defer client.Close()
	prev := storage.Client
	storage.Client = client
	defer func() { storage.Client = prev }()

	rail := &fakeBaas{balance: decimal.NewFromInt(1_000_000)}
	baas.SetDefault(rail)
	defer baas.SetDefault(nil)

	suiSeed := make([]byte, 32)
	for i := range suiSeed {
		suiSeed[i] = byte(i + 11)
	}
	signer := suisigner.NewSigner(suiSeed)
	suiSrv := fakeSuiServer(t, signer.Address)
	defer suiSrv.Close()

	// Fixtures: an LP with NGN float + a Sui delivery address, and a
	// card-tap order routed to the LP network.
	lpUser := client.User.Create().
		SetFirstName("LP").SetLastName("One").
		SetEmail("lp-b@usetapp.xyz").SetPassword("x").SetScope("provider").
		SetIsEmailVerified(true).SaveX(ctx)
	lpSuiAddr := "0x" + strings.Repeat("99", 32)
	lp := client.LpAccount.Create().
		SetName("LP One").SetEmail(lpUser.Email).SetBvnLast4("2222").
		SetAccountReference("lp-" + lpUser.ID.String()).
		SetAccountNumber("1110033596").SetBankName("Test Bank").SetBankCode("000").
		SetBalance(decimal.NewFromInt(5_000)).
		SetSuiUsdcAddress(lpSuiAddr).
		SetUser(lpUser).SaveX(ctx)

	network := client.Network.Create().
		SetChainID(101).SetIdentifier("sui-mainnet-b").
		SetRPCEndpoint(suiSrv.URL).SetIsTestnet(false).
		SetFee(decimal.NewFromFloat(0.01)).SaveX(ctx)
	token := client.Token.Create().
		SetSymbol("USDC").SetContractAddress(suiUSDC).SetDecimals(6).
		SetIsEnabled(true).SetNetwork(network).SaveX(ctx)
	amount := decimal.RequireFromString(orderAmount) // 1.081665 USDC
	rate := decimal.RequireFromString("1386.75")
	po := client.PaymentOrder.Create().
		SetAmount(amount).
		SetAmountPaid(decimal.Zero).SetAmountReturned(decimal.Zero).
		SetPercentSettled(decimal.Zero).SetSenderFee(decimal.Zero).
		SetNetworkFee(decimal.Zero).SetProtocolFee(decimal.Zero).
		SetRate(rate).
		SetReceiveAddressText("0xstub").SetFeePercent(decimal.Zero).
		SetToken(token).SaveX(ctx)
	client.PaymentOrderRecipient.Create().
		SetInstitution("OPAYNGPC").SetAccountIdentifier("9123456789").
		SetAccountName("Route B Merchant").SetPaymentOrder(po).SaveX(ctx)
	ra := client.RouteAOrder.Create().
		SetMode(routeaorder.ModeLpNetwork).
		SetBridgeStatus(routeaorder.BridgeStatusPending).
		SetPaymentOrder(po).SaveX(ctx)

	conf := &config.OrderConfiguration{
		SuiRpcURL:           suiSrv.URL,
		CCTPFallbackEnabled: true,
		BaseChainID:         8453,
	}
	suiAPI := suisdk.NewSuiClient(suiSrv.URL)
	suiClient, _ := suiAPI.(*suisdk.Client)
	d := &RouteADispatcher{
		conf:            conf,
		suiClient:       suiClient,
		signer:          signer,
		lifi:            lifi.New(""),
		quoteFailCounts: map[int]int{},
		instanceID:      "route-b-test",
		burstWake:       make(chan struct{}, 1),
		lifiPollAt:      map[int]time.Time{},
		treasuryRail:    rail,
	}

	for i := 0; i < 5; i++ {
		if err := d.Tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		cur := client.RouteAOrder.GetX(ctx, ra.ID)
		if cur.BridgeStatus == routeaorder.BridgeStatusSettled {
			break
		}
	}

	final := client.RouteAOrder.GetX(ctx, ra.ID)
	if final.BridgeStatus != routeaorder.BridgeStatusSettled {
		t.Fatalf("bridge_status = %s, want settled (mode=%s reason=%q)",
			final.BridgeStatus, final.Mode, final.FailureReason)
	}
	if final.Mode != routeaorder.ModeLpNetwork {
		t.Fatalf("mode = %s — order must not have fallen back", final.Mode)
	}

	// Merchant paid the exact entitlement from the pooled rail.
	targetNGN := amount.Mul(rate).Round(2)
	rail.mu.Lock()
	if len(rail.transfers) != 1 {
		t.Fatalf("rail transfers = %d, want exactly 1", len(rail.transfers))
	}
	tr := rail.transfers[0]
	rail.mu.Unlock()
	if !tr.Amount.Equal(targetNGN) || tr.BeneficiaryAccount != "9123456789" {
		t.Errorf("payout = ₦%s → %s, want ₦%s → merchant", tr.Amount, tr.BeneficiaryAccount, targetNGN)
	}
	if !strings.HasPrefix(tr.PaymentReference, lpNetPayRefPrefix) {
		t.Errorf("pay ref = %q, want %s*", tr.PaymentReference, lpNetPayRefPrefix)
	}

	// LP debited exactly once, fill confirmed, USDC delivery recorded.
	freshLp := client.LpAccount.GetX(ctx, lp.ID)
	wantBal := decimal.NewFromInt(5_000).Sub(targetNGN)
	if !freshLp.Balance.Equal(wantBal) {
		t.Errorf("LP balance = %s, want %s", freshLp.Balance, wantBal)
	}
	entry := client.LpLedgerEntry.Query().OnlyX(ctx)
	if entry.EntryType != lpledgerentry.EntryTypeFill || entry.Status != lpledgerentry.StatusConfirmed {
		t.Errorf("fill entry = %s/%s, want fill/confirmed", entry.EntryType, entry.Status)
	}
	if final.BridgeTxSui == "" {
		t.Error("no USDC delivery digest recorded")
	}

	// NO bridge ever happened.
	if n := client.RouteAEvent.Query().
		Where(routeaevent.StepEQ(routeaevent.StepBridgeSubmit)).
		CountX(ctx); n != 0 {
		t.Errorf("bridge_submit events = %d, want 0 (Route B never bridges)", n)
	}
	finalPO := client.PaymentOrder.GetX(ctx, po.ID)
	if string(finalPO.Status) != "settled" {
		t.Errorf("payment_order.status = %s, want settled", finalPO.Status)
	}
}
