package services

// route_a_lifecycle_test.go — end-to-end test of the Route-A pipeline
// exactly as production runs it: the REAL dispatcher Tick() drives a
// USDC order from `pending` all the way to `settled`, through the real
// CCTP burn PTB builder, canonical BCS, claim-first transitions, Iris
// attestation, Base mint, Gateway dispatch, and settlement polling —
// with every external (Sui RPC, Circle Iris, Base RPC, Paycrest) faked
// at the HTTP layer and an in-memory SQLite DB standing in for prod.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	suisigner "github.com/block-vision/sui-go-sdk/signer"
	suisdk "github.com/block-vision/sui-go-sdk/sui"
	ethabi "github.com/ethereum/go-ethereum/accounts/abi"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	_ "github.com/mattn/go-sqlite3"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent/enttest"
	"github.com/usezoracle/rails-sui/ent/routeaevent"
	"github.com/usezoracle/rails-sui/ent/routeaorder"
	"github.com/usezoracle/rails-sui/services/cctp"
	"github.com/usezoracle/rails-sui/services/evm"
	"github.com/usezoracle/rails-sui/services/lifi"
	"github.com/usezoracle/rails-sui/services/settlement"
	"github.com/usezoracle/rails-sui/storage"
)

const (
	suiUSDC      = "0xdba34672e30cb065b1f93e3ab55318768fd6fef66c15942c9f7cb846e2f900e7::usdc::USDC"
	burnDigest   = "CLkXnati9n3FstZRwxB1k9S63rkajHNLPW7rVVH1hV3T" // valid 32-byte base58
	coinDigest   = "FwA4F8o5bBJZLhNUMpdqojAcv88bVdit8yiFXbgbAmrh"
	orderAmount  = "1.081665" // USDC the card debit collected
	amountSubu   = 1_081_665
	cctpNonce    = 777
	gatewayAddr  = "0x00000000000000000000000000000000000000aa"
	baseUSDCAddr = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	// Throwaway test key — never funded anywhere.
	evmSignerKey = "59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
)

