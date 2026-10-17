# Documentation

| Folder | What | When to read it |
|---|---|---|
| [`challenge/brief.md`](challenge/brief.md) | The official CSTAM-FELCLOUD brief and scoring | First, once |
| [`guide/`](guide/) | **The implementation guide**: design decisions and the step-by-step build plan (Go, Pulumi) | The reference for all work |
| [`team/README.md`](team/README.md) | One-time setup for a new teammate (SSH key, OpenStack, shared Pulumi state) | Before your first day |
| [`notebook/experiments.md`](notebook/experiments.md) | Lab notebook: every experiment, measurement and decision on FelCloud | Before touching the cloud; add to it after |
| [`notebook/learning.md`](notebook/learning.md) | Answers to the guide's learning checkpoints (jury preparation) | After each step |
| [`design-v1/`](design-v1/) | The first solution design (v1) with its diagrams, the edge-case note, the old plan | History only: the guide supersedes it |

## Reading order
1. `challenge/brief.md`: what is scored.
2. Guide Part I, chapters 1 to 8: why the system looks the way it does.
3. `team/README.md`: get access.
4. Guide Part II: the step you are working on. Each step ends with a "Done when" checklist.

## The guide
- Sources: `guide/chapters/*.md` (one file per chapter) and `guide/diagrams/` (PNG + editable `.excalidraw`).
- Build: `guide/tools/build-web.sh` (interactive HTML, code folded) or `guide/tools/build.sh`
  (guide PDF + Code Companion PDF). Output goes to `guide/out/` (not committed).
- Edit the chapters, never the generated files.
