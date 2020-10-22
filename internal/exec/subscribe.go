package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/golangid/graphql-go/errors"
	"github.com/golangid/graphql-go/internal/common"
	"github.com/golangid/graphql-go/internal/exec/resolvable"
	"github.com/golangid/graphql-go/internal/exec/selected"
	"github.com/golangid/graphql-go/internal/query"
	"github.com/golangid/graphql-go/internal/schema"
)

type Response struct {
	Data   json.RawMessage
	Errors []*errors.QueryError
}

func (r *Request) Subscribe(ctx context.Context, s *resolvable.Schema, op *query.Operation) (respChan <-chan *Response) {
	var result reflect.Value
	var err *errors.QueryError
	sels := selected.ApplyOperation(&r.Request, s, op)

	f := r.subscriptionSearchFieldMethod(ctx, sels, nil, s, s.ResolverSubscription)
	if f == nil {
		return sendAndReturnClosed(&Response{Errors: []*errors.QueryError{err}})
	}

	defer func() {
		if panicValue := recover(); panicValue != nil {
			r.Logger.LogPanic(ctx, panicValue)
			if errs, ok := panicValue.(*errors.QueryError); ok {
				err = errs
			} else {
				err = makePanicError(panicValue)
			}
			respChan = sendAndReturnClosed(&Response{Errors: []*errors.QueryError{err}})
		}
	}()

	var in []reflect.Value
	if f.field.HasContext {
		in = append(in, reflect.ValueOf(ctx))
	}
	if f.field.ArgsPacker != nil {
		in = append(in, f.field.PackedArgs)
	}
	callOut := f.resolver.Method(f.field.MethodIndex).Call(in)
	result = callOut[0]

	if f.field.HasError && !callOut[1].IsNil() {
		resolverErr := callOut[1].Interface().(error)
		err = errors.Errorf("%s", resolverErr)
		err.ResolverError = resolverErr
	}

	if err != nil {
		if _, nonNullChild := f.field.Type.(*common.NonNull); nonNullChild {
			return sendAndReturnClosed(&Response{Errors: []*errors.QueryError{err}})
		}
		return sendAndReturnClosed(&Response{Data: []byte(fmt.Sprintf(`{"%s":null}`, f.field.Alias)), Errors: []*errors.QueryError{err}})
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return sendAndReturnClosed(&Response{Errors: []*errors.QueryError{errors.Errorf("%s", ctxErr)}})
	}

	c := make(chan *Response)
	// TODO: handle resolver nil channel better?
	if result == reflect.Zero(result.Type()) {
		close(c)
		return c
	}

	go func() {
		for {
			// Check subscription context
			chosen, resp, ok := reflect.Select([]reflect.SelectCase{
				{
					Dir:  reflect.SelectRecv,
					Chan: reflect.ValueOf(ctx.Done()),
				},
				{
					Dir:  reflect.SelectRecv,
					Chan: result,
				},
			})
			switch chosen {
			// subscription context done
			case 0:
				close(c)
				return
			// upstream received
			case 1:
				// upstream closed
				if !ok {
					close(c)
					return
				}

				subR := &Request{
					Request: selected.Request{
						Doc:    r.Request.Doc,
						Vars:   r.Request.Vars,
						Schema: r.Request.Schema,
					},
					Limiter: r.Limiter,
					Tracer:  r.Tracer,
					Logger:  r.Logger,
				}
				var out bytes.Buffer
				func() {
					// TODO: configurable timeout
					subCtx, cancel := context.WithTimeout(ctx, time.Second)
					defer cancel()

					// resolve response
					func() {
						defer subR.handlePanic(subCtx)

						var buf bytes.Buffer
						subR.execSelectionSet(subCtx, f.sels, f.field.Type, &pathSegment{nil, f.field.Alias}, s, resp, &buf)

						propagateChildError := false
						if _, nonNullChild := f.field.Type.(*common.NonNull); nonNullChild && resolvedToNull(&buf) {
							propagateChildError = true
						}

						if !propagateChildError {
							out.WriteString(fmt.Sprintf(`{"%s":`, f.field.Alias))
							out.Write(buf.Bytes())
							out.WriteString(`}`)
						}
					}()

					if err := subCtx.Err(); err != nil {
						c <- &Response{Errors: []*errors.QueryError{errors.Errorf("%s", err)}}
						return
					}

					// Send response within timeout
					// TODO: maybe block until sent?
					select {
					case <-subCtx.Done():
					case c <- &Response{Data: out.Bytes(), Errors: subR.Errs}:
					}
				}()
			}
		}
	}()

	return c
}

func sendAndReturnClosed(resp *Response) chan *Response {
	c := make(chan *Response, 1)
	c <- resp
	close(c)
	return c
}

func (r *Request) subscriptionSearchFieldMethod(ctx context.Context, sels []selected.Selection, path *pathSegment, s *resolvable.Schema, resolver reflect.Value) (foundField *fieldToExec) {

	var collectFields func(sels []selected.Selection, path *pathSegment, s *resolvable.Schema, resolver reflect.Value)
	var execField func(s *resolvable.Schema, f *fieldToExec, path *pathSegment)
	var execSelectionSet func(sels []selected.Selection, typ common.Type, path *pathSegment, s *resolvable.Schema, resolver reflect.Value)

	collectFields = func(sels []selected.Selection, path *pathSegment, s *resolvable.Schema, resolver reflect.Value) {
		var fields []*fieldToExec
		collectFieldsToResolve(sels, s, resolver, &fields, make(map[string]*fieldToExec))

		for _, f := range fields {
			execField(s, f, &pathSegment{path, f.field.Alias})
			if f.field.UseMethodResolver() && !f.field.FixedResult.IsValid() {
				foundField = f
				return
			}
		}
	}

	execField = func(s *resolvable.Schema, f *fieldToExec, path *pathSegment) {
		var result reflect.Value

		if f.field.FixedResult.IsValid() {
			result = f.field.FixedResult
			return
		}

		res := f.resolver
		if !f.field.UseMethodResolver() {
			res = unwrapPtr(res)
			result = res.FieldByIndex(f.field.FieldIndex)
		}

		execSelectionSet(f.sels, f.field.Type, path, s, result)
	}

	execSelectionSet = func(sels []selected.Selection, typ common.Type, path *pathSegment, s *resolvable.Schema, resolver reflect.Value) {
		t, nonNull := unwrapNonNull(typ)
		switch t := t.(type) {
		case *schema.Object, *schema.Interface, *schema.Union:
			if resolver.Kind() == reflect.Invalid || ((resolver.Kind() == reflect.Ptr || resolver.Kind() == reflect.Interface) && resolver.IsNil()) {
				if nonNull {
					err := errors.Errorf("graphql: got nil for non-null %q", t)
					err.Path = path.toSlice()
					r.AddError(err)
				}
				return
			}

			collectFields(sels, path, s, resolver)
			return
		}
	}

	collectFields(sels, path, s, resolver)
	return
}

func unwrapPtr(v reflect.Value) reflect.Value {
	if v.Kind() == reflect.Ptr {
		return v.Elem()
	}
	return v
}
