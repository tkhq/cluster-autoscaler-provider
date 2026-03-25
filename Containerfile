# Build stage
FROM stagex/pallet-go:1.25.5@sha256:ba4285c7c169936c6be99d7c0197438f858536226d9f34af16f32afb832c259a AS builder

# Set the Current Working Directory inside the container
WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download all dependencies. Dependencies will be cached if the go.mod and go.sum files are not changed
RUN go mod download

# Copy the source from the current directory to the Working Directory inside the container
COPY . .

# Build the Go app
RUN CGO_ENABLED=0 GOOS=linux go build -a -o cluster-autoscaler-provider ./cmd/cluster-autoscaler-provider

# Use core for certs
FROM ghcr.io/tkhq/base/core:sha-fcb913a3883e4bc1e18226a6861238d6e19005ba@sha256:c6a5dd420f059bebd47205870516478aba4e918225ba9f9971310f2f9502982b AS core

# Final image
FROM scratch
COPY --from=core /etc/ssl /etc/ssl

WORKDIR /root/

# Copy the Pre-built binary file from the previous stage
COPY --from=builder /app/cluster-autoscaler-provider .

# Command to run the executable
ENTRYPOINT ["./cluster-autoscaler-provider"]
