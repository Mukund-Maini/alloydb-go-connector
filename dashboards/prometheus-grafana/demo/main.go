// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command demo runs the real connector against a local fake AlloyDB instance
// and serves the connector's OpenTelemetry metrics to Prometheus.
//
// Every metric comes from the connector's own instrumentation: the demo only
// configures the global MeterProvider and calls Dialer.Dial. What is fake is
// the far end (the repo's internal/mock Admin API and server proxy) and the
// traffic. It needs no credentials and makes no Google Cloud calls.
//
// Each failure status comes from a real Dial failing, as follows:
//
//	user_error   asking for a public IP the instance doesn't have
//	cache_error  dialing an instance the Admin API doesn't know
//	tcp_error    a dial function that refuses the connection
//	tls_error    a connection that closes during the TLS handshake
//
// mdx_error is not produced: it needs a server that completes the handshake and
// then fails the metadata exchange, which internal/mock doesn't support.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"

	"cloud.google.com/go/alloydbconn"
	"cloud.google.com/go/alloydbconn/internal/mock"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"golang.org/x/oauth2"
)

// A larger count than the mock can use up, since it only answers a set number
// of requests.
const manyRequests = 1 << 30

type config struct {
	project, location, cluster, instance string
	iam                                  bool
	failPct                              int
}

func main() {
	cfg := config{
		project:  env("DEMO_PROJECT", "demo-project"),
		location: env("DEMO_LOCATION", "us-central1"),
		cluster:  env("DEMO_CLUSTER", "orders"),
		instance: env("DEMO_INSTANCE", "primary"),
		iam:      env("DEMO_IAM", "false") == "true",
	}
	var err error
	if cfg.failPct, err = strconv.Atoi(env("DEMO_FAIL_PCT", "5")); err != nil {
		log.Fatalf("invalid DEMO_FAIL_PCT: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// This is the setup METRICS.md asks applications to do. The connector picks
	// up this provider through otel.GetMeterProvider.
	exporter, err := prometheus.New()
	if err != nil {
		log.Fatal(err)
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	otel.SetMeterProvider(provider)
	defer provider.Shutdown(context.Background())

	d, inst, cleanup, err := newDialer(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer cleanup()
	defer d.Close()

	go generateTraffic(ctx, d, inst, cfg)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{Addr: ":2223", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	log.Printf("dialing %s through the real connector; metrics on :2223/metrics", inst)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// newDialer starts the fake AlloyDB instance and a real Dialer pointed at it.
func newDialer(ctx context.Context, cfg config) (*alloydbconn.Dialer, string, func(), error) {
	inst := mock.NewFakeInstance(cfg.project, cfg.location, cfg.cluster, cfg.instance)
	client, url, closeAdmin := mock.HTTPClient(
		mock.InstanceGetSuccess(inst, manyRequests),
		mock.CreateEphemeralSuccess(inst, manyRequests),
	)
	// StartServerProxy only uses its testing.T to report that port 5433 is
	// taken. Nothing else in this container listens there.
	stopProxy := mock.StartServerProxy(&testing.T{}, inst)
	cleanup := func() {
		stopProxy()
		_ = closeAdmin() // Reports unused mock calls, which don't matter here.
	}

	stub := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "demo-token"})
	opts := []alloydbconn.Option{
		alloydbconn.WithTokenSource(stub),
		// Besides reaching the mock, this has a useful side effect: the built-in
		// Cloud Monitoring client uses gRPC, can't use an HTTP client, and so
		// fails to start. The connector then disables that exporter, and
		// nothing is sent to Google Cloud. WithOptOutOfBuiltInTelemetry isn't an
		// option because it would disable the metrics this demo exists to show.
		alloydbconn.WithHTTPClient(client),
		alloydbconn.WithAdminAPIEndpoint(url),
	}
	if cfg.iam {
		opts = append(opts, alloydbconn.WithIAMAuthN(), alloydbconn.WithIAMAuthNTokenSource(stub))
	}
	if env("DEMO_DEBUG", "false") == "true" {
		opts = append(opts, alloydbconn.WithDebugLogger(debugLogger{}))
	}
	d, err := alloydbconn.NewDialer(ctx, opts...)
	if err != nil {
		cleanup()
		return nil, "", nil, fmt.Errorf("creating dialer: %w", err)
	}
	return d, inst.String(), cleanup, nil
}

// generateTraffic dials twice a second. Most dials succeed and hold their
// connection for a while, so the open connections metric moves.
func generateTraffic(ctx context.Context, d *alloydbconn.Dialer, inst string, cfg config) {
	unknown := fmt.Sprintf("projects/%s/locations/%s/clusters/%s/instances/misconfigured",
		cfg.project, cfg.location, cfg.cluster)
	failures := []func(context.Context){
		func(ctx context.Context) { _, _ = d.Dial(ctx, inst, alloydbconn.WithPublicIP()) },
		func(ctx context.Context) { _, _ = d.Dial(ctx, unknown) },
		func(ctx context.Context) { _, _ = d.Dial(ctx, inst, alloydbconn.WithOneOffDialFunc(refuse)) },
		func(ctx context.Context) { _, _ = d.Dial(ctx, inst, alloydbconn.WithOneOffDialFunc(closedConn)) },
	}

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if rand.IntN(100) < cfg.failPct {
			go failures[rand.IntN(len(failures))](ctx)
			continue
		}
		go dialAndHold(ctx, d, inst)
	}
}

// dialAndHold makes a successful connection, moves some bytes and closes it.
// The mock server sends the instance name and closes its side straight away, so
// the amount of data is small. The connection stays open on the client's side.
func dialAndHold(ctx context.Context, d *alloydbconn.Dialer, inst string) {
	conn, err := d.Dial(ctx, inst)
	if err != nil {
		log.Printf("dial failed: %v", err)
		return
	}
	defer conn.Close()
	_, _ = conn.Write(make([]byte, 1024))
	_, _ = io.Copy(io.Discard, conn)
	select {
	case <-ctx.Done():
	case <-time.After(time.Duration(5+rand.IntN(25)) * time.Second):
	}
}

func refuse(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("demo: connection refused")
}

func closedConn(context.Context, string, string) (net.Conn, error) {
	client, server := net.Pipe()
	server.Close()
	return client, nil
}

type debugLogger struct{}

func (debugLogger) Debugf(format string, args ...any) { log.Printf(format, args...) }

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
