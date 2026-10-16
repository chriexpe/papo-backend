#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
cp ../plugin-sdk/papo-plugin-ui.js reference/ui/papo-plugin-ui.js
for plugin in reference bluemap; do
  (cd "$plugin" && "${TINYGO:-tinygo}" build -target=wasip1 -buildmode=c-shared -scheduler=none -no-debug -o plugin.wasm .)
  chmod 644 "$plugin/plugin.wasm"
done
cd ../../backend/internal/wasmhost/testdata/adversarial
"${TINYGO:-tinygo}" build -target=wasip1 -buildmode=c-shared -scheduler=none -no-debug -o ../adversarial.wasm .
chmod 644 ../adversarial.wasm
