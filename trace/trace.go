package trace

import (
	"context"
	"fmt"

	"github.com/golangid/graphql-go/errors"
	"github.com/golangid/graphql-go/introspection"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	"github.com/opentracing/opentracing-go/log"
)

type TraceQueryFinishFunc func([]byte, []*errors.QueryError)
type TraceFieldFinishFunc func([]byte, *errors.QueryError)

type Tracer interface {
	TraceQuery(ctx context.Context, queryString string, operationName string, variables map[string]interface{}, varTypes map[string]*introspection.Type) (context.Context, TraceQueryFinishFunc)
	TraceField(ctx context.Context, label, typeName, fieldName string, trivial bool, args map[string]interface{}) (context.Context, TraceFieldFinishFunc)
}

type OpenTracingTracer struct{}

func (OpenTracingTracer) TraceQuery(ctx context.Context, queryString string, operationName string, variables map[string]interface{}, varTypes map[string]*introspection.Type) (context.Context, TraceQueryFinishFunc) {
	span, spanCtx := opentracing.StartSpanFromContext(ctx, "GraphQL request")
	span.SetTag("graphql.query", queryString)

	if operationName != "" {
		span.SetTag("graphql.operationName", operationName)
	}

	if len(variables) != 0 {
		span.LogFields(log.Object("graphql.variables", variables))
	}

	return spanCtx, func(data []byte, errs []*errors.QueryError) {
		if len(errs) > 0 {
			msg := errs[0].Error()
			if len(errs) > 1 {
				msg += fmt.Sprintf(" (and %d more errors)", len(errs)-1)
			}
			ext.Error.Set(span, true)
			span.SetTag("graphql.error", msg)
		}
		span.Finish()
	}
}

func (OpenTracingTracer) TraceField(ctx context.Context, label, typeName, fieldName string, trivial bool, args map[string]interface{}) (context.Context, TraceFieldFinishFunc) {
	if trivial {
		return ctx, noop
	}

	span, spanCtx := opentracing.StartSpanFromContext(ctx, label)
	span.SetTag("graphql.type", typeName)
	span.SetTag("graphql.field", fieldName)
	for name, value := range args {
		span.SetTag("graphql.args."+name, value)
	}

	return spanCtx, func(data []byte, err *errors.QueryError) {
		if err != nil {
			ext.Error.Set(span, true)
			span.SetTag("graphql.error", err.Error())
		}
		span.Finish()
	}
}

func noop([]byte, *errors.QueryError) {}

type NoopTracer struct{}

func (NoopTracer) TraceQuery(ctx context.Context, queryString string, operationName string, variables map[string]interface{}, varTypes map[string]*introspection.Type) (context.Context, TraceQueryFinishFunc) {
	return ctx, func(data []byte, errs []*errors.QueryError) {}
}

func (NoopTracer) TraceField(ctx context.Context, label, typeName, fieldName string, trivial bool, args map[string]interface{}) (context.Context, TraceFieldFinishFunc) {
	return ctx, func(data []byte, err *errors.QueryError) {}
}
