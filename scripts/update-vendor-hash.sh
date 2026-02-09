#!/usr/bin/env bash
set -euo pipefail

FLAKE_FILE="$(git rev-parse --show-toplevel)/flake.nix"
FAKE_HASH="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

if [[ ! -f "$FLAKE_FILE" ]]; then
  echo "error: flake.nix not found" >&2
  exit 1
fi

# Extract current hash
current_hash=$(grep -oP 'vendorHash = "\K[^"]+' "$FLAKE_FILE")
echo "Current vendorHash: $current_hash"

# Temporarily set a fake hash
sed -i "s|vendorHash = \"${current_hash}\"|vendorHash = \"${FAKE_HASH}\"|" "$FLAKE_FILE"

# Build and capture the correct hash from the error output
# Use --no-link to avoid creating a result symlink inside the repo
echo "Computing correct vendorHash..."
correct_hash=""
if build_output=$(nix build --no-link 2>&1); then
  echo "nix build succeeded — restoring original hash"
  sed -i "s|vendorHash = \"${FAKE_HASH}\"|vendorHash = \"${current_hash}\"|" "$FLAKE_FILE"
  exit 0
else
  correct_hash=$(echo "$build_output" | grep -oP 'got:\s+\K\S+' | head -1)
fi

if [[ -z "$correct_hash" ]]; then
  echo "error: could not extract hash from nix build output" >&2
  echo "$build_output" >&2
  # Restore original hash
  sed -i "s|vendorHash = \"${FAKE_HASH}\"|vendorHash = \"${current_hash}\"|" "$FLAKE_FILE"
  exit 1
fi

# Update flake.nix with the correct hash
sed -i "s|vendorHash = \"${FAKE_HASH}\"|vendorHash = \"${correct_hash}\"|" "$FLAKE_FILE"

if [[ "$current_hash" == "$correct_hash" ]]; then
  echo "vendorHash is already up to date: $correct_hash"
else
  echo "Updated vendorHash: $current_hash -> $correct_hash"
fi
