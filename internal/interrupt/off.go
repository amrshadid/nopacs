//go:build !interrupt

package interrupt

// At marks the durable write of a step done. Without the build tag
// "interrupt" it does nothing, and the compiler removes each call.
func At(scope, kind, step string) {}
