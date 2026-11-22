// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package app

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/aozmekik/fibonacci-service/internal/version"
)

// newRegistry returns a registry with the metrics of the process itself.
func newRegistry() *prometheus.Registry {
	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "fibonacci_service_build_info",
		Help:        "Build information of the service, the value is always 1.",
		ConstLabels: prometheus.Labels{"version": version.Tag, "revision": version.SHA},
	})
	buildInfo.Set(1)

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		buildInfo,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return registry
}

// apiMetrics are the metrics of the HTTP API.
//
// They have no label for the path or for n, as both are chosen by the client.
type apiMetrics struct {
	inFlight prometheus.Gauge
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	size     *prometheus.HistogramVec
	aborted  prometheus.Counter
	rejected prometheus.Counter
}

func newAPIMetrics(registerer prometheus.Registerer) *apiMetrics {
	metrics := &apiMetrics{
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "fibonacci_service_http_requests_in_flight",
			Help: "Number of HTTP requests being served.",
		}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fibonacci_service_http_requests_total",
			Help: "Number of HTTP requests served, by status code.",
		}, []string{"code"}),
		// The handler does no work to speak of, so the duration is the time the client takes to read the response.
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "fibonacci_service_http_request_duration_seconds",
			Help:    "Time taken to serve an HTTP request.",
			Buckets: []float64{.001, .005, .025, .1, .5, 2.5, 10, 30},
		}, nil),
		// The size of a response grows with the square of n, so it tells how the limit on n is used.
		size: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "fibonacci_service_http_response_size_bytes",
			Help:    "Size of the body of an HTTP response.",
			Buckets: prometheus.ExponentialBuckets(256, 4, 10),
		}, nil),
		// Such a response is counted with the status it was started with, which is a success.
		// A write fails only while the rest of the response does not fit into the buffers of the
		// connection, so a client which leaves after that is not seen here.
		aborted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "fibonacci_service_http_responses_aborted_total",
			Help: "Number of HTTP responses which could not be written in full.",
		}),
		// Such a connection is closed before a request is read from it, so nothing else counts it.
		// It is the same counter for the API and the metrics.
		rejected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "fibonacci_service_http_connections_rejected_total",
			Help: "Number of connections closed as soon as they were accepted, as the server had its limit of connections open.",
		}),
	}

	registerer.MustRegister(metrics.inFlight, metrics.requests, metrics.duration, metrics.size, metrics.aborted, metrics.rejected)

	return metrics
}

// instrument wraps the handler of the API with the metrics which are taken from the outside of it.
func (metrics *apiMetrics) instrument(handler http.Handler) http.Handler {
	return promhttp.InstrumentHandlerInFlight(metrics.inFlight,
		promhttp.InstrumentHandlerCounter(metrics.requests,
			promhttp.InstrumentHandlerDuration(metrics.duration,
				promhttp.InstrumentHandlerResponseSize(metrics.size, handler),
			),
		),
	)
}
