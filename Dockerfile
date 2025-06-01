# Stage 1: Build the Go binary
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Copy go.mod and go.sum first to leverage Docker cache
COPY go.mod go.sum* ./
RUN go mod download

# Copy the rest of the application source code
COPY . .

# Build the application
# CGO_ENABLED=0 for a statically linked binary, useful for minimal Docker images
# -ldflags "-s -w" to strip debug information and reduce binary size
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags "-s -w" -o plex-aggregator main.go

# Stage 2: Create the final lightweight image
FROM alpine:latest

WORKDIR /app

# Copy the config.json. 
# Users should mount their actual config.json or build an image with it.
# For this Dockerfile, we assume config.json will be in the same directory as the Dockerfile during build context,
# or mounted at runtime.
COPY config.json .

# Copy the built binary from the builder stage
COPY --from=builder /app/plex-aggregator .

# Expose the port the application listens on (default 32400)
# This should match the aggregator_listen_address in config.json
EXPOSE 32400

# Set the entrypoint for the container
ENTRYPOINT ["./plex-aggregator"]