// rpcReq is the generic JSON-RPC request envelope both fakes decode.
type rpcReq struct {
	ID     any               `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func rpcOK(w http.ResponseWriter, id any, result any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// fakeSuiServer answers exactly the JSON-RPC calls the burn path makes.
func fakeSuiServer(t *testing.T, aggregator string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		switch req.Method {
		case "suix_getBalance":
			rpcOK(w, req.ID, map[string]any{
				"coinType": suiUSDC, "coinObjectCount": 1,
				"totalBalance": "5000000", "lockedBalance": map[string]any{},
			})
		case "suix_getCoins":
			var owner, coinType string
			_ = json.Unmarshal(req.Params[0], &owner)
			if len(req.Params) > 1 {
				_ = json.Unmarshal(req.Params[1], &coinType)
			}
			balance := "5000000"
			id := "0x" + strings.Repeat("11", 32)
			if strings.Contains(coinType, "sui::SUI") {
				balance = "1000000000"
				id = "0x" + strings.Repeat("22", 32)
			}
			rpcOK(w, req.ID, map[string]any{
				"data": []map[string]any{{
					"coinType": coinType, "coinObjectId": id, "version": "3",
					"digest": coinDigest, "balance": balance, "previousTransaction": coinDigest,
				}},
				"hasNextPage": false,
			})
		case "sui_getObject":
			var objID string
			_ = json.Unmarshal(req.Params[0], &objID)
			rpcOK(w, req.ID, map[string]any{
				"data": map[string]any{
					"objectId": objID, "version": "5", "digest": coinDigest,
					"owner": map[string]any{"Shared": map[string]any{"initial_shared_version": 42}},
				},
			})
		case "suix_getReferenceGasPrice":
			rpcOK(w, req.ID, "1000")
		case "sui_executeTransactionBlock":
			rpcOK(w, req.ID, map[string]any{
				"digest":  burnDigest,
				"effects": map[string]any{"status": map[string]any{"status": "success"}},
			})
		default:
			t.Errorf("fake sui: unexpected method %s (aggregator=%s)", req.Method, aggregator)
			http.Error(w, "unexpected "+req.Method, 400)
		}
	}))
}

// buildV1BurnMessage mirrors Circle's CCTP v1 wire format (the same
// layout services/cctp/message.go parses).
func buildV1BurnMessage(recipient ethcommon.Address, amount uint64) []byte {
	const headerLen = 116
	raw := make([]byte, headerLen+132)
	binary.BigEndian.PutUint32(raw[4:8], 8)  // source: Sui
	binary.BigEndian.PutUint32(raw[8:12], 6) // dest: Base
	binary.BigEndian.PutUint64(raw[12:20], cctpNonce)
	copy(raw[headerLen+36+12:headerLen+68], recipient.Bytes())
	new(big.Int).SetUint64(amount).FillBytes(raw[headerLen+68 : headerLen+100])
	return raw
}

// fakeEVMServer answers the Base-side calls: nonce/gas for transactors,
// any receipt as success WITH an OrderCreated log (only createOrder's
// receipt is parsed for it), and eth_call by selector.
func fakeEVMServer(t *testing.T, signerAddr ethcommon.Address, orderID [32]byte) *httptest.Server {
	t.Helper()
	parsedGw, err := ethabi.JSON(strings.NewReader(evm.GatewayABI))
	if err != nil {
		t.Fatalf("parse gateway abi: %v", err)
	}
	ev := parsedGw.Events["OrderCreated"]
	logData, err := ev.Inputs.NonIndexed().Pack(
		big.NewInt(0),            // protocolFee
		orderID,                  // orderId
		big.NewInt(138_675),      // rate
		"msghash",                // messageHash
	)
	if err != nil {
		t.Fatalf("pack OrderCreated: %v", err)
	}

	allowanceSel := hex.EncodeToString(crypto.Keccak256([]byte("allowance(address,address)"))[:4])
	balanceSel := hex.EncodeToString(crypto.Keccak256([]byte("balanceOf(address)"))[:4])

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		switch req.Method {
		case "eth_getTransactionCount":
			rpcOK(w, req.ID, "0x1")
		case "eth_gasPrice":
			rpcOK(w, req.ID, "0x3b9aca00")
		case "eth_sendRawTransaction":
			rpcOK(w, req.ID, "0x"+strings.Repeat("ab", 32))
		case "eth_getTransactionReceipt":
			var h string
			_ = json.Unmarshal(req.Params[0], &h)
			rpcOK(w, req.ID, map[string]any{
				"status": "0x1", "transactionHash": h, "transactionIndex": "0x0",
				"blockHash": "0x" + strings.Repeat("cd", 32), "blockNumber": "0x10",
				"gasUsed": "0x22dd0", "cumulativeGasUsed": "0x22dd0",
				"contractAddress": nil, "type": "0x0", "effectiveGasPrice": "0x3b9aca00",
				"logsBloom": "0x" + strings.Repeat("00", 256),
				"logs": []map[string]any{{
					"address": gatewayAddr,
					"topics": []string{
						ev.ID.Hex(),
						"0x000000000000000000000000" + strings.TrimPrefix(strings.ToLower(signerAddr.Hex()), "0x"),
						"0x000000000000000000000000" + strings.TrimPrefix(strings.ToLower(baseUSDCAddr), "0x"),
						"0x" + strings.Repeat("00", 32),
					},
					"data":             "0x" + hex.EncodeToString(logData),
					"blockNumber":      "0x10",
					"transactionHash":  h,
					"transactionIndex": "0x0",
					"blockHash":        "0x" + strings.Repeat("cd", 32),
					"logIndex":         "0x0",
					"removed":          false,
				}},
			})
		case "eth_call":
			var call struct {
				Data string `json:"data"`
			}
			_ = json.Unmarshal(req.Params[0], &call)
			sel := strings.TrimPrefix(call.Data, "0x")
			if len(sel) >= 8 {
				sel = sel[:8]
			}
			switch sel {
			case allowanceSel, balanceSel:
				rpcOK(w, req.ID, "0x"+strings.Repeat("0f", 32)) // plenty
			default:
				rpcOK(w, req.ID, "0x"+strings.Repeat("00", 32)) // usedNonces: not used
			}
		default:
			t.Errorf("fake evm: unexpected method %s", req.Method)
			http.Error(w, "unexpected "+req.Method, 400)
		}
	}))
}

func TestRouteALifecycleCCTPToSettled(t *testing.T) {
	ctx := context.Background()

	// ---- In-memory DB standing in for prod ------------------------------
	client := enttest.Open(t, "sqlite3", "file:route_a_lifecycle?mode=memory&cache=shared&_fk=1")
	defer client.Close()
	prev := storage.Client
	storage.Client = client
	defer func() { storage.Client = prev }()

	// ---- Keys / addresses ------------------------------------------------
	suiSeed := make([]byte, 32)
	for i := range suiSeed {
		suiSeed[i] = byte(i + 1)
	}
	signer := suisigner.NewSigner(suiSeed)
	evmKey, _ := crypto.HexToECDSA(evmSignerKey)
	aggregatorEVM := crypto.PubkeyToAddress(evmKey.PublicKey)
	var gatewayOrderID [32]byte
	copy(gatewayOrderID[:], []byte("lifecycle-test-order-id-000000ok"))

	// ---- Fake externals ---------------------------------------------------
	suiSrv := fakeSuiServer(t, signer.Address)
	defer suiSrv.Close()
	evmSrv := fakeEVMServer(t, aggregatorEVM, gatewayOrderID)
	defer evmSrv.Close()

	msg := buildV1BurnMessage(aggregatorEVM, amountSubu)
	irisSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/messages/8/"+burnDigest) {
			t.Errorf("iris: unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]any{{
				"message":     "0x" + hex.EncodeToString(msg),
				"attestation": "0x" + strings.Repeat("dd", 65),
				"eventNonce":  fmt.Sprint(cctpNonce),
			}},
		})
	}))
	defer irisSrv.Close()

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, _ := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	gatewayIDHex := strings.ToLower(ethcommon.Hash(gatewayOrderID).Hex())

	setSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/pubkey":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": pubPEM})
		case strings.HasPrefix(r.URL.Path, "/v2/rates/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data": map[string]any{"sell": map[string]any{
					"rate": "1386.75", "providerIds": []string{"p1"}, "orderType": "fiat", "refundTimeoutMinutes": 5,
				}},
			})
		case strings.HasPrefix(r.URL.Path, "/orders/8453/"+gatewayIDHex):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data":   map[string]any{"orderId": gatewayIDHex, "chainId": 8453, "status": "settled"},
			})
		default:
			t.Errorf("settlement: unexpected path %s", r.URL.Path)
			http.Error(w, "unexpected", 400)
		}
	}))
	defer setSrv.Close()

	// ---- Fixtures: network → token → order → recipient → route-a ---------
	network := client.Network.Create().
		SetChainID(101).SetIdentifier("sui-mainnet").
		SetRPCEndpoint(suiSrv.URL).SetIsTestnet(false).
		SetFee(decimal.NewFromFloat(0.01)).
		SaveX(ctx)
	token := client.Token.Create().
		SetSymbol("USDC").SetContractAddress(suiUSDC).SetDecimals(6).
		SetIsEnabled(true).SetNetwork(network).
		SaveX(ctx)
	amount := decimal.RequireFromString(orderAmount)
	po := client.PaymentOrder.Create().
		SetAmount(amount).
		SetAmountPaid(decimal.Zero).SetAmountReturned(decimal.Zero).
		SetPercentSettled(decimal.Zero).SetSenderFee(decimal.Zero).
		SetNetworkFee(decimal.Zero).SetProtocolFee(decimal.Zero).
		SetRate(decimal.RequireFromString("1386.75")).
		SetReceiveAddressText("0xstub").SetFeePercent(decimal.Zero).
		SetToken(token).
		SaveX(ctx)
	client.PaymentOrderRecipient.Create().
		SetInstitution("TESTBANK").SetAccountIdentifier("0123456789").
		SetAccountName("Lifecycle Merchant").SetPaymentOrder(po).
		SaveX(ctx)
	ra := client.RouteAOrder.Create().
		SetMode(routeaorder.ModeLp).
		SetBridgeStatus(routeaorder.BridgeStatusPending).
		SetBridgeProvider("cctp").
		SetPaymentOrder(po).
		SaveX(ctx)

	// ---- The dispatcher, wired like production ----------------------------
	conf := &config.OrderConfiguration{
		SuiRpcURL:                 suiSrv.URL,
		CCTPFallbackEnabled:       true,
		BaseChainID:               8453,
		BaseRpcURL:                evmSrv.URL,
		BaseAggregatorAddress:     aggregatorEVM.Hex(),
		BaseGatewayContract:       gatewayAddr,
		BaseUSDCContract:          baseUSDCAddr,
		BaseUSDCDecimals:          6,
		BaseSenderFeeBPS:          50,
		BaseSignerKey:             evmSignerKey,
		SettlementAPIURL:          setSrv.URL,
		SettlementSenderAPIKeyID:  "lifecycle-test",
	}
	suiAPI := suisdk.NewSuiClient(suiSrv.URL)
	suiClient, _ := suiAPI.(*suisdk.Client)
	evmClient, err := evm.NewClient(ctx, evm.ChainConfig{
		Name: "test", ChainID: 8453, RPCURL: evmSrv.URL,
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
		instanceID:      "lifecycle-test",
		burstWake:       make(chan struct{}, 1),
		lifiPollAt:      map[int]time.Time{},
	}

	// ---- Drive ticks until settled (prod needs ~1; allow a few) ----------
	for i := 0; i < 5; i++ {
		if err := d.Tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		cur := client.RouteAOrder.GetX(ctx, ra.ID)
		if cur.BridgeStatus == routeaorder.BridgeStatusSettled {
			break
		}
	}

	// ---- Assert the full journey ------------------------------------------
	final := client.RouteAOrder.GetX(ctx, ra.ID)
	if final.BridgeStatus != routeaorder.BridgeStatusSettled {
		t.Fatalf("bridge_status = %s, want settled (failure_reason=%q)", final.BridgeStatus, final.FailureReason)
	}
	if final.BridgeProvider != "cctp" {
		t.Errorf("bridge_provider = %s, want cctp", final.BridgeProvider)
	}
	if final.BridgeTxSui != burnDigest {
		t.Errorf("bridge_tx_sui = %s, want %s", final.BridgeTxSui, burnDigest)
	}
	if final.BridgedAmount == nil || !final.BridgedAmount.Equal(amount) {
		t.Errorf("bridged_amount = %v, want %s (CCTP is 1:1)", final.BridgedAmount, amount)
	}
	if final.GatewayOrderID != gatewayIDHex {
		t.Errorf("gateway_order_id = %s, want %s", final.GatewayOrderID, gatewayIDHex)
	}
	finalPO := client.PaymentOrder.GetX(ctx, po.ID)
	if string(finalPO.Status) != "settled" {
		t.Errorf("payment_order.status = %s, want settled", finalPO.Status)
	}

	// Every stage must have left its audit event, in order.
	for _, step := range []routeaevent.Step{
		routeaevent.StepBridgeSubmit,
		routeaevent.StepBridgeDone,
		routeaevent.StepEvmCreateOrder,
	} {
		ok, err := client.RouteAEvent.Query().
			Where(
				routeaevent.StepEQ(step),
				routeaevent.StatusEQ(routeaevent.StatusSucceeded),
				routeaevent.HasRouteAOrderWith(routeaorder.IDEQ(ra.ID)),
			).Exist(ctx)
		if err != nil || !ok {
			t.Errorf("missing succeeded audit event for step %s (err=%v)", step, err)
		}
	}
}
