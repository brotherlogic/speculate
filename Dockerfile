# Build stage
FROM golang:1.24-alpine as builder

WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binaries
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /bin/speculate ./cmd/speculate
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /bin/prober ./cmd/prober

# Final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates git

WORKDIR /root/

# Copy the binaries from the builder stage
COPY --from=builder /bin/speculate .
COPY --from=builder /bin/prober .

# Expose ports: 8080 (HTTP / healthz / metrics), 50051 (gRPC)
EXPOSE 8080 50051

# Default binary
CMD ["./speculate"]
