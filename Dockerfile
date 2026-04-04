# Stage 1: Build
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o whatsapp-bot main.go

# Stage 2: Runtime (image kecil ~30MB)
FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata
ENV TZ=Asia/Jakarta
WORKDIR /app
COPY --from=builder /app/whatsapp-bot .
EXPOSE 8080
VOLUME ["/app/data"]
CMD ["./whatsapp-bot"]
