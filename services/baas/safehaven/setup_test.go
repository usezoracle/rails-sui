package safehaven

import "testing"

func TestNewClientFromCredentials_NotConfigured(t *testing.T) {
	if _, err := NewClientFromCredentials("", "pem", "", "", ""); err != ErrNotConfigured {
		t.Errorf("empty clientID: got %v, want ErrNotConfigured", err)
	}
	if _, err := NewClientFromCredentials("cid", "", "", "", ""); err != ErrNotConfigured {
		t.Errorf("empty key: got %v, want ErrNotConfigured", err)
	}
}

func TestDefaultRegistry(t *testing.T) {
	if Default() != nil {
		t.Fatal("Default should be nil before SetDefault")
	}
	c := &Client{}
	SetDefault(c)
	if Default() != c {
		t.Fatal("Default did not return the registered client")
	}
	SetDefault(nil) // reset for other tests
}

func TestPaymentReference(t *testing.T) {
	got := PaymentReference("routeA", "abc123")
	if got != "routeA-abc123" {
		t.Errorf("PaymentReference = %q, want routeA-abc123", got)
	}
	// Deterministic: same inputs → same reference (idempotency contract).
	if PaymentReference("routeA", "abc123") != got {
		t.Error("PaymentReference is not deterministic")
	}
}
