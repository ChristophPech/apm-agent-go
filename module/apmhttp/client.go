package apmhttp

import (
	"io"
	"net/http"
	"sync/atomic"
	"unsafe"

	"go.elastic.co/apm"
)

// WrapClient returns a new *http.Client with all fields copied
// across, and the Transport field wrapped with WrapRoundTripper
// such that client requests are reported as spans to Elastic APM
// if their context contains a sampled transaction.
//
// If c is nil, then http.DefaultClient is wrapped.
func WrapClient(c *http.Client, o ...ClientOption) *http.Client {
	if c == nil {
		c = http.DefaultClient
	}
	copied := *c
	copied.Transport = WrapRoundTripper(copied.Transport, o...)
	return &copied
}

// WrapRoundTripper returns an http.RoundTripper wrapping r, reporting each
// request as a span to Elastic APM, if the request's context contains a
// sampled transaction.
//
// If r is nil, then http.DefaultTransport is wrapped.
func WrapRoundTripper(r http.RoundTripper, o ...ClientOption) http.RoundTripper {
	if r == nil {
		r = http.DefaultTransport
	}
	rt := &roundTripper{
		r:              r,
		requestName:    ClientRequestName,
		requestIgnorer: IgnoreNone,
	}
	for _, o := range o {
		o(rt)
	}
	return rt
}

type roundTripper struct {
	r              http.RoundTripper
	requestName    RequestNameFunc
	requestIgnorer RequestIgnorerFunc
}

// RoundTrip delegates to r.r, emitting a span if req's context
// contains a transaction.
func (r *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// TODO(axw) propagate Tracestate, adding/shifting the elastic
	// key to the left most position.
	if r.requestIgnorer(req) {
		return r.r.RoundTrip(req)
	}
	ctx := req.Context()
	tx := apm.TransactionFromContext(ctx)
	if tx == nil {
		return r.r.RoundTrip(req)
	}

	// RoundTrip is not supposed to mutate req, so copy req
	// and set the trace-context headers only in the copy.
	reqCopy := *req
	reqCopy.Header = make(http.Header, len(req.Header))
	for k, v := range req.Header {
		reqCopy.Header[k] = v
	}
	req = &reqCopy

	traceContext := tx.TraceContext()
	if !traceContext.Options.Recorded() {
		req.Header.Set(TraceparentHeader, FormatTraceparentHeader(traceContext))
		return r.r.RoundTrip(req)
	}

	name := r.requestName(req)
	span := tx.StartSpan(name, "external.http", apm.SpanFromContext(ctx))
	span.Context.SetHTTPRequest(req)
	if !span.Dropped() {
		traceContext = span.TraceContext()
		ctx = apm.ContextWithSpan(ctx, span)
		req = RequestWithContext(ctx, req)
	} else {
		span.End()
		span = nil
	}

	req.Header.Set(TraceparentHeader, FormatTraceparentHeader(traceContext))
	resp, err := r.r.RoundTrip(req)
	if span != nil {
		if err != nil {
			span.End()
		} else {
			resp.Body = &responseBody{span: span, body: resp.Body}
		}
	}
	return resp, err
}

type responseBody struct {
	span *apm.Span
	body io.ReadCloser
}

// Close closes the response body, and ends the span if it hasn't already been ended.
func (b *responseBody) Close() error {
	b.endSpan()
	return b.body.Close()
}

// Read reads from the response body, and ends the span when io.EOF is returend if
// the span hasn't already been ended.
func (b *responseBody) Read(p []byte) (n int, err error) {
	n, err = b.body.Read(p)
	if err == io.EOF {
		b.endSpan()
	}
	return n, err
}

func (b *responseBody) endSpan() {
	addr := (*unsafe.Pointer)(unsafe.Pointer(&b.span))
	if old := atomic.SwapPointer(addr, nil); old != nil {
		(*apm.Span)(old).End()
	}
}

// ClientOption sets options for tracing client requests.
type ClientOption func(*roundTripper)
