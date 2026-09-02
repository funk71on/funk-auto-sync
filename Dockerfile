# Stage 1: Build binary
FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /funk-auto-sync main.go

# Stage 2: Runtime image
FROM alpine:3.20

RUN apk add --no-cache \
    git \
    openssh-client \
    ca-certificates \
    tzdata

COPY --from=builder /funk-auto-sync /usr/local/bin/funk-auto-sync

WORKDIR /notes

ENTRYPOINT ["funk-auto-sync"]
CMD ["-path", "/notes", "-debounce", "2s"]
