# syntax=docker/dockerfile:1
FROM golang:1.26-alpine3.23 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY skills/ ./skills/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ssh-use ./cmd/ssh-use

FROM alpine:3.23

RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 ssh-use \
    && adduser -D -u 10001 -G ssh-use -h /home/ssh-use ssh-use \
    && mkdir -p /etc/ssh-use /var/lib/ssh-use /home/ssh-use/.ssh \
    && chown -R ssh-use:ssh-use /etc/ssh-use /var/lib/ssh-use /home/ssh-use

COPY --from=build /out/ssh-use /usr/local/bin/ssh-use

ENV HOME=/home/ssh-use \
    SSH_USE_CONFIG_PATH=/etc/ssh-use/config.yaml \
    SSH_USE_DATA_DIR=/var/lib/ssh-use

USER 10001:10001
WORKDIR /home/ssh-use
EXPOSE 7443/tcp
ENTRYPOINT ["/usr/local/bin/ssh-use"]
CMD ["help"]
