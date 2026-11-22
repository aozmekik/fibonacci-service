// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package app_test

import (
	"bufio"
	"context"
	"io"
	"maps"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil/promlint"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aozmekik/fibonacci-service/internal/app"
	"github.com/aozmekik/fibonacci-service/internal/fibonacci"
)

// TestRun runs the service the way the binary does and talks to it over HTTP.
func TestRun(t *testing.T) {
	t.Parallel()

	address, metricsAddress := freeListenAddress(t), freeListenAddress(t)

	stop := start(t, address, "--metrics-addr="+metricsAddress, "--max-n=100")

	status, numbers := get(t, "http://"+address+"/v1/fibonacci?n=5")

	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "[0,1,1,2,3]", numbers)

	status, negative := get(t, "http://"+address+"/v1/fibonacci?n=-1")

	assert.Equal(t, http.StatusBadRequest, status)
	assert.JSONEq(t, `{"code": "invalid_argument", "message": "n must not be negative"}`, negative)

	// HEAD gets the headers of GET and no body, which only a real server shows
	status, header, body := do(t, t.Context(), http.MethodHead, "http://"+address+"/v1/fibonacci?n=5", nil)

	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "11", header.Get("Content-Length"))
	assert.Empty(t, body)

	// another spelling of the path is not redirected to it, which the client would have followed
	status, notFound := get(t, "http://"+address+"//v1/fibonacci?n=5")

	assert.Equal(t, http.StatusNotFound, status)
	assert.JSONEq(t, `{"code": "not_found", "message": "no such path"}`, notFound)

	// net/http turns down a request with headers above the limit on its own, before the API and its metrics see it
	status, _, _ = do(t, t.Context(), http.MethodGet, "http://"+address+"/v1/fibonacci?n=5", http.Header{"X-Padding": {strings.Repeat("a", 16<<10)}})

	assert.Equal(t, http.StatusRequestHeaderFieldsTooLarge, status)

	// the health check answers whatever the method is, and it is not counted either
	status, _, body = do(t, t.Context(), http.MethodPost, "http://"+address+"/healthz", nil)

	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "ok\n", body)

	// another spelling of its path is not the health check, the same way as with the path of the API
	status, escaped := get(t, "http://"+address+"/%68ealthz")

	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, notFound, escaped)

	metrics := scrape(t, metricsAddress)

	assert.Contains(t, metrics, `fibonacci_service_http_requests_total{code="200"} 2`+"\n")
	assert.Contains(t, metrics, `fibonacci_service_http_requests_total{code="400"} 1`+"\n")
	assert.Contains(t, metrics, `fibonacci_service_http_requests_total{code="404"} 2`+"\n")
	assert.NotContains(t, metrics, `code="431"`)
	// the bodies of the four GET requests, and nothing for HEAD
	assert.Contains(t, metrics, "fibonacci_service_http_response_size_bytes_sum "+strconv.Itoa(len(numbers)+len(negative)+len(notFound)+len(escaped))+"\n")
	assert.Contains(t, metrics, "fibonacci_service_http_response_size_bytes_count 5\n")
	assert.Contains(t, metrics, "fibonacci_service_http_request_duration_seconds_count 5\n")
	assert.Contains(t, metrics, "fibonacci_service_http_responses_aborted_total 0\n")
	assert.Contains(t, metrics, "fibonacci_service_build_info")
	assert.Contains(t, metrics, "go_goroutines")

	problems, err := promlint.New(strings.NewReader(metrics)).Lint()
	require.NoError(t, err)
	assert.Empty(t, problems)

	// the metrics are not served along with the API
	status, _ = get(t, "http://"+address+"/metrics")

	assert.Equal(t, http.StatusNotFound, status)

	require.NoError(t, stop())
}

// TestRunShutdown checks that a response which is being sent when the shutdown starts is sent in full,
// and that the metrics are served until then.
//
// The response is the largest one there is, far more than the buffers between the server
// and a client which does not read can hold, so the server is still writing it.
func TestRunShutdown(t *testing.T) {
	t.Parallel()

	address, metricsAddress := freeListenAddress(t), freeListenAddress(t)

	stop := start(t, address, "--metrics-addr="+metricsAddress, "--max-n="+strconv.Itoa(fibonacci.MaxLen))

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+"/v1/fibonacci?n="+strconv.Itoa(fibonacci.MaxLen), nil)
	require.NoError(t, err)

	// Do returns once the headers are in, the body is read below
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer response.Body.Close() //nolint:errcheck

	stopped := make(chan error, 1)

	go func() { stopped <- stop() }()

	// the shutdown has started when the server does not accept connections anymore
	require.Eventually(t, func() bool {
		conn, dialErr := (&net.Dialer{}).DialContext(t.Context(), "tcp", address)
		if dialErr != nil {
			return true
		}

		conn.Close() //nolint:errcheck

		return false
	}, 10*time.Second, 10*time.Millisecond)

	// the API is not done yet, so the metrics are still there, and they show the request
	metrics := scrape(t, metricsAddress)

	assert.Contains(t, metrics, "fibonacci_service_http_requests_in_flight 1\n")

	select {
	case err = <-stopped:
		require.FailNow(t, "the service stopped before the response was read", "error: %v", err)
	default:
	}

	size, err := io.Copy(io.Discard, response.Body)
	require.NoError(t, err)

	assert.Equal(t, response.ContentLength, size)
	assert.Positive(t, size)

	require.NoError(t, <-stopped)
}

