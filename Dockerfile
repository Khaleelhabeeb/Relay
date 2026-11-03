# syntax=docker/dockerfile:1

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/khaleelhabeeb/relay/internal/cli.Version=${VERSION}" \
      -o /out/relay ./cmd/relay \
 && mkdir -p /out/data

# Static binary on distroless: no shell, no package manager, runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="Relay" \
      org.opencontainers.image.description="Self-hosted Paystack webhook router: one account, many applications" \
      org.opencontainers.image.source="https://github.com/khaleelhabeeb/relay" \
      org.opencontainers.image.documentation="https://github.com/khaleelhabeeb/relay#readme" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/relay /usr/local/bin/relay
COPY --from=build --chown=nonroot:nonroot /out/data /var/lib/relay
VOLUME ["/var/lib/relay"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/relay"]
CMD ["serve", "--config", "/etc/relay/relay.yaml"]
