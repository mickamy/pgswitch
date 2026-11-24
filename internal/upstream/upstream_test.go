package upstream_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mickamy/pgswitch/internal/upstream"
)

func TestPool(t *testing.T) {
	t.Parallel()

	var made []upstream.Upstream
	pool := upstream.NewPool(func(u upstream.Upstream) string {
		made = append(made, u)

		return u.Addr + " as " + u.AdminUser
	})
	shop := upstream.Upstream{Addr: "127.0.0.1:5433", AdminUser: "postgres"}
	shopAsOwner := upstream.Upstream{Addr: "127.0.0.1:5433", AdminUser: "shop"}
	blog := upstream.Upstream{Addr: "127.0.0.1:5434", AdminUser: "postgres"}

	assert.Equal(t, "127.0.0.1:5433 as postgres", pool.Get(shop))
	assert.Equal(t, "127.0.0.1:5433 as postgres", pool.Get(shop), "kept")
	assert.Equal(t, "127.0.0.1:5433 as shop", pool.Get(shopAsOwner))
	assert.Equal(t, "127.0.0.1:5434 as postgres", pool.Get(blog))
	assert.Equal(t, []upstream.Upstream{shop, shopAsOwner, blog}, made, "one per upstream")

	assert.Equal(t, map[string]string{
		"127.0.0.1:5433": "127.0.0.1:5433 as shop",
		"127.0.0.1:5434": "127.0.0.1:5434 as postgres",
	}, pool.ByAddr(), "one per server, as last used")
	pool.Get(shop)
	assert.Equal(t, "127.0.0.1:5433 as postgres", pool.ByAddr()["127.0.0.1:5433"])
}
