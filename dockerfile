# syntax=docker/dockerfile:1.4
FROM golang:1.24 AS builder

WORKDIR /app

RUN --mount=type=cache,target=/root/.cache \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,target=. \
    go build -o /bot ./main.go

FROM alpine:3.18

WORKDIR /root/

COPY --from=builder /bot .

CMD ["./bot"]