// TestRunShutdownTimeout checks that a client which never reads its response does not hold the shutdown back,
// and that its connection is closed at the end of it.
func TestRunShutdownTimeout(t *testing.T) {
	t.Parallel()

	address := freeListenAddress(t)

	stop := start(t, address, "--metrics-addr=", "--max-n="+strconv.Itoa(fibonacci.MaxLen), "--shutdown-timeout=200ms")

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+"/v1/fibonacci?n="+strconv.Itoa(fibonacci.MaxLen), nil)
	require.NoError(t, err)

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer response.Body.Close() //nolint:errcheck

	err = stop()

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	// the client is not left with a response which never ends
	_, err = io.Copy(io.Discard, response.Body)

	require.Error(t, err)
}

// TestRunAbortedResponse checks that a response the client did not wait for is counted.
//
// The request itself is counted as a success, as the status was sent before the client left.
func TestRunAbortedResponse(t *testing.T) {
	t.Parallel()

	address, metricsAddress := freeListenAddress(t), freeListenAddress(t)

	stop := start(t, address, "--metrics-addr="+metricsAddress, "--max-n="+strconv.Itoa(fibonacci.MaxLen))

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+"/v1/fibonacci?n="+strconv.Itoa(fibonacci.MaxLen), nil)
	require.NoError(t, err)

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	// closing a body which was not read closes the connection, while the server is still writing to it
	response.Body.Close() //nolint:errcheck

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		_, _, metrics := do(collect, t.Context(), http.MethodGet, "http://"+metricsAddress+"/metrics", nil)

		assert.Contains(collect, metrics, "fibonacci_service_http_responses_aborted_total 1\n")
		assert.Contains(collect, metrics, `fibonacci_service_http_requests_total{code="200"} 1`+"\n")
	}, 10*time.Second, 50*time.Millisecond)

	require.NoError(t, stop())
}

// TestRunAddressInUse checks that the service fails when one of its addresses is taken,
// and that the server which did start is shut down again.
func TestRunAddressInUse(t *testing.T) {
	t.Parallel()

	address, taken := freeListenAddress(t), freeListenAddress(t)

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", taken)
	require.NoError(t, err)

	defer listener.Close() //nolint:errcheck

	err = app.Run(t.Context(), []string{"--listen-addr=" + address, "--metrics-addr=" + taken, "--log-level=error"})

	require.ErrorContains(t, err, `failed to serve on "`+taken+`"`)

	// the API may have started to listen before the metrics failed to
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", address)
	if err == nil {
		conn.Close() //nolint:errcheck
	}

	require.Error(t, err, "the API still listens")
}

// TestRunMaxConnections checks that a connection over the limit is closed right away and counted,
// and that a new one is served again once another one is closed.
func TestRunMaxConnections(t *testing.T) {
	t.Parallel()

	address, metricsAddress := freeListenAddress(t), freeListenAddress(t)

	stop := start(t, address, "--metrics-addr="+metricsAddress, "--max-connections=1")

	// HTTP/1.1 keeps the connection open after the response, so it holds the only place
	first := servedConnection(t, address)
	rejected := rejectedConnections(t, metricsAddress)

	_, err := healthCheck(dial(t, address), 10*time.Second)
	require.Error(t, err)

	assert.Equal(t, rejected+1, rejectedConnections(t, metricsAddress))

	require.NoError(t, first.Close())

	servedConnection(t, address)

	require.NoError(t, stop())
}

