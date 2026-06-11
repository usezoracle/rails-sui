package korapay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/services/baas"
)

const testSecret = "sk_test_abc123"

// fakeKorapay serves the endpoints the adapter touches, with a
// duplicate-reference rejection on repeat disbursements — the exact
// behaviour the adapter must normalise into idempotency.
func fakeKorapay(t *testing.T) (*httptest.Server, map[string]int) {
	t.Helper()
	calls := map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/misc/banks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":true,"message":"ok","data":[
			{"name":"First Bank","slug":"firstbank","code":"011","nibss_bank_code":"000016","country":"NG"},
			{"name":"OPay","slug":"opay","code":"100004","nibss_bank_code":"100004","country":"NG"}]}`))
	})
	mux.HandleFunc("/api/v1/misc/banks/resolve", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":true,"message":"ok","data":
			{"bank_name":"First Bank","bank_code":"011","account_number":"0123456789","account_name":"LIFECYCLE MERCHANT"}}`))
	})
	mux.HandleFunc("/api/v1/balances", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":true,"message":"ok","data":
			{"NGN":{"available_balance":250000.50,"pending_balance":1000}}}`))
	})
	mux.HandleFunc("/api/v1/transactions/disburse", func(w http.ResponseWriter, r *http.Request) {
		calls["disburse"]++
		if r.Header.Get("Authorization") != "Bearer "+testSecret {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"status":false,"message":"Invalid authorization key"}`))
			return
		}
		if calls["disburse"] > 1 {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"status":false,"message":"Duplicate Transaction Reference. Please use a unique reference"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":true,"message":"queued","data":
			{"reference":"order-42-payout","status":"processing","amount":1500,"fee":2.5,"currency":"NGN","narration":"tap"}}`))
	})
	mux.HandleFunc("/api/v1/transactions/order-42-payout", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":true,"message":"ok","data":
			{"reference":"order-42-payout","status":"success","amount":1500,"fee":2.5,"currency":"NGN","message":"Transfer successful"}}`))
	})
	mux.HandleFunc("/api/v1/virtual-bank-account", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if kyc, _ := body["kyc"].(map[string]any); kyc == nil || kyc["bvn"] == "" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"status":false,"message":"kyc.bvn is required"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":true,"message":"ok","data":
			{"account_number":"9977581763","account_name":"lp-one","bank_code":"035","bank_name":"Wema Bank",
			 "account_reference":"lp-profile-1","unique_id":"KPY-VA-1","account_status":"active","currency":"NGN"}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, calls
}

func newTestAdapter(t *testing.T) (*Adapter, map[string]int) {
	srv, calls := fakeKorapay(t)
	return NewAdapter(New(testSecret, "pk_test_x", srv.URL, "ops@usetapp.xyz", "035")), calls
}

func TestBanksResolveBalance(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()

	banks, err := a.ListBanks(ctx)
	if err != nil || len(banks) != 2 || banks[0].BankCode != "011" {
		t.Fatalf("ListBanks = %v, %v", banks, err)
	}

	ne, err := a.NameEnquiry(ctx, "011", "0123456789")
	if err != nil || ne.AccountName != "LIFECYCLE MERCHANT" {
		t.Fatalf("NameEnquiry = %v, %v", ne, err)
	}

	acct, err := a.GetAccount(ctx, "NGN")
	if err != nil || !acct.Balance.Equal(decimal.RequireFromString("250000.5")) {
		t.Fatalf("GetAccount = %v, %v", acct, err)
	}
	subs, err := a.ListAccounts(ctx, true)
	if err != nil || len(subs) != 0 {
		t.Fatalf("sub accounts should be empty on a pooled rail: %v, %v", subs, err)
	}
}

