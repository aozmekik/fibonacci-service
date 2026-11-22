// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package app wires up and runs the Fibonacci service.
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"

	"github.com/aozmekik/fibonacci-service/internal/api"
	"github.com/aozmekik/fibonacci-service/internal/fibonacci"
	"github.com/aozmekik/fibonacci-service/internal/version"
)

const (
	appName = "fibonacci-service"

	// writeTimeout is the time a client has to read a response, the largest one included.
	// The requests in flight get as long on shutdown by default, so a response which would
	// have made it is not cut short by a restart.
	writeTimeout = 30 * time.Second

	// maxHeaderBytes limits how much of the request line and the headers the server reads,
	// and net/http allows 4 kB more. A request to the API takes about a hundred bytes,
	// the rest is for what a proxy adds. With the default of 1 MB, a client could make
	// the server read and keep that much on every connection it opens.
	maxHeaderBytes = 8 << 10
)

// Run starts the service with the given command line arguments and serves until ctx is canceled.
func Run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet(appName, flag.ContinueOnError)

	var (
		listenAddr  = flags.String("listen-addr", ":8080", "address to serve the API on")
		metricsAddr = flags.String("metrics-addr", ":2122", "address to serve the Prometheus metrics on, empty to disable")
		maxN        = flags.Int("max-n", 1000,
			fmt.Sprintf("largest n to accept, at most %d; the numbers are computed once at startup and kept in memory", fibonacci.MaxLen))
		maxConnections  = flags.Int("max-connections", 256, "most connections each server keeps open at once; the ones over it are closed right away")
		shutdownTimeout = flags.Duration("shutdown-timeout", writeTimeout, "time the requests in flight are given to complete on shutdown")
		logLevel        = flags.String("log-level", "info", "log level")
		showVersion     = flags.Bool("version", false, "print the version and exit")
	)

	// The flag package reports a failure both by printing it and by returning it.
	// The caller prints the returned error, so nothing is printed here but the help.
	flags.SetOutput(io.Discard)

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(os.Stdout)
			flags.Usage()

			return nil
		}

		return fmt.Errorf("failed to parse the command line: %w", err)
	}

	if flags.NArg() > 0 {
		return fmt.Errorf("failed to parse the command line: unexpected argument %q", flags.Arg(0))
	}

	if *showVersion {
		fmt.Printf("%s %s (%s)\n", appName, version.Tag, version.SHA)

		return nil
	}

	// net/http takes an empty address for ":http", which is not what leaving it empty would mean here
	if *listenAddr == "" {
		return errors.New("invalid listen address: it must not be empty")
	}

	if *maxConnections <= 0 {
		return fmt.Errorf("invalid max-connections %d: it must be positive", *maxConnections)
	}

	if *shutdownTimeout <= 0 {
		return fmt.Errorf("invalid shutdown timeout %s: it must be positive", *shutdownTimeout)
	}

	logger, err := buildLogger(*logLevel)
	if err != nil {
		return err
	}

	defer logger.Sync() //nolint:errcheck

	start := time.Now()

	table, err := fibonacci.NewTable(*maxN)
	if err != nil {
		return fmt.Errorf("invalid max-n: %w", err)
	}

	logger.Info(
		"computed the Fibonacci numbers",
		zap.Int("max_n", table.Len()),
		zap.Int("table_bytes", table.JSONLen(table.Len())),
		zap.Duration("duration", time.Since(start)),
	)

	// net/http and promhttp report what goes wrong inside of them through this logger,
	// a panic in a handler included.
	errorLog, err := zap.NewStdLogAt(logger, zapcore.ErrorLevel)
	if err != nil {
		return fmt.Errorf("failed to create the error logger: %w", err)
	}

	registry := newRegistry()
	metrics := newAPIMetrics(registry)

	apiHandler := metrics.instrument(api.NewHandler(table, func(err error) {
		metrics.aborted.Inc()

		logger.Debug("failed to write a response in full", zap.Error(err))
	}))

	// The health check is answered before the metrics of the API see the request,
	// as they would otherwise mostly count probes. Its path is compared the way it
	// was sent, as the API compares its own.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() == "/healthz" {
			w.Write([]byte("ok\n")) //nolint:errcheck

			return
		}

		apiHandler.ServeHTTP(w, r)
	})

	// The API comes first, as this is also the order the servers are shut down in.
	servers := []*http.Server{newServer(*listenAddr, handler, errorLog, limitConnections(*maxConnections, metrics.rejected.Inc))}

	if *metricsAddr != "" {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{ErrorLog: errorLog}))

		servers = append(servers, newServer(*metricsAddr, metricsMux, errorLog, limitConnections(*maxConnections, metrics.rejected.Inc)))
	}

	logger.Info(
		"starting",
		zap.String("version", version.Tag),
		zap.String("listen_addr", *listenAddr),
		zap.String("metrics_addr", *metricsAddr),
		zap.Int("max_connections", *maxConnections),
	)

	return serve(ctx, servers, *shutdownTimeout, logger)
}

// newServer returns a server with the limits which keep a slow or stuck client from holding a connection forever.
func newServer(addr string, handler http.Handler, errorLog *log.Logger, connState func(net.Conn, http.ConnState)) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          errorLog,
		ConnState:         connState,
	}
}

// limitConnections returns a ConnState hook which closes a new connection right away
// while limit others are open, and calls rejected for it.
//
// Every open connection holds a goroutine and its buffers, whether a request is served on it or not,
// so the timeouts bound for how long a client can hold memory, and this bounds how much.
//
// The count relies on what net/http documents for the hook: a connection is reported as new once,
// and as closed or hijacked once at its end. A connection which is closed here is no exception,
// as the server fails to read from it and reports it closed like any other.
func limitConnections(limit int, rejected func()) func(net.Conn, http.ConnState) {
	var open atomic.Int64

	return func(conn net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			if open.Add(1) > int64(limit) {
				rejected()

				conn.Close() //nolint:errcheck
			}
		case http.StateHijacked, http.StateClosed:
			open.Add(-1)
		case http.StateActive, http.StateIdle:
		}
	}
}

// serve runs the servers until ctx is canceled or one of them fails, and shuts all of them down.
func serve(ctx context.Context, servers []*http.Server, shutdownTimeout time.Duration, logger *zap.Logger) error {
	eg, ctx := errgroup.WithContext(ctx)

	for _, server := range servers {
		eg.Go(func() error {
			if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("failed to serve on %q: %w", server.Addr, err)
			}

			return nil
		})
	}

	// ListenAndServe returns as soon as the shutdown starts, so it is this goroutine
	// which keeps the process alive until the requests in flight are done.
	eg.Go(func() error {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()

		var errs []error

		// One server after the other, so that the metrics are served for as long as the API is.
		for _, server := range servers {
			logger.Info("shutting down", zap.String("addr", server.Addr))

			if err := server.Shutdown(shutdownCtx); err != nil {
				server.Close() //nolint:errcheck

				errs = append(errs, fmt.Errorf("failed to shut down the server on %q gracefully: %w", server.Addr, err))
			}
		}

		return errors.Join(errs...)
	})

	return eg.Wait()
}

func buildLogger(level string) (*zap.Logger, error) {
	var zapLevel zapcore.Level

	if err := zapLevel.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", level, err)
	}

	config := zap.NewProductionConfig()
	config.Level.SetLevel(zapLevel)

	return config.Build(zap.AddStacktrace(zapcore.FatalLevel)) // stack traces only for fatal errors
}
