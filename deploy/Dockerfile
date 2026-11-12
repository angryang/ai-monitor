# AI monitor Dockerfile — multi-stage scratch image.
# CGO_ENABLED=0 avoids needing glibc in the runtime image.
#
# Stage 1 builds the SPA. The Go builder image carries no Node.js, and
# internal/webui/dist is a gitignored build artifact that is therefore absent
# from a fresh checkout — so the bundle has to be produced inside the image,
# otherwise `make build` fails with "pattern dist: no matching files found".

# ---- stage 1: SPA (vue-tsc typecheck + vite build) ----
FROM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---- stage 2: static Go binary with the SPA embedded ----
FROM golang:1.25 AS builder
WORKDIR /src

# Copy go.mod/go.sum first for layer caching.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest and build a static binary.
COPY . .
# Install the embed source produced by stage 1 (.dockerignore keeps the host's
# copy of internal/webui/dist out of the context, so this is the only one).
COPY --from=web /web/dist ./internal/webui/dist
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
