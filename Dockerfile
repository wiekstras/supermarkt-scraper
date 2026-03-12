FROM golang:1.23-alpine AS builder
WORKDIR /app

# Download deps first (cached layer)
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o scraper .

# Minimal final image
FROM alpine:3.19
RUN apk --no-cache add ca-certificates
COPY --from=builder /app/scraper /scraper
RUN chmod +x /scraper
EXPOSE 8080
CMD ["/scraper"]
