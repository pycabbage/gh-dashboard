#!/bin/bash

REPO=$(git rev-parse --show-toplevel)
WORKSPACE=$(mktemp -d)

(
  set -euo pipefail

  cd "$REPO"

  docker build -t "gh-dashboard-builder:latest" -f build/Dockerfile .

  echo "Cloning repository into workspace: $WORKSPACE/repo"
  git clone --depth 1 "$REPO" "$WORKSPACE/repo"

  echo "Running build in Docker container..."
  docker run \
    --name gh-dashboard-builder \
    --rm -d \
    -v "$WORKSPACE/repo:/home/ubuntu/repo" \
    -w /home/ubuntu/repo \
    "gh-dashboard-builder:latest" \
    sleep infinity

  echo "Executing build commands inside Docker container..."
  docker exec -i gh-dashboard-builder bash -c "
  echo 'Generating GraphQL code...'
  curl --fail-with-body -sSL https://docs.github.com/public/fpt/schema.docs.graphql -o gql/schema.graphql
  go generate ./gql/...
  ls -lh gql
  echo 'Building binaries...'
  go build ./...
  "
)

# Cleanup
echo "Cleaning up Docker container..."
docker rm -f gh-dashboard-builder
echo "Build artifacts may be available in $WORKSPACE/repo"
