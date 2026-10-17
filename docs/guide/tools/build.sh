#!/usr/bin/env bash
# Build the guide as two PDFs into docs/guide/out/:
#   CSTAM-Implementation-Guide.pdf  explanations, decisions, steps (long code replaced by references)
#   CSTAM-Code-Companion.pdf        every long listing, numbered, + the infra/pulumi source
# Needs: pandoc, python3, node 22, google-chrome.
set -euo pipefail
TOOLS=$(cd "$(dirname "$0")" && pwd); GUIDE=$(dirname "$TOOLS"); OUT="$GUIDE/out"
mkdir -p "$OUT/diagrams" && cp "$GUIDE"/diagrams/*.png "$OUT/diagrams/" && cp "$TOOLS/guide.css" "$OUT/"
python3 "$TOOLS/split.py"
DATE="$(date +'%d %B %Y')"
common=(-f markdown+fenced_divs+pipe_tables+grid_tables -t html5 -s --template "$TOOLS/template.html" --highlight-style=tango --metadata date="$DATE")

pandoc "$OUT"/.work/guide/*.md "${common[@]}" --toc --toc-depth=2 \
  --metadata title="CSTAM x FelCloud - Implementation Guide" \
  --metadata subtitle="Implementation guide in Go: architecture decisions and a step-by-step build plan that works on FelCloud" \
  --metadata coverimg="diagrams/f03-final-architecture.png" \
  --metadata coverMeta="Version 2.1 · FelCloud OpenStack (OVN) · Go · Pulumi · code listings in the Code Companion" \
  --metadata bodyclass="guide" -o "$OUT/CSTAM-Implementation-Guide.html"

pandoc "$OUT"/.work/code/*.md "${common[@]}" --toc --toc-depth=2 \
  --metadata title="CSTAM x FelCloud - Code Companion" \
  --metadata subtitle="Code Companion: every configuration file, SQL schema and Go listing referenced by the implementation guide" \
  --metadata coverMeta="Version 2.1 · listings are numbered <chapter>.<n> exactly as referenced in the guide" \
  --metadata bodyclass="code" -o "$OUT/CSTAM-Code-Companion.html"

node "$TOOLS/print.mjs" "$OUT/CSTAM-Implementation-Guide.html" "$OUT/CSTAM-Implementation-Guide.pdf" "Implementation guide"
node "$TOOLS/print.mjs" "$OUT/CSTAM-Code-Companion.html" "$OUT/CSTAM-Code-Companion.pdf" "Code Companion"
