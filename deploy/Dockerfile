# AI monitor Dockerfile — multi-stage scratch image.
# CGO_ENABLED=0 avoids needing glibc in the runtime image.

FROM golang:1.25 AS builder
WORKDIR /src

# Copy go.mod/go.sum first for layer caching.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest and build a static binary.
COPY . .
ENV CGO_ENABLED=0 GOOS=linux
RUN make build

FROM scratch
LABEL org.opencontainers.image.title="ai-monitor"
LABEL org.opencontainers.image.source="https://github.com/angryang/ai-monitor"

# Static binaries only need CA + the binary + the timezone database.
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /src/ai-monitor /ai-monitor

EXPOSE 9090
USER 1000:1000
ENTRYPOINT ["/ai-monitor"]
