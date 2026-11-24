package proxy

import "context"

type containerKey struct{}

func withContainer(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, containerKey{}, id)
}

// Container returns the container pgswitch relay declared for the
// connection being routed with ctx, if it did.
func Container(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(containerKey{}).(string)

	return id, ok
}
