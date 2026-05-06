FROM golang:1.26 AS builder
RUN git clone --depth 1 https://github.com/etcd-io/etcd.git /etcd

WORKDIR /etcd/server
RUN go build -o /usr/local/bin/etcd .

WORKDIR /etcd/tools/benchmark
RUN go build -o /usr/local/bin/benchmark .

FROM debian:bookworm-slim
COPY --from=builder /usr/local/bin/etcd /usr/local/bin/etcd
COPY --from=builder /usr/local/bin/benchmark /usr/local/bin/benchmark
EXPOSE 2379 2380
CMD ["etcd"]
