-- Wrap every fenced code block in <details> so the reader opens code on demand.
local names = {go="Go", sql="SQL", bash="Shell", sh="Shell", yaml="YAML", json="JSON", haproxy="HAProxy",
  conf="Config", proto="Protobuf", text="Text", ts="TypeScript", tsx="TSX", ini="systemd", makefile="Makefile",
  nft="nftables", markdown="Markdown", toml="TOML", dockerfile="Dockerfile", hcl="HCL", http="HTTP", diff="Diff"}

local function esc(s)
  return (s:gsub("&","&amp;"):gsub("<","&lt;"):gsub(">","&gt;"):gsub('"',"&quot;"))
end

local function title_of(text)
  local i = 0
  for line in (text.."\n"):gmatch("([^\n]*)\n") do
    i = i + 1
    if i > 4 then break end
    local t = line:match("^%s*//%s*(.+)$") or line:match("^%s*%-%-%s*(.+)$") or line:match("^%s*#%s+(.+)$")
    if t and not line:match("^#!") then return t end
  end
  local first = text:match("^%s*([^\n]+)") or ""
  if pandoc.text.len(first) > 70 then first = pandoc.text.sub(first, 1, 67) .. "…" end
  return first
end

function CodeBlock(cb)
  local lang = cb.classes[1] or ""
  local n = select(2, cb.text:gsub("\n", "")) + 1
  local label = names[lang] or (lang ~= "" and lang or "Code")
  local head = string.format(
    '<details class="code"><summary><span class="lang">%s</span><span class="ctitle">%s</span><span class="lines">%d line%s</span></summary>',
    esc(label), esc(title_of(cb.text)), n, n == 1 and "" or "s")
  return { pandoc.RawBlock("html", head), cb, pandoc.RawBlock("html", "</details>") }
end
