#!/usr/bin/env bash
# Build the interactive HTML guide (code blocks folded) into docs/guide/out/web/:
#   index.html   page body for publishing as a claude.ai artifact (no <html>/<head> skeleton)
#   guide.html   the same page as a full HTML document: open it locally in a browser
#   diagrams/    the diagrams, resized for the web
# Needs: pandoc, python3 with Pillow.
set -euo pipefail
TOOLS=$(cd "$(dirname "$0")" && pwd); GUIDE=$(dirname "$TOOLS"); REPO=$(cd "$GUIDE/../.." && pwd)
OUT="$GUIDE/out/web"; WORK="$GUIDE/out/.work/web"
rm -rf "$WORK" && mkdir -p "$WORK" "$OUT/diagrams"
cp "$GUIDE"/chapters/*.md "$WORK/"
# Step 2: append the full Pulumi source as folded code blocks
{
  echo; echo "#### Full source of \`infra/pulumi\`"; echo
  echo "Every file of the program, in reading order. Open the ones you need."; echo
  for f in Pulumi.yaml Pulumi.prod.yaml.example config.go network.go secgroups.go ports.go servers.go main.go; do
    if [[ $f == *.go ]]; then lang=go; c='//'; else lang=yaml; c='#'; fi
    echo "\`\`\`$lang"; echo "$c infra/pulumi/$f"; cat "$REPO/infra/pulumi/$f"; echo "\`\`\`"; echo
  done
} > "$WORK/pulumi-source.part"
python3 - "$WORK" <<'PY'
import sys; w = sys.argv[1]
p = f'{w}/11-step2-foundation.md'; s = open(p).read(); add = open(f'{w}/pulumi-source.part').read()
k = s.index('#### 2.2 Run it'); open(p, 'w').write(s[:k] + add + '\n' + s[k:])
PY
rm "$WORK/pulumi-source.part"
python3 - "$GUIDE/diagrams" "$OUT/diagrams" <<'PY'
import glob, os, sys
from PIL import Image
src, out = sys.argv[1], sys.argv[2]
for f in glob.glob(f'{src}/*.png'):
    im = Image.open(f)
    if im.width > 2000: im = im.resize((2000, round(im.height * 2000 / im.width)), Image.LANCZOS)
    im.save(os.path.join(out, os.path.basename(f)), optimize=True)
PY
pandoc "$WORK"/*.md -f markdown+fenced_divs+pipe_tables+grid_tables -t html5 --template "$TOOLS/web-template.html" \
  --lua-filter "$TOOLS/collapse.lua" --toc --toc-depth=2 --highlight-style=pygments \
  --metadata title="CSTAM Implementation Guide" --metadata date="$(date +'%d %B %Y')" -o "$OUT/index.html"
{ echo '<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover"></head><body>'
  cat "$OUT/index.html"; echo '</body></html>'; } > "$OUT/guide.html"
echo "open: $OUT/guide.html"
