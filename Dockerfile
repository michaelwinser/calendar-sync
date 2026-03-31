# Multi-stage build for calendar-sync
FROM golang:1.25-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -o calendar-sync .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata sqlite-libs
WORKDIR /app
COPY --from=builder /build/calendar-sync .
COPY app.yaml .

EXPOSE 4004
VOLUME /app/data

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -qO- http://localhost:4004/health || exit 1

ENTRYPOINT ["./calendar-sync"]
CMD ["serve"]
