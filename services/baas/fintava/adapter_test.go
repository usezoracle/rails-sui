package fintava

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/services/baas"
)

// TestVerifyWebhookSHA512 pins the signature scheme: HMAC-SHA512 over
// the RAW body, hex, keyed with the dashboard webhook secret.
func TestVerifyWebhookSHA512(t *testing.T) {
	a := NewAdapter(New("key", "whsec", ""))
	body := []byte(`{"type":"account_funded","data":{"amount":100.00,"reference":"r1","accountNumber":"1234567890"}}`)
	mac := hmac.New(sha512.New, []byte("whsec"))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	if !a.VerifyWebhook(body, sig) {
		t.Fatal("valid signature rejected")
	}
	if a.VerifyWebhook(body, "deadbeef") {
		t.Fatal("forged signature accepted")
	}
	if a.VerifyWebhook(append(body, ' '), sig) {
		t.Fatal("tampered body accepted")
	}
	none := NewAdapter(New("key", "", ""))
	if none.VerifyWebhook(body, sig) {
		t.Fatal("must fail closed without a secret")
	}
}

// TestParseWebhookDeposit pins the deposit normalisation: credit
// events become Type "deposit" routed by account number.
func TestParseWebhookDeposit(t *testing.T) {
	a := NewAdapter(New("key", "s", ""))
	ev, err := a.ParseWebhook([]byte(`{
		"type": "account_funded",
		"data": {"amount": "2500.00", "reference": "FTV-99", "accountNumber": "0123456789", "accountName": "LP ONE", "status": "SUCCESS"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != "deposit" || ev.AccountNumber != "0123456789" || ev.Status != baas.TransferSuccess {
		t.Fatalf("ev = %+v", ev)
	}
	if ev.ProviderRef != "FTV-99" || ev.Amount != "2500" {
		t.Fatalf("ref/amount = %s/%s", ev.ProviderRef, ev.Amount)
	}
}

// TestParseWebhookTransferFinality pins transfer events routing by our
// CustomerReference, and reversal → failed.
func TestParseWebhookTransferFinality(t *testing.T) {
	a := NewAdapter(New("key", "s", ""))
	ev, _ := a.ParseWebhook([]byte(`{
		"type": "customer_bank_transfer",
		"data": {"amount": 1500, "reference": "FTV-1", "customerReference": "lpwd-abc", "status": "SUCCESS"}
	}`))
	if ev.PaymentReference != "lpwd-abc" || ev.Status != baas.TransferSuccess {
		t.Fatalf("transfer ev = %+v", ev)
	}
	rev, _ := a.ParseWebhook([]byte(`{
		"type": "debit_transfer_reversal",
		"data": {"amount": 1500, "transactionReference": "lpwd-abc", "status": "REVERSED"}
	}`))
	if rev.Status != baas.TransferFailed {
		t.Fatalf("reversal must normalise to failed, got %s", rev.Status)
	}
}

// TestTransferResolvesNameFirst pins the mis-send guard: the adapter
// refuses to move money when the beneficiary cannot be resolved, and
// sends the rail the RESOLVED name plus our reference when it can.
func TestTransferResolvesNameFirst(t *testing.T) {
	var gotTransfer map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/name/enquiry":
			if r.Header.Get("Authorization") != "Bearer sk_f" {
				t.Errorf("auth header = %q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": true, "data": map[string]any{
				"accountName": "MERCHANT NAME", "accountNumber": r.URL.Query().Get("accountNumber"),
			}})
		case r.URL.Path == "/bank/credit/merchant":
			_ = json.NewDecoder(r.Body).Decode(&gotTransfer)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": true, "data": map[string]any{
				"reference": "FTV-OK", "status": "PENDING",
			}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := NewAdapter(New("sk_f", "s", srv.URL))
	tr, err := a.Transfer(t.Context(), baas.TransferRequest{
		BeneficiaryBankCode: "058",
		BeneficiaryAccount:  "0123456789",
		Amount:              decimal.NewFromInt(2000),
		Narration:           "Tapp settlement",
		PaymentReference:    "rctp-77-0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != baas.TransferPending || tr.Reference != "FTV-OK" {
		t.Fatalf("transfer = %+v", tr)
	}
	if gotTransfer["accountName"] != "MERCHANT NAME" {
		t.Fatalf("rail got accountName %q, want the RESOLVED name", gotTransfer["accountName"])
	}
	if gotTransfer["CustomerReference"] != "rctp-77-0" {
		t.Fatalf("rail got CustomerReference %q", gotTransfer["CustomerReference"])
	}
}

// TestStatusNormalisation: unknown statuses must NEVER become success.
func TestStatusNormalisation(t *testing.T) {
	cases := map[string]baas.TransferStatus{
		"SUCCESS": baas.TransferSuccess, "Paid": baas.TransferSuccess,
		"FAILED": baas.TransferFailed, "REVERSED": baas.TransferFailed,
		"PROCESSING": baas.TransferPending, "weird_new_state": baas.TransferPending, "": baas.TransferPending,
	}
	for raw, want := range cases {
		if got := normalizeStatus(raw); got != want {
			t.Errorf("normalizeStatus(%q) = %s, want %s", raw, got, want)
		}
	}
}

// TestListAccountsMerchantWallet pins the float read used by the
// Route B/C gate.
func TestListAccountsMerchantWallet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/merchant/balance" {
			t.Errorf("path = %s", r.URL.Path)
		}
		// Live-verified shape: nested balance object + wallet NUBAN.
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 200, "message": "successful", "data": map[string]any{
			"accountName": "octa hq", "accountNumber": "0033988783",
			"balance": map[string]any{"bookedBalance": 150000.50, "availableBalance": 150000.50},
		}})
	}))
	defer srv.Close()
	a := NewAdapter(New("sk", "s", srv.URL))
	accts, err := a.ListAccounts(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(accts) != 1 || !accts[0].Balance.Equal(decimal.RequireFromString("150000.50")) || accts[0].Currency != "NGN" {
		t.Fatalf("accounts = %+v", accts)
	}
	if accts[0].AccountNumber != "0033988783" {
		t.Fatalf("wallet NUBAN = %q (the reload destination)", accts[0].AccountNumber)
	}
}
