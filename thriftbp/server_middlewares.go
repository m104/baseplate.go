package thriftbp

import (
	"context"

	"github.com/apache/thrift/lib/go/thrift"

	"github.com/reddit/baseplate.go/edgecontext"
	"github.com/reddit/baseplate.go/log"
	"github.com/reddit/baseplate.go/tracing"
)

// BaseplateProcessor is a TProcessor that can be thriftbp.WrapProcessor-ed and
// thriftbp.Merge-d.
//
// The TProcessors generated by the Apache Thrift compiler fufill this
// interface, but not all of them are a part of any interface within Apache
// Thrift.
type BaseplateProcessor interface {
	thrift.TProcessor

	// ProcessorMap returns a map of thrift method names to TProcessorFunctions.
	ProcessorMap() map[string]thrift.TProcessorFunction

	// AddToProcessorMap adds the given TProcessorFunction to the internal
	// processor map at the given key.
	//
	// If one is already set at the given key, it will be replaced with the new
	// TProcessorFunction.
	AddToProcessorMap(string, thrift.TProcessorFunction)
}

// ProcessorMiddleware is a function that can be passed to WrapProcessor to wrap
// the TProcessorFunctions for that TProcessor.
//
// Middlewares are passed in the name of the function as set in the processor
// map of the TProcessor and a logger that can be used by the ProcessorMiddleware.
type ProcessorMiddleware func(name string, next thrift.TProcessorFunction) thrift.TProcessorFunction

// WrappedTProcessorFunc is a conveinence struct that implements the
// TProcessorFunction interface that you can pass in a wrapped function that
// will be called by Process.
type WrappedTProcessorFunc struct {
	// Wrapped is called by WrappedTProcessorFunc and should be a "wrapped"
	// call to a base TProcessorFunc.Process call.
	Wrapped func(ctx context.Context, seqId int32, in, out thrift.TProtocol) (bool, thrift.TException)
}

// Process implements the TProcessorFunction interface by calling and returning
// p.Wrapped.
func (p WrappedTProcessorFunc) Process(ctx context.Context, seqID int32, in, out thrift.TProtocol) (bool, thrift.TException) {
	return p.Wrapped(ctx, seqID, in, out)
}

var (
	_ thrift.TProcessorFunction = WrappedTProcessorFunc{}
	_ thrift.TProcessorFunction = (*WrappedTProcessorFunc)(nil)
)

// WrapProcessor takes an existing BaseplateProcessor and wraps each of its inner
// TProcessorFunctions with the middlewares passed in and returns it.
//
// Middlewares will be called in the order that they are defined:
//
//		1. Middlewares[0]
//		2. Middlewares[1]
//		...
//		N. Middlewares[n]
//
// It is recomended that you pass in tracing.InjectServerSpan and the
// ProcessorMiddleware returned by edgecontext.InjectEdgeContext as the first two
// middlewares.
func WrapProcessor(processor BaseplateProcessor, middlewares ...ProcessorMiddleware) thrift.TProcessor {
	for name, processorFunc := range processor.ProcessorMap() {
		wrapped := processorFunc
		for i := len(middlewares) - 1; i >= 0; i-- {
			wrapped = middlewares[i](name, wrapped)
		}
		processor.AddToProcessorMap(name, wrapped)
	}
	return processor
}

// StartSpanFromThriftContext creates a server span from thrift context object.
//
// This span would usually be used as the span of the whole thrift endpoint
// handler, and the parent of the child-spans.
//
// Caller should pass in the context object they got from thrift library,
// which would have all the required headers already injected.
//
// Please note that "Sampled" header is default to false according to baseplate
// spec, so if the context object doesn't have headers injected correctly,
// this span (and all its child-spans) will never be sampled,
// unless debug flag was set explicitly later.
//
// If any of the tracing related thrift header is present but malformed,
// it will be ignored.
// The error will also be logged if InitGlobalTracer was last called with a
// non-nil logger.
// Absent tracing related headers are always silently ignored.
func StartSpanFromThriftContext(ctx context.Context, name string) (context.Context, *tracing.Span) {
	var headers tracing.Headers
	var sampled bool

	if str, ok := thrift.GetHeader(ctx, HeaderTracingTrace); ok {
		headers.TraceID = str
	}
	if str, ok := thrift.GetHeader(ctx, HeaderTracingSpan); ok {
		headers.SpanID = str
	}
	if str, ok := thrift.GetHeader(ctx, HeaderTracingFlags); ok {
		headers.Flags = str
	}
	if str, ok := thrift.GetHeader(ctx, HeaderTracingSampled); ok {
		sampled = str == HeaderTracingSampledTrue
		headers.Sampled = &sampled
	}

	return tracing.StartSpanFromHeaders(ctx, name, headers)
}

// InjectServerSpan implements thriftbp.ProcessorMiddleware and injects a server
// span into the `next` context.
//
// Starts the server span before calling the `next` TProcessorFunction and stops
// the span after it finishes.
// If the function returns an error, that will be passed to span.Stop.
//
// Note, the span will be created according to tracing related headers already
// being set on the context object.
// These should be automatically injected by your thrift.TSimpleServer.
func InjectServerSpan(name string, next thrift.TProcessorFunction) thrift.TProcessorFunction {
	return WrappedTProcessorFunc{
		Wrapped: func(ctx context.Context, seqId int32, in, out thrift.TProtocol) (success bool, err thrift.TException) {
			ctx, span := StartSpanFromThriftContext(ctx, name)
			defer func() {
				span.FinishWithOptions(tracing.FinishOptions{
					Ctx: ctx,
					Err: err,
				}.Convert())
			}()

			return next.Process(ctx, seqId, in, out)
		},
	}
}

var (
	_ ProcessorMiddleware = InjectServerSpan
)

// InitializeEdgeContext sets an edge request context created from the Thrift
// headers set on the context onto the context and configures Thrift to forward
// the edge requent context header on any Thrift calls made by the server.
func InitializeEdgeContext(ctx context.Context, impl *edgecontext.Impl) context.Context {
	header, ok := thrift.GetHeader(ctx, HeaderEdgeRequest)
	if !ok {
		return ctx
	}

	ec, err := edgecontext.FromHeader(header, impl)
	if err != nil {
		log.Error("Error while parsing EdgeRequestContext: " + err.Error())
		return ctx
	}
	if ec == nil {
		return ctx
	}

	return edgecontext.SetEdgeContext(ctx, ec)
}

// InjectEdgeContext returns a ProcessorMiddleware that injects an edge request
// context created from the Thrift headers set on the context into the `next`
// thrift.TProcessorFunction.
//
// Note, this depends on the edge context headers already being set on the
// context object.  These should be automatically injected by your
// thrift.TSimpleServer.
func InjectEdgeContext(impl *edgecontext.Impl) ProcessorMiddleware {
	return func(name string, next thrift.TProcessorFunction) thrift.TProcessorFunction {
		return WrappedTProcessorFunc{
			Wrapped: func(ctx context.Context, seqId int32, in, out thrift.TProtocol) (bool, thrift.TException) {
				ctx = InitializeEdgeContext(ctx, impl)
				return next.Process(ctx, seqId, in, out)
			},
		}
	}
}
