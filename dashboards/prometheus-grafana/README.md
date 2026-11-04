# Grafana + Prometheus dashboard for connector metrics

Choose which connector metrics you want, then generate a Grafana dashboard for
them. Metrics are documented in [METRICS.md](../../METRICS.md). The demo is its
own Go module, so its dependencies don't affect the connector's `go.mod`.

## Try it (real connector, fake AlloyDB)

```sh
docker compose up --build
```

1. Open the setup page at http://localhost:8080.
2. Turn off any metrics you don't want.
3. Click **Create dashboard in Grafana**, then follow the link. Grafana has no
   dashboard until you do this. Clicking again replaces it.

| Service | URL | Purpose |
| --- | --- | --- |
| Setup page | http://localhost:8080 | Pick metrics, create the dashboard |
| Grafana | http://localhost:3000 | View the dashboard |
| Prometheus | http://localhost:9090 | Stores the metrics |
| Demo | http://localhost:2223/metrics | Metrics from one of the three demo processes |

This stack is for local use only: Grafana allows anonymous admin access, and
the ports are bound to `127.0.0.1`.

### What is real and what isn't

`demo/` runs the connector from this checkout. It configures the global
MeterProvider and calls `Dialer.Dial`, so every metric comes from the connector's
own instrumentation. It needs no credentials and sends nothing to Google Cloud.

The far end and the traffic are fake. Dials go to the `internal/mock` Admin API
and server proxy on localhost, so **latency and byte counts say nothing about
real AlloyDB performance**. This has not yet run against a live instance.

Failures are real `Dial` failures, triggered on purpose:

| Status | Trigger |
| --- | --- |
| `user_error` | Requesting a public IP the instance doesn't have |
| `cache_error` | Dialing an instance the Admin API doesn't know (`misconfigured` in the filters) |
| `tcp_error` | A dial function that refuses the connection |
| `tls_error` | A connection that closes during the TLS handshake |

Not exercised: the `mdx_error` status and the `lazy` refresh type. Refresh counts
are driven by failed dials, because the mock's certificates last 24 hours.

Set `DEMO_DEBUG=true` on a demo service to see the connector's debug logs.

## Use it with a real application

1. Copy the **Go snippet** from the setup page into your application's startup
   code, before creating the Dialer. It configures the global MeterProvider,
   serves `/metrics`, and drops the metrics you turned off.
2. In `prometheus/prometheus.yml`, replace the `demo-*` targets with your
   application's address.
3. Create the dashboard from the setup page with the same selection.

The connector always records all six metrics. The selection takes effect in
your application's OpenTelemetry SDK, and in which panels the dashboard has.

## Styling limits

The setup page follows the design reference fully. Grafana does not allow custom
CSS, so the dashboard only approximates it, using solid stat tiles, the reference
palette, smooth charts and the same layout. Rounded cards and colored chart
backgrounds aren't available.

## Prometheus metric names

The OTel Prometheus exporter converts dots to underscores, appends `milliseconds`
for `ms` units, drops `{...}` units, and adds `_total` to counters. These names
were checked against the real connector running in the demo:

| OTel metric | Prometheus series |
| --- | --- |
| `alloydb.connector.dial.count` | `alloydb_connector_dial_count_total` |
| `alloydb.connector.dial.duration` | `alloydb_connector_dial_duration_milliseconds_{bucket,sum,count}` |
| `alloydb.connector.connections.open` | `alloydb_connector_connections_open` |
| `alloydb.connector.bytes.sent` | `alloydb_connector_bytes_sent_total` |
| `alloydb.connector.bytes.received` | `alloydb_connector_bytes_received_total` |
| `alloydb.connector.refresh.count` | `alloydb_connector_refresh_count_total` |

Attributes become labels the same way (`alloydb.instance_id` ->
`alloydb_instance_id`). If your exporter version or translation strategy
differs, adjust the queries in `setup/index.html`.
