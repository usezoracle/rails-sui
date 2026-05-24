package test

import (
	"context"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
)

// MockOrderService satisfies types.OrderService for use in unit tests.
type MockOrderService struct {
	mock.Mock
}

func (m *MockOrderService) CreateOrder(ctx context.Context, orderID uuid.UUID) error {
	return nil
}

func (m *MockOrderService) RefundOrder(ctx context.Context, orderID string) error {
	return nil
}

func (m *MockOrderService) SettleOrder(ctx context.Context, orderID uuid.UUID) error {
	return nil
}

func (m *MockOrderService) SponsorTransaction(ctx context.Context, txBytes string, sender string) (string, string, error) {
	return "", "", nil
}
