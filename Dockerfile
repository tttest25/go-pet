FROM golang:1.23-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -trimpath -tags="netgo,osusergo" -ldflags="-s -w -extldflags '-static'" -o /app/petar ./cmd/petar

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata bash curl jq \
    && adduser -D -u 1000 petar \
    && mkdir -p /app/spool/in /app/spool/processing /app/spool/out \
    && chown -R petar:petar /app/spool

COPY --from=builder /app/petar /usr/local/bin/petar

USER petar
WORKDIR /app

EXPOSE 8080

ENTRYPOINT ["petar"]
