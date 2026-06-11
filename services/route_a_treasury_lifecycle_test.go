package services

// route_a_treasury_lifecycle_test.go — Route C end-to-end: a
// mode=treasury order driven by the real Tick() from pending →
// CCTP bridge → INSTANT float payout (settled) → float reload
// submitted and confirmed, against the same faked externals as the
// Route A lifecycle test plus an in-memory BaaS rail.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	suisigner "github.com/block-vision/sui-go-sdk/signer"
	suisdk "github.com/block-vision/sui-go-sdk/sui"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	_ "github.com/mattn/go-sqlite3"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent/enttest"
	"github.com/usezoracle/rails-sui/ent/routeaevent"
	"github.com/usezoracle/rails-sui/ent/routeaorder"
	"github.com/usezoracle/rails-sui/services/baas"
	"github.com/usezoracle/rails-sui/services/cctp"
	"github.com/usezoracle/rails-sui/services/evm"
	"github.com/usezoracle/rails-sui/services/lifi"
	"github.com/usezoracle/rails-sui/services/settlement"
	"github.com/usezoracle/rails-sui/storage"
)

// fakeBaas is an in-memory baas.Provider: a big NGN float, transfers
// that succeed on submit, and a call log for assertions.
type fakeBaas struct {
	mu        sync.Mutex
	transfers []baas.TransferRequest
	balance   decimal.Decimal
}

func (f *fakeBaas) Name() string { return "fake" }
func (f *fakeBaas) ListBanks(context.Context) ([]baas.Bank, error) {
	return []baas.Bank{{Name: "OPay", BankCode: "OPAYNGPC", Active: true}}, nil
}
func (f *fakeBaas) ListAccounts(context.Context, bool) ([]baas.Account, error) {
	return []baas.Account{{ID: "float", AccountNumber: "9000000001", Currency: "NGN", Balance: f.balance}}, nil
}
func (f *fakeBaas) GetAccount(context.Context, string) (*baas.Account, error) {
	return &baas.Account{ID: "float", Currency: "NGN", Balance: f.balance}, nil
}
func (f *fakeBaas) NameEnquiry(_ context.Context, bankCode, acct string) (*baas.NameEnquiry, error) {
	return &baas.NameEnquiry{BankCode: bankCode, AccountNumber: acct, AccountName: "MERCHANT"}, nil
}
func (f *fakeBaas) Transfer(_ context.Context, req baas.TransferRequest) (*baas.Transfer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transfers = append(f.transfers, req)
	f.balance = f.balance.Sub(req.Amount)
	return &baas.Transfer{
		Reference: req.PaymentReference, PaymentReference: req.PaymentReference,
		Amount: req.Amount, Status: baas.TransferSuccess, RawStatus: "success",
	}, nil
}
func (f *fakeBaas) TransferStatus(_ context.Context, ref string) (*baas.Transfer, error) {
	return &baas.Transfer{Reference: ref, PaymentReference: ref, Status: baas.TransferSuccess, RawStatus: "success"}, nil
}
func (f *fakeBaas) InitiateIdentity(context.Context, baas.IdentityInit) (*baas.IdentityResult, error) {
	return &baas.IdentityResult{Status: "verified"}, nil
}
func (f *fakeBaas) ValidateIdentity(context.Context, string, string, string) (*baas.IdentityResult, error) {
	return &baas.IdentityResult{Status: "verified"}, nil
}
func (f *fakeBaas) CreateSubAccount(context.Context, baas.CreateSubAccountRequest) (*baas.Account, error) {
	return &baas.Account{}, nil
}
func (f *fakeBaas) VerifyWebhook([]byte, string) bool          { return true }
func (f *fakeBaas) WebhookConfigured() bool                    { return true }
func (f *fakeBaas) ParseWebhook([]byte) (*baas.WebhookEvent, error) {
	return &baas.WebhookEvent{}, nil
}

