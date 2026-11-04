# Build from the repository root (see docker-compose.yml): the demo compiles the
# connector in this checkout, not a published release.
FROM golang:1.25 AS build
WORKDIR /src
COPY . .
WORKDIR /src/dashboards/prometheus-grafana/demo
RUN CGO_ENABLED=0 go build -mod=readonly -o /demo .

FROM gcr.io/distroless/static-debian12
COPY --from=build /demo /demo
EXPOSE 2223
ENTRYPOINT ["/demo"]
