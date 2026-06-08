package mfb

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/services/baas"
)

// TestNormalizeStatus pins the vendor→neutral status mapping. This is a money
// path: a mis-mapped status would settle (release USDC) on a failed payout or
// never settle a successful one.
func TestNormalizeStatus(t *testing.T) {
	cases := map[string]baas.TransferStatus{
		"success":    baas.TransferSuccess,
		"successful": baas.TransferSuccess,
		"completed":  baas.TransferSuccess,
		"Completed":  baas.TransferSuccess, // case-insensitive
		"SUCCESS":    baas.TransferSuccess,
		"00":         baas.TransferSuccess,
		"approved":   baas.TransferSuccess,
		" success ":  baas.TransferSuccess, // trimmed

		"failed":    baas.TransferFailed,
		"rejected":  baas.TransferFailed,
		"cancelled": baas.TransferFailed,
		"canceled":  baas.TransferFailed,
		"reversed":  baas.TransferFailed,
		"declined":  baas.TransferFailed,
		"error":     baas.TransferFailed,

		"":           baas.TransferPending, // unknown → pending, never assumed terminal
		"pending":    baas.TransferPending,
		"processing": baas.TransferPending,
		"in_flight":  baas.TransferPending,
		"weird":      baas.TransferPending,
	}

	for in, want := range cases {
		if got := normalizeStatus(in); got != want {
			t.Errorf("normalizeStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestToTransfer verifies the vendor Transfer is mapped onto the neutral type
// including normalised status and preserved raw status.
func TestToTransfer(t *testing.T) {
	in := &Transfer{
		SessionID:        "sess-1",
		PaymentReference: "routeB-abc",
		Amount:           decimal.NewFromInt(5000),
		Fees:             decimal.NewFromInt(10),
		Status:           "Completed",
		ResponseMessage:  "ok",
		CreditAccount:    "0123456789",
	}
	got := toTransfer(in)
	if got.Reference != "sess-1" {
		t.Errorf("Reference = %q, want sess-1", got.Reference)
	}
	if got.PaymentReference != "routeB-abc" {
		t.Errorf("PaymentReference = %q, want routeB-abc", got.PaymentReference)
	}
	if got.Status != baas.TransferSuccess {
		t.Errorf("Status = %q, want success", got.Status)
	}
	if got.RawStatus != "Completed" {
		t.Errorf("RawStatus = %q, want Completed", got.RawStatus)
	}
	if !got.Amount.Equal(decimal.NewFromInt(5000)) {
		t.Errorf("Amount = %s, want 5000", got.Amount)
	}
	if !got.Fees.Equal(decimal.NewFromInt(10)) {
		t.Errorf("Fees = %s, want 10", got.Fees)
	}
}

// TestToAccount verifies balance fields are mapped (Balance is the spendable
// float the dashboard and float check rely on).
func TestToAccount(t *testing.T) {
	in := Account{
		ID:             "acct-1",
		AccountNumber:  "0110890780",
		AccountName:    "BLAZE AFRICA LTD",
		AccountBalance: decimal.NewFromInt(250000),
		LedgerBalance:  decimal.NewFromInt(260000),
		Status:         "Active",
	}
	got := toAccount(in)
	if got.ID != "acct-1" || got.AccountNumber != "0110890780" || got.AccountName != "BLAZE AFRICA LTD" {
		t.Errorf("identity fields mismapped: %+v", got)
	}
	if !got.Balance.Equal(decimal.NewFromInt(250000)) {
		t.Errorf("Balance = %s, want 250000", got.Balance)
	}
	if !got.LedgerBalance.Equal(decimal.NewFromInt(260000)) {
		t.Errorf("LedgerBalance = %s, want 260000", got.LedgerBalance)
	}
	if got.Status != "Active" {
		t.Errorf("Status = %q, want Active", got.Status)
	}
}

// TestWebhookVerifyAndParse covers the adapter's webhook handling: signature
// verification and neutral-event parsing with normalised status.
func TestWebhookVerifyAndParse(t *testing.T) {
	a := NewAdapter(nil, "shhh")
	body := []byte(`{"type":"transfer","paymentReference":"routeB-xyz","sessionId":"s1","status":"Completed","amount":"5000","accountNumber":"012"}`)

	if !a.WebhookConfigured() {
		t.Fatal("WebhookConfigured = false, want true")
	}
	// Wrong signature rejected.
	if a.VerifyWebhook(body, "deadbeef") {
		t.Error("VerifyWebhook accepted a bad signature")
	}
	// No-secret adapter never verifies.
	if NewAdapter(nil, "").VerifyWebhook(body, "anything") {
		t.Error("VerifyWebhook accepted with no secret configured")
	}

	evt, err := a.ParseWebhook(body)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if evt.PaymentReference != "routeB-xyz" || evt.ProviderRef != "s1" {
		t.Errorf("parsed refs wrong: %+v", evt)
	}
	if evt.Status != baas.TransferSuccess {
		t.Errorf("parsed status = %q, want success", evt.Status)
	}
	if evt.RawStatus != "Completed" {
		t.Errorf("parsed raw status = %q, want Completed", evt.RawStatus)
	}
}
