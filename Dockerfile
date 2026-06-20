# ── Build stage ──────────────────────────────────────────────────────────────
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Cache dependency layer
COPY go.mod go.sum ./
RUN go mod download

# Build binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o bot .

# ── Final stage (minimal image) ───────────────────────────────────────────────
FROM alpine:latest

# Required for HTTPS calls (Groq API + Telegram)
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /root/

COPY --from=builder /app/bot .

EXPOSE 8080

CMD ["./bot"]
