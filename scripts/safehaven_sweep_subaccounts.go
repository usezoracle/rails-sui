//go:build ignore

// safehaven_sweep_subaccounts.go
//
// One self-contained process for Safe Haven sub-account liquidity consolidation.
// A single run does, in order:
//
//  1. Resolve Safe Haven's intra-bank code (auto, or -bankcode override).
//  2. Inventory: list main account(s) and all sub-accounts with balance + status.
//  3. Diagnose: name-enquiry the destination so you can see if it is transactable
//     (Dormant accounts return "Invalid Account" / NIP code 07).
//  4. Plan/execute: print the per-account sweep plan; debit only with -confirm
//     AND only once the destination name-enquiry succeeds.
//
// SAFE BY DEFAULT: dry-run unless -confirm. Steps 1-3 are read-only (no money
// moves). Destination MUST be a company-controlled account, never personal.
//
// Usage:
//
//	go run scripts/safehaven_sweep_subaccounts.go                 # inventory + diagnostics + dry-run plan
//	go run scripts/safehaven_sweep_subaccounts.go -confirm        # same, then execute the sweep
//	go run scripts/safehaven_sweep_subaccounts.go -bankcode 090286 -dest 0110890780
//	go run scripts/safehaven_sweep_subaccounts.go -account 5655858793 -accbank 090286  # ad-hoc name-enquiry test
//
// Idempotency: each transfer uses paymentReference "sweep-<subAccount>-<YYYYMM>",
// so a re-run within the same month will not double-pay.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/services/baas/safehaven"
)