// TestTransferIdempotentOnDuplicate pins the retry-safety contract:
// the second Transfer with the same PaymentReference must not error —
// it converts Korapay's duplicate rejection into a status fetch.
func TestTransferIdempotentOnDuplicate(t *testing.T) {
	a, calls := newTestAdapter(t)
	ctx := context.Background()
	req := baas.TransferRequest{
		BeneficiaryBankCode: "011",
		BeneficiaryAccount:  "0123456789",
		Amount:              decimal.RequireFromString("1500"),
		Narration:           "tap",
		PaymentReference:    "order-42-payout",
	}

	first, err := a.Transfer(ctx, req)
	if err != nil || first.Status != baas.TransferPending {
		t.Fatalf("first transfer = %+v, %v (want pending)", first, err)
	}
	if !first.Fees.Equal(decimal.RequireFromString("2.5")) {
		t.Errorf("fees = %s, want 2.5", first.Fees)
	}

	second, err := a.Transfer(ctx, req)
	if err != nil {
		t.Fatalf("retried transfer must be idempotent, got error: %v", err)
	}
	if second.Status != baas.TransferSuccess {
		t.Errorf("retry should return the original transfer's live status, got %s", second.Status)
	}
	if calls["disburse"] != 2 {
		t.Errorf("disburse called %d times, want 2 (second rejected as duplicate)", calls["disburse"])
	}
}

func TestCreateSubAccountRequiresBVN(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()

	if _, err := a.CreateSubAccount(ctx, baas.CreateSubAccountRequest{
		ExternalReference: "lp-profile-1",
		EmailAddress:      "lp-one@usetapp.xyz",
		IdentityType:      "NIN",
		IdentityNumber:    "123",
	}); err == nil {
		t.Fatal("non-BVN identity should be refused")
	}

	acct, err := a.CreateSubAccount(ctx, baas.CreateSubAccountRequest{
		ExternalReference: "lp-profile-1",
		EmailAddress:      "lp-one@usetapp.xyz",
		IdentityType:      "bvn",
		IdentityNumber:    "22222222222",
	})
	if err != nil {
		t.Fatalf("CreateSubAccount: %v", err)
	}
	if acct.AccountNumber != "9977581763" || acct.Type != "virtual" {
		t.Errorf("acct = %+v", acct)
	}
}

// TestWebhookSignatureOverDataSlice pins the subtle part of Korapay's
// scheme: the HMAC covers the raw `data` object bytes only.
func TestWebhookSignatureOverDataSlice(t *testing.T) {
	a, _ := newTestAdapter(t)

	// Note deliberate non-canonical spacing inside data — the adapter
	// must HMAC the exact original bytes, not a re-marshalled form.
	data := `{"reference":"order-42-payout",  "status":"success","amount":1500,"fee":2.5,"currency":"NGN"}`
	body := []byte(`{"event":"transfer.success","data":` + data + `}`)

	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(data))
	sig := hex.EncodeToString(mac.Sum(nil))

	if !a.VerifyWebhook(body, sig) {
		t.Fatal("valid signature rejected")
	}
	if a.VerifyWebhook(body, sig[:len(sig)-2]+"ff") {
		t.Fatal("tampered signature accepted")
	}

	ev, err := a.ParseWebhook(body)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Type != "transfer.success" || ev.Status != baas.TransferSuccess ||
		ev.PaymentReference != "order-42-payout" || ev.Amount != "1500" {
		t.Errorf("event = %+v", ev)
	}
}

func TestParseVBACreditWebhook(t *testing.T) {
	a, _ := newTestAdapter(t)
	body := []byte(`{"event":"charge.success","data":{
		"reference":"KPY-DEP-9","status":"success","amount":50000,"fee":750,"currency":"NGN",
		"virtual_bank_account_details":{"virtual_bank_account":{"account_number":"9977581763","account_reference":"lp-profile-1"}}}}`)
	ev, err := a.ParseWebhook(body)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Type != "charge.success" || ev.AccountNumber != "9977581763" || ev.Status != baas.TransferSuccess {
		t.Errorf("event = %+v", ev)
	}
}
