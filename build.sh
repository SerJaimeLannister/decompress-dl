#!/bin/bash

# Build script for Hugging Face Space
echo "Building Go application..."

# Install dependencies
go mod download

# Build the binary
go build -o downloader-service .

echo "Build complete!"