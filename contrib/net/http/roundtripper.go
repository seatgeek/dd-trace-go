// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-2020 Datadog, Inc.

package http

import (
	"context"
	"crypto/tls"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"strconv"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/ext"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
)

const defaultResourceName = "http.request"

type roundTripper struct {
	base http.RoundTripper
	cfg  *roundTripperConfig
}

func (rt *roundTripper) RoundTrip(req *http.Request) (res *http.Response, err error) {
	opts := []ddtrace.StartSpanOption{
		tracer.SpanType(ext.SpanTypeHTTP),
		tracer.ResourceName(defaultResourceName),
		tracer.Tag(ext.HTTPMethod, req.Method),
		tracer.Tag(ext.HTTPURL, req.URL.String()),
		tracer.Tag("http.path", req.URL.Path),
	}
	if !math.IsNaN(rt.cfg.analyticsRate) {
		opts = append(opts, tracer.Tag(ext.EventSampleRate, rt.cfg.analyticsRate))
	}
	if rt.cfg.serviceName != "" {
		opts = append(opts, tracer.ServiceName(rt.cfg.serviceName))
	}
	span, ctx := tracer.StartSpanFromContext(req.Context(), defaultResourceName, opts...)
	defer func() {
		if rt.cfg.after != nil {
			rt.cfg.after(res, span)
		}
		span.Finish(tracer.WithError(err))
	}()
	if rt.cfg.before != nil {
		rt.cfg.before(req, span)
	}

	// Inject Go's "httptrace" context into the request
	var httpTraceResult httpTraceResult
	ctx = WithClientTrace(ctx, &httpTraceResult)

	err = tracer.Inject(span.Context(), tracer.HTTPHeadersCarrier(req.Header))
	if err != nil {
		fmt.Fprintf(os.Stderr, "contrib/net/http.Roundtrip: failed to inject http headers: %v\n", err)
	}

	res, err = rt.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		span.SetTag("http.errors", err.Error())
	} else {
		span.SetTag(ext.HTTPCode, strconv.Itoa(res.StatusCode))
		span.SetTag("network.destination.ip", httpTraceResult.remoteIP)
		span.SetTag("network.destination.port", httpTraceResult.remotePort)
		span.SetTag("http.content_type", res.Header.Get("Content-Type"))
		span.SetTag("http.connect_time", httpTraceResult.Connect.Nanoseconds())
		span.SetTag("http.dns_lookup_time", httpTraceResult.DNSLookup.Nanoseconds())
		span.SetTag("http.pretransfer_time", httpTraceResult.Pretransfer.Nanoseconds())
		span.SetTag("http.starttransfer_time", httpTraceResult.StartTransfer.Nanoseconds())
		span.SetTag("http.is_tls", httpTraceResult.isTLS)
		span.SetTag("http.is_reused", httpTraceResult.isReused)

		if httpTraceResult.isTLS {
			span.SetTag("http.tls_handshake_time", httpTraceResult.TLSHandshake.Nanoseconds())
		}

		// treat 5XX as errors
		if res.StatusCode/100 == 5 {
			span.SetTag("http.errors", res.Status)
		}
	}

	return res, err
}

// WrapRoundTripper returns a new RoundTripper which traces all requests sent
// over the transport.
func WrapRoundTripper(rt http.RoundTripper, opts ...RoundTripperOption) http.RoundTripper {
	cfg := newRoundTripperConfig()
	for _, opt := range opts {
		opt(cfg)
	}
	if wrapped, ok := rt.(*roundTripper); ok {
		rt = wrapped.base
	}
	return &roundTripper{
		base: rt,
		cfg:  cfg,
	}
}

// WrapClient modifies the given client's transport to augment it with tracing and returns it.
func WrapClient(c *http.Client, opts ...RoundTripperOption) *http.Client {
	if c.Transport == nil {
		c.Transport = http.DefaultTransport
	}
	c.Transport = WrapRoundTripper(c.Transport, opts...)
	return c
}

type httpTraceResult struct {
	DNSLookup        time.Duration
	TCPConnection    time.Duration
	TLSHandshake     time.Duration
	ServerProcessing time.Duration
	NameLookup       time.Duration
	Connect          time.Duration
	Pretransfer      time.Duration
	StartTransfer    time.Duration
	dnsStart         time.Time
	dnsDone          time.Time
	tcpStart         time.Time
	tcpDone          time.Time
	tlsStart         time.Time
	tlsDone          time.Time
	serverStart      time.Time
	serverDone       time.Time
	transferStart    time.Time
	isTLS            bool
	isReused         bool
	remoteIP         string
	remotePort       string
}

func WithClientTrace(ctx context.Context, r *httpTraceResult) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		DNSStart: func(i httptrace.DNSStartInfo) {
			r.dnsStart = time.Now()
		},
		DNSDone: func(i httptrace.DNSDoneInfo) {
			r.dnsDone = time.Now()
			r.DNSLookup = r.dnsDone.Sub(r.dnsStart)
			r.NameLookup = r.dnsDone.Sub(r.dnsStart)
		},
		ConnectStart: func(_, _ string) {
			r.tcpStart = time.Now()
			if r.dnsStart.IsZero() {
				r.dnsStart = r.tcpStart
				r.dnsDone = r.tcpStart
			}
		},
		ConnectDone: func(network, addr string, err error) {
			r.tcpDone = time.Now()
			r.TCPConnection = r.tcpDone.Sub(r.tcpStart)
			r.Connect = r.tcpDone.Sub(r.dnsStart)
		},
		TLSHandshakeStart: func() {
			r.isTLS = true
			r.tlsStart = time.Now()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			r.tlsDone = time.Now()
			r.TLSHandshake = r.tlsDone.Sub(r.tlsStart)
			r.Pretransfer = r.tlsDone.Sub(r.dnsStart)
		},
		GotConn: func(i httptrace.GotConnInfo) {
			if i.Reused {
				r.isReused = true
			}

			r.remoteIP, r.remotePort, _ = net.SplitHostPort(i.Conn.RemoteAddr().String())
		},

		WroteRequest: func(info httptrace.WroteRequestInfo) {
			r.serverStart = time.Now()
			if r.dnsStart.IsZero() && r.tcpStart.IsZero() {
				now := r.serverStart
				r.dnsStart = now
				r.dnsDone = now
				r.tcpStart = now
				r.tcpDone = now
			}

			if r.isReused {
				now := r.serverStart
				r.dnsStart = now
				r.dnsDone = now
				r.tcpStart = now
				r.tcpDone = now
				r.tlsStart = now
				r.tlsDone = now
			}

			if r.isTLS {
				return
			}

			r.TLSHandshake = r.tcpDone.Sub(r.tcpDone)
			r.Pretransfer = r.Connect
		},

		GotFirstResponseByte: func() {
			r.serverDone = time.Now()
			r.ServerProcessing = r.serverDone.Sub(r.serverStart)
			r.StartTransfer = r.serverDone.Sub(r.dnsStart)
			r.transferStart = r.serverDone
		},
	})
}
