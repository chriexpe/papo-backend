#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
"${TINYGO:-tinygo}" build -target=wasip1 -buildmode=c-shared -scheduler=none -no-debug -o plugin.wasm .
cd ../../backend/internal/wasmhost/testdata/adversarial
"${TINYGO:-tinygo}" build -target=wasip1 -buildmode=c-shared -scheduler=none -no-debug -o ../adversarial.wasm .
