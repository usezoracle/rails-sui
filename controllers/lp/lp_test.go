package lp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/enttest"
	"github.com/usezoracle/rails-sui/ent/lpledgerentry"
	"github.com/usezoracle/rails-sui/services/baas/korapay"
	"github.com/usezoracle/rails-sui/storage"
)

const testSecret = "sk_test_lp"

func setup(t *testing.T) (*Controller, *ent.Client, *ent.LpAccount) {
	t.Helper()
	client := enttest.Open(t, "sqlite3", "file:lp_"+t.Name()+"?mode=memory&cache=shared&_fk=1")
	t.Cleanup(func() { client.Close() })
	prev := storage.Client
	storage.Client = client
	t.Cleanup(func() { storage.Client = prev })

	ctx := context.Background()
	usr := client.User.Create().
		SetFirstName("LP").SetLastName("One").
		SetEmail("lp@usetapp.xyz").SetPassword("x").SetScope("sender").
		SetIsEmailVerified(true).
		SaveX(ctx)
	acct := client.LpAccount.Create().
		SetName("LP One").SetEmail(usr.Email).SetBvnLast4("2222").
		SetAccountReference("lp-" + usr.ID.String()).
		SetAccountNumber("1110033596").
		SetBankName("Test Bank").SetBankCode("000").
		SetBalance(decimal.NewFromInt(5000)).
		SetUser(usr).
		SaveX(ctx)

	kc := korapay.New(testSecret, "pk_test_x", "http://invalid.local", "ops@usetapp.xyz", "000")
	c := &Controller{kora: korapay.NewAdapter(kc), koraClient: kc}
	return c, client, acct
}

// signedWebhook builds a Korapay-shaped body + valid signature (HMAC
// over the raw data slice).
func signedWebhook(event, data string) (*http.Request, *httptest.ResponseRecorder) {
	body := []byte(`{"event":"` + event + `","data":` + data + `}`)
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(data))
	req := httptest.NewRequest(http.MethodPost, "/v1/korapay/webhook", bytes.NewReader(body))
	req.Header.Set("x-korapay-signature", hex.EncodeToString(mac.Sum(nil)))
	return req, httptest.NewRecorder()
}

func runWebhook(c *Controller, req *http.Request, w *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	gctx, _ := gin.CreateTestContext(w)
	gctx.Request = req
	c.Webhook(gctx)
}

// TestDepositCreditIdempotent pins the ledger's core property: a
// redelivered charge.success credits exactly once.
func TestDepositCreditIdempotent(t *testing.T) {
	c, client, acct := setup(t)
	data := `{"reference":"KPY-DEP-1","status":"success","amount":2500,"currency":"NGN",
		"virtual_bank_account_details":{"virtual_bank_account":{"account_number":"1110033596","account_reference":"` + acct.AccountReference + `"}}}`

	for i := 0; i < 3; i++ { // deliver three times
		req, w := signedWebhook("charge.success", data)
		runWebhook(c, req, w)
		if w.Code != 200 {
			t.Fatalf("delivery %d: status %d", i, w.Code)
		}
	}

	fresh := client.LpAccount.GetX(context.Background(), acct.ID)
	if want := decimal.NewFromInt(7500); !fresh.Balance.Equal(want) {
		t.Fatalf("balance = %s, want %s (credited exactly once)", fresh.Balance, want)
	}
	n := client.LpLedgerEntry.Query().CountX(context.Background())
	if n != 1 {
		t.Fatalf("ledger entries = %d, want 1", n)
	}
}

// TestWebhookRejectsBadSignature: fail closed.
func TestWebhookRejectsBadSignature(t *testing.T) {
	c, client, _ := setup(t)
	req, w := signedWebhook("charge.success", `{"reference":"X","status":"success","amount":100,
		"virtual_bank_account_details":{"virtual_bank_account":{"account_number":"1110033596"}}}`)
	req.Header.Set("x-korapay-signature", "deadbeef")
	runWebhook(c, req, w)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if n := client.LpLedgerEntry.Query().CountX(context.Background()); n != 0 {
		t.Fatalf("ledger entries after forged webhook = %d, want 0", n)
	}
}

