# Build stage
FROM golang:1.25.5-trixie AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o dbbat ./cmd/dbbat

# Runtime stage
FROM gcr.io/distroless/base-debian13:nonroot

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/dbbat .

# Expose ports
EXPOSE 5432 8080

# Run the binary
CMD ["./dbbat"]