func main() {
	var (
		dest             = flag.String("dest", "0110890780", "destination account number (must be company-controlled)")
		minBal           = flag.String("min", "1", "skip sub-accounts with balance below this (NGN)")
		confirm          = flag.Bool("confirm", false, "actually execute transfers; without this it is a dry-run")
		bankCodeOverride = flag.String("bankcode", "", "override Safe Haven's intra-bank code (else auto-resolved by name)")
		testAccount      = flag.String("account", "", "optional: ad-hoc name-enquiry test on this account number")
		testAccountBank  = flag.String("accbank", "", "bank code for -account test (else uses Safe Haven's code)")
	)
	flag.Parse()

	minAmount, err := decimal.NewFromString(*minBal)
	if err != nil {
		fmt.Println("invalid -min:", err)
		os.Exit(1)
	}

	cfg := config.SafehavenConfig()
	auth, err := safehaven.NewAuthenticator(safehaven.Config{
		ClientID:      cfg.ClientID,
		PrivateKeyPEM: cfg.PrivateKeyPEM,
		BaseURL:       cfg.BaseURL,
		Audience:      cfg.Audience,
		Issuer:        cfg.Issuer,
	})
	if err != nil {
		fmt.Println("auth init:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := safehaven.NewClient(auth)

	mode := "DRY-RUN (no money will move)"
	if *confirm {
		mode = "EXECUTE"
	}
	fmt.Printf("=== Safe Haven sub-account sweep — %s ===\n", mode)
	fmt.Printf("Destination: %s\n\n", *dest)

	// --- 1. resolve Safe Haven's intra-bank code -----------------------------
	banks, err := client.GetBanks(ctx)
	if err != nil {
		fmt.Println("get banks:", err)
		os.Exit(1)
	}
	fmt.Println("[banks] candidates containing HAVEN:")
	for _, b := range banks {
		if strings.Contains(strings.ToUpper(b.Name), "HAVEN") {
			fmt.Printf("  - %q  bankCode=%s\n", b.Name, b.BankCode)
		}
	}
	shBankCode := *bankCodeOverride
	if shBankCode == "" {
		for _, b := range banks {
			if strings.Contains(strings.ToUpper(b.Name), "SAFE HAVEN") {
				shBankCode = b.BankCode
				break
			}
		}
	}
	if shBankCode == "" {
		fmt.Println("could not resolve Safe Haven bank code; pass -bankcode <code>")
		os.Exit(1)
	}
	fmt.Printf("[banks] using Safe Haven bankCode=%s\n\n", shBankCode)

	// --- 2. inventory (read-only) --------------------------------------------
	fmt.Println("[inventory] main accounts:")
	if mains, err := client.ListAccounts(ctx, false); err != nil {
		fmt.Println("  list main accounts:", err)
	} else {
		for _, a := range mains {
			fmt.Printf("  %-12s ₦%-12s %-10s %s\n", a.AccountNumber, a.AccountBalance.String(), a.Status, a.AccountName)
		}
	}

	fmt.Println("\n[inventory] sub-accounts:")
	subs, err := client.ListAccounts(ctx, true)
	if err != nil {
		fmt.Println("  list sub-accounts:", err)
		os.Exit(1)
	}
	subTotal := decimal.Zero
	dormant := 0
	for i, a := range subs {
		if strings.EqualFold(a.Status, "Dormant") {
			dormant++
		}
		subTotal = subTotal.Add(a.AccountBalance)
		fmt.Printf("  %-3d %-12s ₦%-12s %-10s %s\n", i+1, a.AccountNumber, a.AccountBalance.String(), a.Status, a.AccountName)
	}
	fmt.Printf("  -> %d sub-accounts, total ₦%s, dormant=%d\n\n", len(subs), subTotal.String(), dormant)

	// --- 3. diagnostics: name-enquiry (read-only) ----------------------------
	if *testAccount != "" {
		bc := *testAccountBank
		if bc == "" {
			bc = shBankCode
		}
		if ne, err := client.NameEnquiry(ctx, bc, *testAccount); err != nil {
			fmt.Printf("[diag] name-enquiry %s@%s FAILED: %v\n", *testAccount, bc, err)
		} else {
			fmt.Printf("[diag] name-enquiry %s@%s -> %q (transactable)\n", *testAccount, bc, ne.AccountName)
		}
		fmt.Println()
	}

	destEnq, destErr := client.NameEnquiry(ctx, shBankCode, *dest)
	destOK := destErr == nil
	if !destOK {
		fmt.Printf("[diag] destination %s name-enquiry FAILED: %v\n", *dest, destErr)
		fmt.Println("       NIP code 07 'Invalid Account' here usually means the account is Dormant/")
		fmt.Println("       not transactable. Activate it with Safe Haven before sweeping. Transfers skipped.")
	} else {
		fmt.Printf("[diag] destination %s -> %q\n", *dest, destEnq.AccountName)
		if !strings.Contains(strings.ToUpper(destEnq.AccountName), "BLAZE AFRICA") {
			fmt.Println("       WARNING: destination name is not the company account; transfers skipped.")
			destOK = false
		}
	}
	fmt.Println()

	// --- 4. plan / execute ---------------------------------------------------
	yyyymm := timeYYYYMM()
	var planned, moved, failed int
	planTotal := decimal.Zero
	canExecute := *confirm && destOK

	if *confirm && !destOK {
		fmt.Println("[sweep] -confirm given but destination is not verified/transactable; NOT executing.")
	}

	for _, a := range subs {
		if a.AccountNumber == *dest || a.AccountBalance.LessThan(minAmount) {
			continue
		}
		planned++
		planTotal = planTotal.Add(a.AccountBalance)
		ref := fmt.Sprintf("sweep-%s-%s", a.AccountNumber, yyyymm)

		if !canExecute {
			fmt.Printf("[plan] %-12s ₦%-12s -> %s  [%s]  (%s)\n",
				a.AccountNumber, a.AccountBalance.String(), *dest, ref, a.Status)
			continue
		}

		enq, err := client.NameEnquiry(ctx, shBankCode, *dest)
		if err != nil {
			failed++
			fmt.Printf("[FAIL] %-12s name-enquiry: %v\n", a.AccountNumber, err)
			continue
		}
		res, err := client.Transfer(ctx, safehaven.TransferRequest{
			NameEnquiryReference: enq.SessionID,
			DebitAccountNumber:   a.AccountNumber,
			BeneficiaryBankCode:  shBankCode,
			BeneficiaryAccount:   *dest,
			Amount:               a.AccountBalance,
			Narration:            "LP liquidity consolidation " + yyyymm,
			PaymentReference:     ref,
			SaveBeneficiary:      false,
		})
		if err != nil {
			failed++
			fmt.Printf("[FAIL] %-12s ₦%-12s -> %s: %v\n", a.AccountNumber, a.AccountBalance.String(), *dest, err)
			continue
		}
		moved++
		fmt.Printf("[OK]   %-12s ₦%-12s -> %s  status=%s ref=%s\n",
			a.AccountNumber, a.AccountBalance.String(), *dest, res.Status, res.PaymentReference)
	}

	fmt.Printf("\n--- summary ---\n")
	if canExecute {
		fmt.Printf("planned=%d moved=%d failed=%d total-attempted=₦%s\n", planned, moved, failed, planTotal.String())
	} else {
		fmt.Printf("planned=%d total=₦%s (dry-run / not executed)\n", planned, planTotal.String())
	}
}

func timeYYYYMM() string {
	now := time.Now()
	return fmt.Sprintf("%04d%02d", now.Year(), int(now.Month()))
}
