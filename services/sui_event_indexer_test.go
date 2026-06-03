package services

import "testing"

func TestEventTypeMatchesUsesEventTypeModule(t *testing.T) {
	packageID := "0x99b54bda5d3b31b4453dd1eec7f26cf985f77090c2d2c9caf866142f41c3dec7"
	eventType := packageID + "::events::OrderCreated"

	if !eventTypeMatches(packageID, eventType, "OrderCreated") {
		t.Fatal("expected OrderCreated event type to match")
	}
}

func TestEventTypeMatchesRejectsWrongModule(t *testing.T) {
	packageID := "0x99b54bda5d3b31b4453dd1eec7f26cf985f77090c2d2c9caf866142f41c3dec7"
	eventType := packageID + "::order::OrderCreated"

	if eventTypeMatches(packageID, eventType, "OrderCreated") {
		t.Fatal("expected non-events module to be rejected")
	}
}
