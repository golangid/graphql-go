package trace

import (
	"context"

	"github.com/golangid/graphql-go/errors"
)

type TraceValidationFinishFunc = TraceQueryFinishFunc

// Deprecated: use ValidationTracerContext.
type ValidationTracer interface {
	TraceValidation() TraceValidationFinishFunc
}

type ValidationTracerContext interface {
	TraceValidation(ctx context.Context) TraceValidationFinishFunc
}

type NoopValidationTracer struct{}

// Deprecated: use a Tracer which implements ValidationTracerContext.
func (NoopValidationTracer) TraceValidation() TraceValidationFinishFunc {
	return func(data []byte, errs []*errors.QueryError) {}
}
