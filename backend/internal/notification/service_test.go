package notification

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidType(t *testing.T) {
	assert.True(t, ValidType("system"))
	assert.True(t, ValidType("order"))
	assert.True(t, ValidType("ticket"))
	assert.False(t, ValidType(""))
	assert.False(t, ValidType("invoice"))
	assert.False(t, ValidType("SYSTEM"))
}

func TestRecordRejectsInvalidPayload(t *testing.T) {
	s := &Service{}
	assert.Error(t, s.Record(0, "system", "t", "c", ""))
	assert.Error(t, s.Record(1, "bad-type", "t", "c", ""))
	assert.Error(t, s.Record(1, "system", "  ", "c", ""))

	var nilSvc *Service
	assert.Error(t, nilSvc.Record(1, "system", "t", "c", ""))
}