func TestRouteCLifecycleTreasuryToSettled(t *testing.T) {
	ctx := context.Background()

	client := enttest.Open(t, "sqlite3", "file:route_c_lifecycle?mode=memory&cache=shared&_fk=1")
	defer client.Close()
	prev := storage.Client
	storage.Client = client
	defer func() { storage.Client = prev }()

	rail := &fakeBaas{balance: decimal.NewFromInt(1_000_000)}
	baas.SetDefault(rail)
	defer baas.SetDefault(nil)
	// The dispatcher will also get rail injected as treasuryRail below
	// — Route C must never touch a real Korapay config in tests.

	suiSeed := make([]byte, 32)
	for i := range suiSeed {
		suiSeed[i] = byte(i + 7)
	}
	signer := suisigner.NewSigner(suiSeed)
	evmKey, _ := crypto.HexToECDSA(evmSignerKey)
	aggregatorEVM := crypto.PubkeyToAddress(evmKey.PublicKey)
	var gatewayOrderID [32]byte
	copy(gatewayOrderID[:], []byte("route-c-reload-order-id-000000ok"))
	gatewayIDHex := strings.ToLower(ethcommon.Hash(gatewayOrderID).Hex())

	suiSrv := fakeSuiServer(t, signer.Address)
	defer suiSrv.Close()
	evmSrv := fakeEVMServer(t, aggregatorEVM, gatewayOrderID)
	defer evmSrv.Close()

	msg := buildV1BurnMessage(aggregatorEVM, amountSubu)
	irisSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]any{{
				"message":     "0x" + hex.EncodeToString(msg),
				"attestation": "0x" + strings.Repeat("dd", 65),
				"eventNonce":  fmt.Sprint(cctpNonce),
			}},
		})
	}))
	defer irisSrv.Close()

	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	pubDER, _ := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	setSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/pubkey":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": pubPEM})
		case strings.HasPrefix(r.URL.Path, "/v2/rates/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data":   map[string]any{"sell": map[string]any{"rate": "1386.75", "providerIds": []string{"p1"}, "orderType": "fiat", "refundTimeoutMinutes": 5}},
			})
		case strings.HasPrefix(r.URL.Path, "/orders/8453/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data":   map[string]any{"orderId": gatewayIDHex, "chainId": 8453, "status": "settled"},
			})
		default:
			t.Errorf("settlement: unexpected path %s", r.URL.Path)
		}
	}))
	defer setSrv.Close()

	// Fixtures: a treasury-mode card-tap order.
	network := client.Network.Create().
		SetChainID(101).SetIdentifier("sui-mainnet-c").
		SetRPCEndpoint(suiSrv.URL).SetIsTestnet(false).
		SetFee(decimal.NewFromFloat(0.01)).SaveX(ctx)
	token := client.Token.Create().
		SetSymbol("USDC").SetContractAddress(suiUSDC).SetDecimals(6).
		SetIsEnabled(true).SetNetwork(network).SaveX(ctx)
	amount := decimal.RequireFromString(orderAmount)
	po := client.PaymentOrder.Create().
		SetAmount(amount).
		SetAmountPaid(decimal.Zero).SetAmountReturned(decimal.Zero).
		SetPercentSettled(decimal.Zero).SetSenderFee(decimal.Zero).
		SetNetworkFee(decimal.Zero).SetProtocolFee(decimal.Zero).
		SetRate(decimal.RequireFromString("1386.75")).
		SetReceiveAddressText("0xstub").SetFeePercent(decimal.Zero).
		SetToken(token).SaveX(ctx)
	client.PaymentOrderRecipient.Create().
		SetInstitution("OPAYNGPC").SetAccountIdentifier("9123456789").
		SetAccountName("Route C Merchant").SetPaymentOrder(po).SaveX(ctx)
	ra := client.RouteAOrder.Create().
		SetMode(routeaorder.ModeTreasury).
		SetBridgeStatus(routeaorder.BridgeStatusPending).
		SetBridgeProvider("cctp").
		SetPaymentOrder(po).SaveX(ctx)

	conf := &config.OrderConfiguration{
		SuiRpcURL:                  suiSrv.URL,
		CCTPFallbackEnabled:        true,
		BaseChainID:                8453,
		BaseRpcURL:                 evmSrv.URL,
		BaseAggregatorAddress:      aggregatorEVM.Hex(),
		BaseGatewayContract:        gatewayAddr,
		BaseUSDCContract:           baseUSDCAddr,
		BaseUSDCDecimals:           6,
		BaseSenderFeeBPS:           50,
		BaseSignerKey:              evmSignerKey,
		SettlementAPIURL:           setSrv.URL,
		SettlementSenderAPIKeyID:   "route-c-test",
		TreasuryFloatInstitution:   "TESTBANK",
		TreasuryFloatAccountNumber: "9000000001",
		TreasuryFloatAccountName:   "Tapp Float",
	}
	suiAPI := suisdk.NewSuiClient(suiSrv.URL)
	suiClient, _ := suiAPI.(*suisdk.Client)
	evmClient, err := evm.NewClient(ctx, evm.ChainConfig{
		Name: "test-c", ChainID: 8453, RPCURL: evmSrv.URL,
		GatewayAddr: ethcommon.HexToAddress(gatewayAddr),
		USDCAddr:    ethcommon.HexToAddress(baseUSDCAddr),
		USDCDecimals: 6, SignerHex: evmSignerKey,
	})
	if err != nil {
		t.Fatalf("evm client: %v", err)
	}
	net, _ := cctp.ForBaseChainID(8453)
	net = net.WithIrisURL(irisSrv.URL)
	d := &RouteADispatcher{
		conf:            conf,
		suiClient:       suiClient,
		signer:          signer,
		lifi:            lifi.New(""),
		evm:             evmClient,
		settlement:      settlement.New(setSrv.URL, time.Minute),
		quoteFailCounts: map[int]int{},
		cctpNet:         net,
		cctpNetOK:       true,
		cctpIris:        cctp.NewIris(irisSrv.URL),
		instanceID:      "route-c-test",
		burstWake:       make(chan struct{}, 1),
		lifiPollAt:      map[int]time.Time{},
		treasuryRail:    rail,
	}

	for i := 0; i < 5; i++ {
		if err := d.Tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		cur := client.RouteAOrder.GetX(ctx, ra.ID)
		if cur.BridgeStatus == routeaorder.BridgeStatusSettled && cur.SettlementStatus == "settled" {
			break
		}
	}

	final := client.RouteAOrder.GetX(ctx, ra.ID)

	// Merchant paid from float, instantly.
	if final.BridgeStatus != routeaorder.BridgeStatusSettled {
		t.Fatalf("bridge_status = %s, want settled (reason=%q)", final.BridgeStatus, final.FailureReason)
	}
	if !strings.HasPrefix(final.TreasuryPayoutRef, treasuryRefPrefix) {
		t.Errorf("treasury_payout_ref = %q, want %s*", final.TreasuryPayoutRef, treasuryRefPrefix)
	}
	rail.mu.Lock()
	if len(rail.transfers) != 1 {
		t.Fatalf("BaaS transfers = %d, want exactly 1", len(rail.transfers))
	}
	tr := rail.transfers[0]
	rail.mu.Unlock()
	wantNGN := amount.Mul(decimal.RequireFromString("1386.75")).Round(2)
	if !tr.Amount.Equal(wantNGN) {
		t.Errorf("payout = ₦%s, want ₦%s (exact merchant entitlement)", tr.Amount, wantNGN)
	}
	if tr.BeneficiaryBankCode != "OPAYNGPC" || tr.BeneficiaryAccount != "9123456789" {
		t.Errorf("payout beneficiary = %s/%s, want the merchant", tr.BeneficiaryBankCode, tr.BeneficiaryAccount)
	}

	// Reload aimed at OUR float account and confirmed settled.
	if final.GatewayOrderID != gatewayIDHex {
		t.Errorf("reload gateway_order_id = %q, want %s", final.GatewayOrderID, gatewayIDHex)
	}
	if final.SettlementStatus != "settled" {
		t.Errorf("reload settlement_status = %q, want settled", final.SettlementStatus)
	}

	finalPO := client.PaymentOrder.GetX(ctx, po.ID)
	if string(finalPO.Status) != "settled" {
		t.Errorf("payment_order.status = %s, want settled", finalPO.Status)
	}

	// Audit trail.
	for _, step := range []routeaevent.Step{
		routeaevent.StepBridgeSubmit,
		routeaevent.StepTreasuryPayout,
		routeaevent.StepFloatReload,
	} {
		ok, qerr := client.RouteAEvent.Query().
			Where(
				routeaevent.StepEQ(step),
				routeaevent.StatusEQ(routeaevent.StatusSucceeded),
				routeaevent.HasRouteAOrderWith(routeaorder.IDEQ(ra.ID)),
			).Exist(ctx)
		if qerr != nil || !ok {
			t.Errorf("missing succeeded event for %s (err=%v)", step, qerr)
		}
	}
}
