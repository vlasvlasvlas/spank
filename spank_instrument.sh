#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG="${1:-$ROOT/instrument.example.yaml}"
ADDR="${SPANK_WEB_ADDR:-127.0.0.1:8765}"

cd "$ROOT"

go build -o spank .
sudo ./spank --instrument --config "$CONFIG" --web-addr "$ADDR"
