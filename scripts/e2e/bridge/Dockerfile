# One image for every role in the bridge e2e: relay, connector, agent, echo.
# Build context: repository root.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY relay/go.mod relay/go.sum relay/
RUN go mod download && cd relay && go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/tunr ./cmd/tunr && \
    cd relay && CGO_ENABLED=0 go build -trimpath -o /out/relay ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache bash curl jq socat postgresql16-client netcat-openbsd ca-certificates
COPY --from=build /out/tunr /out/relay /usr/local/bin/
COPY scripts/e2e/bridge/connector.sh /e2e/connector.sh
ENTRYPOINT []
