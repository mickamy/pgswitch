package ident_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mickamy/pgswitch/internal/ident"
)

func TestShortHash(t *testing.T) {
	t.Parallel()

	got := ident.ShortHash("shop_development", "feature/coupons")

	assert.Len(t, got, 8)
	assert.Equal(t, got, ident.ShortHash("shop_development", "feature/coupons"))
	assert.NotEqual(t, got, ident.ShortHash("shop_developmentfeature/coupons"))
	assert.NotEqual(t, got, ident.ShortHash("shop_development", "feature/coupon"))
}
