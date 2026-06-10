package cctp

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	suimodels "github.com/block-vision/sui-go-sdk/models"
	ethcommon "github.com/ethereum/go-ethereum/common"
)

// buildBurnMessage assembles a synthetic v1 burn message the way the
// Sui MessageTransmitter emits it, so the parser is tested against the
// exact wire layout.
func buildBurnMessage(srcDomain, dstDomain uint32, nonce uint64, recipient ethcommon.Address, amount *big.Int) []byte {
	raw := make([]byte, burnMessageLen)
	binary.BigEndian.PutUint32(raw[0:4], 0) // version
	binary.BigEndian.PutUint32(raw[4:8], srcDomain)
	binary.BigEndian.PutUint32(raw[8:12], dstDomain)
	binary.BigEndian.PutUint64(raw[12:20], nonce)
	// header sender/recipient/destinationCaller left zero — unused by us.
	copy(raw[headerLen+36+12:headerLen+68], recipient.Bytes()) // mintRecipient, left-padded
	amount.FillBytes(raw[headerLen+68 : headerLen+100])
	return raw
}

func TestParseBurnMessage(t *testing.T) {
	recipient := ethcommon.HexToAddress("0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B")
	amount := big.NewInt(123_456_789) // 123.456789 USDC
	raw := buildBurnMessage(8, 6, 42, recipient, amount)

	m, err := ParseBurnMessage(raw)
	if err != nil {
		t.Fatalf("ParseBurnMessage: %v", err)
	}
	if m.SourceDomain != 8 || m.DestinationDomain != 6 {
		t.Errorf("domains = %d→%d, want 8→6", m.SourceDomain, m.DestinationDomain)
	}
	if m.Nonce != 42 {
		t.Errorf("nonce = %d, want 42", m.Nonce)
	}
	if m.Amount.Cmp(amount) != 0 {
		t.Errorf("amount = %s, want %s", m.Amount, amount)
	}
	if got := ethcommon.Address(m.MintRecipientEVM()); got != recipient {
		t.Errorf("mintRecipient = %s, want %s", got.Hex(), recipient.Hex())
	}
}

func TestParseBurnMessageRejectsBadInput(t *testing.T) {
	if _, err := ParseBurnMessage(make([]byte, 100)); err == nil {
		t.Error("short message: want error")
	}
	raw := buildBurnMessage(8, 6, 1, ethcommon.Address{}, big.NewInt(1))
	binary.BigEndian.PutUint32(raw[0:4], 1) // v2 header version
	if _, err := ParseBurnMessage(raw); err == nil {
		t.Error("wrong version: want error")
	}
}

func TestIrisMessageFor(t *testing.T) {
	recipient := ethcommon.HexToAddress("0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B")
	msgHex := "0x" + hex.EncodeToString(buildBurnMessage(8, 6, 7, recipient, big.NewInt(5_000_000)))

	cases := []struct {
		name    string
		status  int
		body    string
		wantErr error
		wantAtt bool
	}{
		{
			name:   "complete",
			status: 200,
			body:   fmt.Sprintf(`{"messages":[{"attestation":"0xdeadbeef","message":"%s","eventNonce":"7"}]}`, msgHex),
			wantAtt: true,
		},
		{
			name:    "pending",
			status:  200,
			body:    fmt.Sprintf(`{"messages":[{"attestation":"PENDING","message":"%s","eventNonce":"7"}]}`, msgHex),
			wantErr: ErrAttestationPending,
		},
		{
			name:    "not indexed",
			status:  404,
			body:    `{"error":"Transaction hash not found"}`,
			wantErr: ErrNotIndexed,
		},
		{
			name:    "empty list",
			status:  200,
			body:    `{"messages":[]}`,
			wantErr: ErrNotIndexed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/messages/8/SOMEDIGEST" {
					t.Errorf("path = %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			got, err := NewIris(srv.URL).MessageFor(context.Background(), 8, "SOMEDIGEST")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("MessageFor: %v", err)
			}
			if tc.wantAtt && len(got.Attestation) == 0 {
				t.Error("attestation empty, want bytes")
			}
			if _, perr := ParseBurnMessage(got.Message); perr != nil {
				t.Errorf("returned message does not parse: %v", perr)
			}
		})
	}
}

func TestSelectCoins(t *testing.T) {
	coins := []suimodels.CoinData{
		{CoinObjectId: "0x1", Balance: "3000000"},
		{CoinObjectId: "0x2", Balance: "0"},
		{CoinObjectId: "0x3", Balance: "4000000"},
		{CoinObjectId: "0x4", Balance: "9000000"},
	}

	sel, err := selectCoins(coins, big.NewInt(5_000_000))
	if err != nil {
		t.Fatalf("selectCoins: %v", err)
	}
	// Takes 0x1 (3) then 0x3 (4) — zero-balance 0x2 skipped, covered at 7.
	if len(sel) != 2 || sel[0].CoinObjectId != "0x1" || sel[1].CoinObjectId != "0x3" {
		t.Errorf("selected %+v", sel)
	}

	if _, err := selectCoins(coins, big.NewInt(17_000_000)); err == nil {
		t.Error("insufficient balance: want error")
	}
}

func TestNetworkSelection(t *testing.T) {
	for chainID, wantIris := range map[int64]string{
		8453:  "https://iris-api.circle.com",
		84532: "https://iris-api-sandbox.circle.com",
	} {
		net, ok := ForBaseChainID(chainID)
		if !ok {
			t.Fatalf("ForBaseChainID(%d): not found", chainID)
		}
		if net.IrisBaseURL != wantIris {
			t.Errorf("chain %d iris = %s", chainID, net.IrisBaseURL)
		}
		if net.SuiDomain != 8 || net.BaseDomain != 6 {
			t.Errorf("chain %d domains = %d/%d", chainID, net.SuiDomain, net.BaseDomain)
		}
	}
	if _, ok := ForBaseChainID(1); ok {
		t.Error("mainnet ethereum should not resolve")
	}
	if got := mainnet.WithIrisURL("http://localhost:1").IrisBaseURL; got != "http://localhost:1" {
		t.Errorf("WithIrisURL = %s", got)
	}
}

func TestEvmAddressToSuiAddress(t *testing.T) {
	a := ethcommon.HexToAddress("0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B")
	got := evmAddressToSuiAddress(a)
	want := "0x000000000000000000000000ab5801a7d398351b8be11c439e05c5b3259aec9b"
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestStructTypeTag(t *testing.T) {
	tag, err := structTypeTag(mainnet.SuiUSDCCoinType)
	if err != nil {
		t.Fatalf("structTypeTag: %v", err)
	}
	if tag.Struct == nil || tag.Struct.Module != "usdc" || tag.Struct.Name != "USDC" {
		t.Errorf("tag = %+v", tag.Struct)
	}
	if _, err := structTypeTag("just-a-string"); err == nil {
		t.Error("malformed coin type: want error")
	}
}

func TestInitialSharedVersion(t *testing.T) {
	v, err := initialSharedVersion(map[string]any{
		"Shared": map[string]any{"initial_shared_version": float64(1234)},
	})
	if err != nil || v != 1234 {
		t.Errorf("got %d, %v", v, err)
	}
	if _, err := initialSharedVersion("Immutable"); err == nil {
		t.Error("immutable owner: want error")
	}
}
