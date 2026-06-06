package token

import (
	"regexp"
	"testing"
)

func TestGenerateOTP(t *testing.T) {
	re := regexp.MustCompile(`^\d{6}$`)
	seen := map[string]int{}
	for i := 0; i < 500; i++ {
		otp, err := GenerateOTP()
		if err != nil {
			t.Fatalf("GenerateOTP: %v", err)
		}
		if !re.MatchString(otp) {
			t.Fatalf("OTP %q is not exactly 6 digits", otp)
		}
		seen[otp]++
	}
	// Sanity: 500 draws from 10^6 should be overwhelmingly distinct. Allow a
	// little slack for the birthday paradox without making the test flaky.
	if len(seen) < 490 {
		t.Fatalf("OTP entropy looks low: %d distinct of 500", len(seen))
	}
}
