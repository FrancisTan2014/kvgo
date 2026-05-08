FROM golang:1.25 AS builder
WORKDIR /build
COPY src/go.mod src/go.sum ./
RUN go mod download
COPY src/ ./
RUN CGO_ENABLED=0 go build -o /kv-server ./cmd/kv-server/
RUN CGO_ENABLED=0 go build -o /kv-cli ./cmd/kv-cli/
RUN CGO_ENABLED=0 go build -o /kv-bench ./cmd/kv-bench/

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /kv-server /usr/local/bin/
COPY --from=builder /kv-cli /usr/local/bin/
COPY --from=builder /kv-bench /usr/local/bin/
ENTRYPOINT ["kv-server"]
