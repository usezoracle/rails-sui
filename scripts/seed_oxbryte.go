package main

import (
	"context"
	"fmt"
	"time"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/user"
	"github.com/usezoracle/rails-sui/ent/senderordertoken"
	"github.com/usezoracle/rails-sui/ent/senderprofile"
	"github.com/usezoracle/rails-sui/ent/token"
	"github.com/usezoracle/rails-sui/storage"
	"github.com/shopspring/decimal"
)

func main() {
	DSN := config.DBConfig()
	if err := storage.DBConnection(DSN); err != nil {
		panic(fmt.Errorf("database DBConnection: %w", err))
	}
	client := storage.GetClient()
	defer client.Close()

	ctx := context.Background()
	email := "oxbryte@gmail.com"

	fmt.Printf("Querying user %s...\n", email)
	u, err := client.User.Query().Where(user.EmailEQ(email)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			fmt.Printf("User %s not found. Creating user as a merchant/sender...\n", email)
			u, err = client.User.Create().
				SetFirstName("Oxbryte").
				SetLastName("Merchant").
				SetEmail(email).
				SetPassword("password").
				SetScope("sender").
				SetIsEmailVerified(true).
				Save(ctx)
			if err != nil {
				panic(fmt.Errorf("failed to create user: %w", err))
			}
		} else {
			panic(fmt.Errorf("failed to query user: %w", err))
		}
	}

	fmt.Printf("User found/created: ID=%s, Scope=%s\n", u.ID, u.Scope)

	// Fetch or create SenderProfile
	sp, err := u.QuerySenderProfile().Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			fmt.Println("SenderProfile not found. Creating one...")
			sp, err = client.SenderProfile.Create().
				SetUser(u).
				SetIsActive(true).
				Save(ctx)
			if err != nil {
				panic(fmt.Errorf("failed to create sender profile: %w", err))
			}
		} else {
			panic(fmt.Errorf("failed to query sender profile: %w", err))
		}
	}

	// Update SenderProfile to ensure it is active
	if !sp.IsActive {
		fmt.Println("Activating SenderProfile...")
		sp, err = sp.Update().SetIsActive(true).Save(ctx)
		if err != nil {
			panic(fmt.Errorf("failed to activate sender profile: %w", err))
		}
	}

	fmt.Printf("SenderProfile active: ID=%s\n", sp.ID)

	// Seed SenderOrderTokens for all enabled tokens so that the sender can initiate orders
	fmt.Println("Seeding SenderOrderTokens...")
	tokens, err := client.Token.Query().All(ctx)
	if err != nil {
		panic(fmt.Errorf("failed querying tokens: %w", err))
	}
	for _, t := range tokens {
		// Delete any existing SenderOrderToken for this token/sender first
		_, _ = client.SenderOrderToken.Delete().
			Where(
				senderordertoken.HasSenderWith(senderprofile.IDEQ(sp.ID)),
				senderordertoken.HasTokenWith(token.IDEQ(t.ID)),
			).Exec(ctx)

		_, err = client.SenderOrderToken.
			Create().
			SetSender(sp).
			SetToken(t).
			SetFeePercent(decimal.NewFromFloat(0.01)). // 1% fee
			SetFeeAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8"). // Example fee address
			SetRefundAddress("0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"). // Example refund address
			Save(ctx)
		if err != nil {
			fmt.Printf("SenderOrderToken for token %s creation error: %v\n", t.Symbol, err)
		} else {
			fmt.Printf("Seeded SenderOrderToken for token %s\n", t.Symbol)
		}
	}

	// Check for existing MerchantBankAccount
	mba, err := sp.QueryMerchantBankAccount().Only(ctx)
	now := time.Now()
	if err != nil {
		if ent.IsNotFound(err) {
			fmt.Println("MerchantBankAccount not found. Creating one...")
			mba, err = client.MerchantBankAccount.Create().
				SetSenderProfile(sp).
				SetCurrency("NGN").
				SetBankCode("GTBINGLA").
				SetAccountNumber("0454171926").
				SetAccountName("OGUNDELE OLUMIDE SILAS").
				SetVerifiedAt(now).
				Save(ctx)
			if err != nil {
				panic(fmt.Errorf("failed to create merchant bank account: %w", err))
			}
		} else {
			panic(fmt.Errorf("failed to query merchant bank account: %w", err))
		}
	} else {
		fmt.Println("MerchantBankAccount already exists. Updating it...")
		mba, err = mba.Update().
			SetCurrency("NGN").
			SetBankCode("GTBINGLA").
			SetAccountNumber("0454171926").
			SetAccountName("OGUNDELE OLUMIDE SILAS").
			SetVerifiedAt(now).
			Save(ctx)
		if err != nil {
			panic(fmt.Errorf("failed to update merchant bank account: %w", err))
		}
	}

	fmt.Printf("Successfully seeded merchant bank account: ID=%s, Currency=%s, BankCode=%s, AccountNumber=%s, AccountName=%s, VerifiedAt=%s\n",
		mba.ID, mba.Currency, mba.BankCode, mba.AccountNumber, mba.AccountName, mba.VerifiedAt.Format(time.RFC3339))
}
