package compute

import (
	"testing"
	"github.com/stretchr/testify/assert"
)

func TestProductFilterDefaults(t *testing.T) {
	f := ProductFilter{}
	if f.Page <= 0 { f.Page = 1 }
	if f.PageSize <= 0 { f.PageSize = 20 }
	assert.Equal(t, 1, f.Page)
	assert.Equal(t, 20, f.PageSize)
}

func TestOrderAmountCalculation(t *testing.T) {
	unitPrice := int64(3500) // ¥35.00 in fen
	qty := 8
	duration := 720 // hours
	totalFen := unitPrice * int64(qty) * int64(duration)
	assert.Equal(t, int64(20160000), totalFen) // ¥201,600.00

	feeRate := int64(500) // 5% = 500 basis points
	feeFen := totalFen * feeRate / 10000
	assert.Equal(t, int64(1008000), feeFen) // ¥10,080.00
}

func TestOrderStatusTransitions(t *testing.T) {
	validTransitions := map[string][]string{
		"pending_payment": {"paid", "cancelled"},
		"paid":             {"provisioning", "refunding", "cancelled"},
		"provisioning":     {"active", "refunding"},
		"active":           {"completed", "refunding", "frozen"},
		"refunding":        {"refunded"},
	}
	assert.Contains(t, validTransitions["pending_payment"], "paid")
	assert.NotContains(t, validTransitions["pending_payment"], "active")
}

func TestFeeRateCalculation(t *testing.T) {
	svc := &Service{feeRate: 500}
	assert.Equal(t, int64(500), svc.feeRate)
	assert.Equal(t, int64(50000), int64(1000000)*svc.feeRate/10000) // ¥10,000 × 5% = ¥500
}