// TestWithdrawalFailureRecredits: a reserved withdrawal whose bank
// transfer terminally fails must release the funds — exactly once,
// even when the failure webhook is redelivered.
func TestWithdrawalFailureRecredits(t *testing.T) {
	c, client, acct := setup(t)
	ctx := context.Background()

	// Reserve ₦2,000 the way Withdraw does.
	entry := client.LpLedgerEntry.Create().
		SetID(uuid.MustParse("00000000-0000-0000-0000-00000000aaaa")).
		SetEntryType(lpledgerentry.EntryTypeWithdrawal).
		SetAmount(decimal.NewFromInt(2000)).
		SetStatus(lpledgerentry.StatusPending).
		SetProviderRef(withdrawalRefPrefix + "00000000-0000-0000-0000-00000000aaaa").
		SetLpAccount(acct).
		SaveX(ctx)
	_ = entry
	client.LpAccount.UpdateOneID(acct.ID).AddBalance(decimal.NewFromInt(-2000)).ExecX(ctx)

	data := `{"reference":"kpy-x","payment_reference":"` + withdrawalRefPrefix + `00000000-0000-0000-0000-00000000aaaa","status":"failed","amount":2000}`
	for i := 0; i < 2; i++ {
		req, w := signedWebhook("transfer.failed", data)
		runWebhook(c, req, w)
		if w.Code != 200 {
			t.Fatalf("delivery %d: %d", i, w.Code)
		}
	}

	fresh := client.LpAccount.GetX(ctx, acct.ID)
	if want := decimal.NewFromInt(5000); !fresh.Balance.Equal(want) {
		t.Fatalf("balance = %s, want %s (re-credited exactly once)", fresh.Balance, want)
	}
	e := client.LpLedgerEntry.Query().OnlyX(ctx)
	if e.Status != lpledgerentry.StatusFailed {
		t.Fatalf("entry status = %s, want failed", e.Status)
	}
}

// TestWithdrawalSuccessConfirms: terminal success flips pending →
// confirmed without touching the balance again.
func TestWithdrawalSuccessConfirms(t *testing.T) {
	c, client, acct := setup(t)
	ctx := context.Background()

	client.LpLedgerEntry.Create().
		SetID(uuid.MustParse("00000000-0000-0000-0000-00000000bbbb")).
		SetEntryType(lpledgerentry.EntryTypeWithdrawal).
		SetAmount(decimal.NewFromInt(1000)).
		SetStatus(lpledgerentry.StatusPending).
		SetProviderRef(withdrawalRefPrefix + "00000000-0000-0000-0000-00000000bbbb").
		SetLpAccount(acct).
		SaveX(ctx)
	client.LpAccount.UpdateOneID(acct.ID).AddBalance(decimal.NewFromInt(-1000)).ExecX(ctx)

	data := `{"reference":"kpy-y","payment_reference":"` + withdrawalRefPrefix + `00000000-0000-0000-0000-00000000bbbb","status":"success","amount":1000}`
	req, w := signedWebhook("transfer.success", data)
	runWebhook(c, req, w)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}

	fresh := client.LpAccount.GetX(ctx, acct.ID)
	if want := decimal.NewFromInt(4000); !fresh.Balance.Equal(want) {
		t.Fatalf("balance = %s, want %s (no double movement)", fresh.Balance, want)
	}
	e := client.LpLedgerEntry.Query().OnlyX(ctx)
	if e.Status != lpledgerentry.StatusConfirmed {
		t.Fatalf("entry status = %s, want confirmed", e.Status)
	}
}

var _ = json.Marshal // keep import if fixtures change
