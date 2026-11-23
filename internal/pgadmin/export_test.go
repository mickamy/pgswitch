package pgadmin

import "context"

var (
	SCRAMClientFirstBare   = scramClientFirstBare
	SCRAMClientFinal       = scramClientFinal
	SCRAMVerifyServerFinal = scramVerifyServerFinal
	NewSCRAMNonce          = newSCRAMNonce
)

func Watch(ctx context.Context, c Conn) func(synced bool) {
	return c.watch(ctx)
}
