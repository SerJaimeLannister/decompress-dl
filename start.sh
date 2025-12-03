#!/bin/bash

# Hugging Face Space startup script
echo "Starting Hugging Face Space..."

# Build the application if it doesn't exist
if [ ! -f "downloader-service" ]; then
    echo "Building application..."
    ./build.sh
fi

# Start the service
echo "Starting downloader service..."
./downloader-service