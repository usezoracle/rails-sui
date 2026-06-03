package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/tappcard"
	userEnt "github.com/usezoracle/rails-sui/ent/user"
	"github.com/usezoracle/rails-sui/storage"
)

const activationAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func generateActivationToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, 16)
	for i, b := range buf {
		out[i] = activationAlphabet[int(b)&0x1F]
	}
	return string(out), nil
}

func generateRandomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func main() {
	DSN := config.DBConfig()
	if err := storage.DBConnection(DSN); err != nil {
		panic(fmt.Sprintf("Failed to connect to database: %v", err))
	}
	client := storage.GetClient()
	defer client.Close()

	ctx := context.Background()
	email := "deodru6@gmail.com"

	fmt.Printf("Looking up user: %s\n", email)
	user, err := client.User.Query().Where(userEnt.EmailEQ(strings.ToLower(email))).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			fmt.Printf("User not found, creating a new user with email %s\n", email)
			user, err = client.User.Create().
				SetEmail(strings.ToLower(email)).
				SetFirstName("Deo").
				SetLastName("Dru").
				SetPassword("google-oauth").
				SetScope("cardholder").
				SetIsEmailVerified(true).
				SetHasEarlyAccess(true).
				Save(ctx)
			if err != nil {
				panic(fmt.Sprintf("Failed to create user: %v", err))
			}
		} else {
			panic(fmt.Sprintf("Failed to query user: %v", err))
		}
	}

	fmt.Printf("User: ID=%s Email=%s Scope=%s\n", user.ID, user.Email, user.Scope)

	// Check if this user already has a card
	card, err := client.TappCard.Query().
		Where(tappcard.HasUserWith(userEnt.IDEQ(user.ID))).
		First(ctx)
	if err == nil {
		fmt.Printf("User already has a card linked: ID=%s, Status=%s, ActivationToken=%s\n", 
			card.ID, card.Status, card.ActivationToken)
		fmt.Println("Updating the existing card to live status with mock keys...")
		_, err = card.Update().
			SetStatus(tappcard.StatusLive).
			SetCardUIDHash(generateRandomBytes(32)).
			SetCapObjectID("0xd8bd3ec04bf496ecd6e7cd4a02b18fc85f6930c91bd208cd7c144d17b7ab804d").
			SetCoinType("0xa1ec7fc00a6f40db9693ad1415d0c193ad3906494428cf252621037bd7117e29::usdc::USDC").
			SetLinkingProof(generateRandomBytes(32)).
			SetPinVerifier(generateRandomBytes(32)).
			SetCardPassword([]byte{0x01, 0x02, 0x03, 0x04}).
			SetCurrentTokenCiphertext(generateRandomBytes(32)).
			SetTokenRotatedAt(time.Now()).
			SetDailyLimitSubunit(100000000).
			SetPerTapLimitSubunit(10000000).
			SetStepUpThresholdSubunit(50000000).
			SetNeedsResync(false).
			SetPinAttemptsRemaining(5).
			Save(ctx)
		if err != nil {
			panic(fmt.Sprintf("Failed to update existing card: %v", err))
		}
		fmt.Println("Card successfully updated!")
		return
	}

	fmt.Println("Creating a new live card for the user...")
	token, err := generateActivationToken()
	if err != nil {
		panic(fmt.Sprintf("Failed to generate activation token: %v", err))
	}

	card, err = client.TappCard.Create().
		SetActivationToken(token).
		SetStatus(tappcard.StatusLive).
		SetUser(user).
		SetCardUIDHash(generateRandomBytes(32)).
		SetCapObjectID("0xd8bd3ec04bf496ecd6e7cd4a02b18fc85f6930c91bd208cd7c144d17b7ab804d").
		SetCoinType("0xa1ec7fc00a6f40db9693ad1415d0c193ad3906494428cf252621037bd7117e29::usdc::USDC").
		SetLinkingProof(generateRandomBytes(32)).
		SetPinVerifier(generateRandomBytes(32)).
		SetCardPassword([]byte{0x01, 0x02, 0x03, 0x04}).
		SetCurrentTokenCiphertext(generateRandomBytes(32)).
		SetTokenRotatedAt(time.Now()).
		SetDailyLimitSubunit(100000000).
		SetPerTapLimitSubunit(10000000).
		SetStepUpThresholdSubunit(50000000).
		SetNeedsResync(false).
		SetPinAttemptsRemaining(5).
		Save(ctx)
	if err != nil {
		panic(fmt.Sprintf("Failed to create card: %v", err))
	}

	fmt.Printf("Successfully created and linked card! ID=%s Status=%s ActivationToken=%s\n", 
		card.ID, card.Status, card.ActivationToken)
}
