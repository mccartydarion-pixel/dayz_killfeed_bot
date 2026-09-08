FROM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/dayz-killfeed ./cmd/server

FROM alpine:3.20
RUN addgroup -S app && adduser -S -G app app
WORKDIR /app
COPY --from=builder /out/dayz-killfeed /app/dayz-killfeed
EXPOSE 8080
USER app
ENTRYPOINT ["/app/dayz-killfeed"]