func TestRunInvalidArguments(t *testing.T) {
	t.Parallel()

	// The context is canceled already and the address is a free one, so arguments which are
	// accepted by mistake make Run return without an error, instead of serving until the test
	// times out. Each case looks for its own error, as Run can fail for other reasons too.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	for _, test := range []struct {
		name     string
		expected string
		args     []string
	}{
		{name: "unknown flag", args: []string{"--unknown"}, expected: "flag provided but not defined: -unknown"},
		{name: "positional argument", args: []string{"1000"}, expected: `unexpected argument "1000"`},
		{name: "negative max-n", args: []string{"--max-n=-1"}, expected: "invalid max-n"},
		{name: "max-n above the limit", args: []string{"--max-n=" + strconv.Itoa(fibonacci.MaxLen+1)}, expected: "invalid max-n"},
		{name: "max-n not a number", args: []string{"--max-n=many"}, expected: `invalid value "many" for flag -max-n`},
		{name: "empty listen address", args: []string{"--listen-addr="}, expected: "invalid listen address"},
		{name: "zero max-connections", args: []string{"--max-connections=0"}, expected: "invalid max-connections"},
		{name: "zero shutdown timeout", args: []string{"--shutdown-timeout=0s"}, expected: "invalid shutdown timeout"},
		{name: "unknown log level", args: []string{"--log-level=loud"}, expected: `invalid log level "loud"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			args := append([]string{"--listen-addr=127.0.0.1:0", "--metrics-addr=", "--log-level=error"}, test.args...)

			require.ErrorContains(t, app.Run(ctx, args), test.expected)
		})
	}
}

// start runs the service on the given address in the background and waits until the API answers.
//
// It returns a function which stops the service and returns what Run returned.
func start(t *testing.T, address string, args ...string) (stop func() error) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())

	var runErr error

	done := make(chan struct{})

	go func() {
		defer close(done)

		runErr = app.Run(ctx, append(args, "--listen-addr="+address, "--log-level=error"))
	}()

	stop = func() error {
		cancel()

		<-done

		return runErr
	}

	// the service is stopped even if the test fails before it gets to do that itself
	t.Cleanup(func() {
		stop() //nolint:errcheck
	})

	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
		}

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/healthz", nil)
		if err != nil {
			return false
		}

		// the connection is not kept, so that it does not hold one of --max-connections
		request.Close = true

		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return false
		}

		response.Body.Close() //nolint:errcheck

		return response.StatusCode == http.StatusOK
	}, 30*time.Second, 50*time.Millisecond)

	select {
	case <-done:
		require.FailNow(t, "the service stopped", "error: %v", runErr)
	default:
	}

	return stop
}

func get(t *testing.T, url string) (status int, body string) {
	t.Helper()

	status, _, body = do(t, t.Context(), http.MethodGet, url, nil)

	return status, body
}

// do sends a request and returns the status, the headers and the body of the response.
func do(t require.TestingT, ctx context.Context, method, url string, header http.Header) (int, http.Header, string) {
	request, err := http.NewRequestWithContext(ctx, method, url, nil)
	require.NoError(t, err)

	maps.Copy(request.Header, header)

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer response.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	return response.StatusCode, response.Header, string(body)
}

// dial opens a connection to the address, which is closed at the end of the test at the latest.
func dial(t *testing.T, address string) net.Conn {
	t.Helper()

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", address)
	require.NoError(t, err)

	t.Cleanup(func() { conn.Close() }) //nolint:errcheck

	return conn
}

// servedConnection returns a new connection to the address on which a health check was answered.
//
// It tries again while the connection is closed right away, as the server may not have seen
// the last one which was closed go yet.
func servedConnection(t *testing.T, address string) net.Conn {
	t.Helper()

	var conn net.Conn

	require.Eventually(t, func() bool {
		c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", address)
		if err != nil {
			return false
		}

		status, err := healthCheck(c, 10*time.Second)
		if err != nil || status != http.StatusOK {
			c.Close() //nolint:errcheck

			return false
		}

		conn = c

		return true
	}, 10*time.Second, 50*time.Millisecond)

	t.Cleanup(func() { conn.Close() }) //nolint:errcheck

	return conn
}

// healthCheck sends a health check over the connection and returns the status of the response,
// if it comes within the timeout.
func healthCheck(conn net.Conn, timeout time.Duration) (int, error) {
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return 0, err
	}

	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: fibonacci-service\r\n\r\n"); err != nil {
		return 0, err
	}

	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, err
	}

	return response.StatusCode, response.Body.Close()
}

// scrape returns the metrics of the service.
//
// The two servers start to listen independently of each other, and start waits for the API only.
// So it tries again while the metrics do not answer yet.
func scrape(t *testing.T, metricsAddress string) string {
	t.Helper()

	var metrics string

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		var status int

		status, _, metrics = do(collect, t.Context(), http.MethodGet, "http://"+metricsAddress+"/metrics", nil)

		assert.Equal(collect, http.StatusOK, status)
	}, 10*time.Second, 50*time.Millisecond)

	return metrics
}

// rejectedConnections returns the number of connections the service has closed right away.
func rejectedConnections(t *testing.T, metricsAddress string) int {
	t.Helper()

	metrics := scrape(t, metricsAddress)

	for line := range strings.Lines(metrics) {
		if value, ok := strings.CutPrefix(line, "fibonacci_service_http_connections_rejected_total "); ok {
			rejected, err := strconv.Atoi(strings.TrimSpace(value))
			require.NoError(t, err)

			return rejected
		}
	}

	require.FailNow(t, "the metric is missing")

	return 0
}

// handedOut holds the addresses freeListenAddress has returned to the tests which are running.
var handedOut sync.Map

// freeListenAddress returns a localhost address with a free port.
//
// The port is free again as soon as it is known, and Linux may pick it for the next caller.
// Two tests which run in parallel would then both start a service on it, so an address is
// not returned again before the test it was returned to is over.
func freeListenAddress(t *testing.T) string {
	t.Helper()

	for {
		listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		require.NoError(t, err)

		address := listener.Addr().String()

		require.NoError(t, listener.Close())

		if _, again := handedOut.LoadOrStore(address, struct{}{}); !again {
			t.Cleanup(func() { handedOut.Delete(address) })

			return address
		}
	}
}
