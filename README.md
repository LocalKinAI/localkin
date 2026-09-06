# KinClaw

> **The self-fissioning lobster. Breeds its own swarm on demand.**
> 可以裂变的龙虾 — 根据需求自己造龙虾群。

KinClaw is a computer-use agent — primary target macOS, also runs on
Linux and Windows as of 2026-05-12. It sees your screen, understands
your UI semantically, clicks, types, and — the part no one else has —
**reproduces on demand** via three primitives:

- **Soul Clone** (`pkg/clone`) — duplicate a specialist into N
  parallel workers with small per-clone divergence.
- **Skill Forge** (`pkg/skill` forge) — when an existing skill
  can't handle a task, KinClaw drafts, writes, registers, and tests
  a new one, then retries.
- **Sub-agent dispatch** (`spawn` skill) — fork-exec a specialist
  child on a different brain (researcher / eye / critic / coder ship
  in-box; hierarchical, kernel-capped at depth 1).

Single binary, ~18 MB, Go 1.22+, MIT licensed.

> ## 🙏 Help wanted — Linux + Windows testers
>
> The agent wrote 2,042 lines of Linux + Windows port code from API
> docs without hardware to test against on 2026-05-12. **Builds are
> green; runtime behaviour is unverified.** If you have a Linux box or
> a Windows machine and 10 minutes to spare, grab a prebuilt binary
> from the [latest release](https://github.com/LocalKinAI/kinclaw/releases)
> and try a few smoke commands. See [**`TESTING.md`**](TESTING.md) for
> a 3-command quickstart + report template. Track Linux progress in
> [**#1**](https://github.com/LocalKinAI/kinclaw/issues/1), Windows in
> [**#2**](https://github.com/LocalKinAI/kinclaw/issues/2). Both ✅ and
> ❌ reports are useful.

**Primary target: macOS** (tested daily-driver).
**Linux Phase 2-5 + Windows Phase 6 both landed 2026-05-12** — code
complete, awaiting community runtime testing
([#1 Linux](https://github.com/LocalKinAI/kinclaw/issues/1),
[#2 Windows](https://github.com/LocalKinAI/kinclaw/issues/2)):

| Layer | macOS | Linux | Windows | Backend (non-mac) |
|---|---|---|---|---|
| `screen` claw | ScreenCaptureKit | ✅ | ✅ | Linux: grim / scrot / xrandr · Windows: PowerShell + System.Drawing |
| `input` claw | CGEvent | ✅ | ✅ | Linux: xdotool / ydotool · Windows: user32.dll P/Invoke + SendKeys |
| `ui` claw — window-level | AX | ✅ | ✅ | Linux: xdotool / wmctrl · Windows: user32.dll GetForegroundWindow |
| `ui` claw — a11y tree | AX | ✅ | ✅ | Linux: **AT-SPI 2 via godbus** · Windows: **UI Automation (UIA)** |
| `record` claw | ScreenCaptureKit | ✅ | ✅ | Linux: ffmpeg (x11grab / pipewire) · Windows: ffmpeg (gdigrab) |
| `location` skill | corelocationcli | ✅ | ✅ | Linux: gdbus + geoclue2 · Windows: Windows.Devices.Geolocation (ipapi.co fallback) |
| `cerebellum/*.sh` library | 16 cats (478 actions) | 4 cats (linux-files/apps/settings/clipboard) | 4 cats (windows-files/apps/settings/clipboard) | shell + POSIX tools / PowerShell |
| `pilot` soul | 24 skills | 23 skills (no `app_open_clean`) | 23 skills (no `app_open_clean`) | — |

Daily-driver souls:
- [`souls/pilot.soul.md`](souls/pilot.soul.md) — macOS
- [`souls/pilot_linux.soul.md`](souls/pilot_linux.soul.md) — Linux
- [`souls/pilot_windows.soul.md`](souls/pilot_windows.soul.md) — Windows

> *Same starter lobster for everyone. Every user's swarm is unique
> after a month.*

KinClaw grew out of the earlier `localkin` runtime (a minimal
embodied-AI microkernel, ~2,300 lines). This repo is that same
skeleton with the **five claws** bolted on: `screen`, `input`, `ui`,
`record`, and `web` (Playwright). On macOS the first four use
ScreenCaptureKit / CGEvent / Accessibility API / kinrec via their own
zero-cgo KinKit libraries; the Linux + Windows ports swap those for
native backends (see feature-parity table above) without changing the
skill API.

## 🆕 The grep-routed agent stack (2026-05-11, paper #11)

Since 2026-05-11 KinClaw ships a four-layer NL → action router
(`skills/kinthink/`) on top of a multi-platform skill library
(`skills/cerebellum/` — 16 macOS cats / 478 actions, 4 Linux cats,
4 Windows cats; each backend swaps in transparently per `runtime.GOOS`).
For prompts that match a known canonical operation — file rename,
note create, calendar event, settings toggle, web fetch, … — the
router skips the LLM entirely:

```
$ kinclaw -soul souls/macbench.soul.md -exec "rename foo.txt to bar.txt"
★ matched   : 001-finder-rename  (tf-idf=3.43, 20ms)
★ substituted: cerebellum 'finder rename ~/Desktop/kinbench/foo.txt ~/Desktop/kinbench/bar.txt'
★ router    : 65 ms
ok: rename /Users/jackysun/Desktop/kinbench/foo.txt -> /Users/jackysun/Desktop/kinbench/bar.txt
★ TOTAL     : 53 ms (router 20 ms + exec 33 ms)
real 0.56     # ← 0 LLM tokens, 50-500× faster than agent loop
```

On the **macbench v0.2 benchmark (379 tasks)**:

| Configuration | Pass | Time | Tokens |
|---|---:|---:|---|
| LLM-only baseline (paper #10) | 30.4% | 107 min | Full |
| Reference verifier (no LLM) | 84.3% | 22 min | 0 |
| **kinthink + cerebellum (this stack)** | **48.0%** | **76 min** | **0 on Layer-0 hits** |

Opt in by setting `cerebellum.grep_route: true` in your soul. See
[paper #11](https://www.localkin.dev/papers/grep-routed-agents) for
the architecture + measurement.

## Quick start

```bash
go install github.com/LocalKinAI/kinclaw/cmd/kinclaw@latest

# Default pilot runs Kimi K2.5 via Ollama Cloud. Sign in once:
ollama signin

# The pilot soul — the generalist that drives your Mac
kinclaw -soul souls/pilot.soul.md

# Then ask it something like:
# > "What app is in front? Click the Save button if there is one."
```

Want a specialist instead of the generalist pilot? KinClaw ships four
focused souls; pilot dispatches to them via `spawn`, but you can also
launch them directly:

```bash
kinclaw -soul souls/researcher.soul.md    # Kimi K2.6 (1T, 256k ctx) — deep web research
kinclaw -soul souls/eye.soul.md           # Kimi K2.6 multimodal — visual verification
kinclaw -soul souls/critic.soul.md        # Minimax M2.7 — adversarial review
kinclaw -soul souls/coder.soul.md         # DeepSeek V4 Pro — harvest --inspire forge specialist
```

All four use Ollama Cloud routing (`ollama signin` once). Different labs
on purpose: pilot+researcher+eye on Moonshot, critic on Minimax, coder
on DeepSeek — different model lineage means different blind spots.

First run triggers two macOS TCC prompts:
- **Screen Recording** (for `screen` + `record` skills via sckit-go / kinrec)
- **Accessibility** (for `input` + `ui` skills via input-go + kinax-go)

Grant both; rerun. `record mic=true` adds a Microphone prompt; `location` skill adds a Location Services prompt; first browser launch downloads Chromium (~500MB).

### `kinclaw serve` — floating chat UI (v1.8+)

CLI not your thing? Run it as a chat in the browser:

```bash
kinclaw serve -soul souls/pilot.soul.md
# Open: http://127.0.0.1:8020/
# Float: http://127.0.0.1:8020/?compact   (chat-only, ~380×600 friendly)
```

Single floating window — chat goes in, KinClaw operates your real
desktop, you watch your actual macOS screen change. No split-pane,
no virtual sandbox, no recreated "agent's eyes" view. The window is
the remote control; your desktop is the work.

Open as a standalone-looking window (no browser chrome) via Chrome's
`--app=` mode:

```bash
open -na "Google Chrome" --args --app="http://127.0.0.1:8020/?compact"
```

Drag to a corner, ~380×600. For real always-on-top use Rectangle
Pro / Hammerspoon / BetterTouchTool to pin the window. (A native
Swift WKWebView shell with built-in always-on-top is v0.2 work.)

Features in the chat window:

- **🎙 push-to-talk** — hold the mic button to speak; release to
  send. STT via local SenseVoice on `:8000` (LocalKin Service Audio).
  CJK + English both work.
- **🔊 voice replies** — toggle on, KinClaw speaks its replies via
  Kokoro on `:8001`. Auto-picks `zf_xiaoxiao` for Chinese.
- **Markdown rendering** — tables / code fences / lists / links
  stream into the chat as the LLM types.
- **Soul switcher** — click the soul name top-left, dropdown lists
  all souls in `./souls/` and `~/.localkin/souls/`.
- **Session replay** — every run writes JSONL to
  `~/.kinclaw/serve-sessions/<ts>.jsonl`. Replay later:
  `kinclaw serve --replay <file>`.
- **Esc** — cancel a running turn.

Voice mode requires the LocalKin Audio Service running locally
(`:8000` STT, `:8001` TTS); both are open-source sidecars. Not
strictly required — the chat works fine without them, mic/speak
toggles just no-op.

### Optional sidecars (peripheral capabilities)

KinClaw stays a small Go binary; capabilities that need heavy deps
ship as opt-in sidecars selected via env var:

| Capability | Sidecar | Env var | Setup |
|---|---|---|---|
| Web research | SearXNG | `SEARXNG_ENDPOINT` | self-host (default: `http://localhost:8080`) |
| Voice synthesis | Kokoro (via [localkin-service-audio](https://github.com/LocalKinAI/localkin-service-audio)) | `TTS_ENDPOINT` | run server on `:8001` |
| Voice recognition | SenseVoice (via [localkin-service-audio](https://github.com/LocalKinAI/localkin-service-audio)) | `STT_ENDPOINT` | run server on `:8000` |
| Web automation | Playwright (Python) | none — `web` skill uses `python3 ./web.py` directly | `pip install playwright && playwright install chromium` |
| Real-time GPS | corelocationcli | none — `location` skill calls binary | `brew install corelocationcli` |

### Per-user context (auto-injected to every soul prompt)

| Variable | Where it comes from |
|---|---|
| `{{current_date}}` | `time.Now()` at boot |
| `{{tz}}` | local timezone (e.g. `PDT (UTC-7)`) |
| `{{platform}}` | `runtime.GOOS` mapped to `macOS`/`Linux`/`Windows` |
| `{{arch}}` | `runtime.GOARCH` (`arm64` / `amd64`) |
| `{{location}}` `{{lat}}` `{{lon}}` `{{city}}` `{{country}}` | `$KINCLAW_LOCATION="lat,lon[,city[,country]]"` env var |
| `## 已学到的` section | `~/.kinclaw/learned.md` — **technical doctrine** across sessions (full file up to 8KB; beyond that a topic index + newest tail, `learn action=recall topic=X` for the rest) |
| `## 用户长期记忆` section | `memories` k-v table in `~/.localkin/memory.db` — **user-facts** across sessions (v1.9+) |
| Last 50 messages of `<soul-name>` session | `messages` table in `~/.localkin/memory.db` — **conversation continuity** across kinclaw restarts (v1.9+) |

After a few weeks of use, the agent boots with rich context: knows
its OS + general location, remembers what worked on which app, and
— crucially since v1.9 — **picks up where the conversation left
off**. Restart kinclaw → same `<soul-name>` thread continues, same
durable user-facts in working memory.

**Two memory layers, two recall scopes:**

```
memory action=save  key=<dotted.path>  value=<fact>     # store user-fact
memory action=recall query=X                            # search k-v facts (default)
memory action=recall query=X scope=history              # LIKE-search the raw chat log
memory action=recall query=X scope=all                  # both, two sections
```

`learn topic=<bundle_id> note=<...>` is the technical-doctrine sibling
(writes to learned.md). Rule of thumb: **learn** for "how X system
behaves" (app schema, error codes, shortcut tricks). **memory** for
"who the user is" (name, location, friends, projects, preferences).

### Data location — two homes (since v1.10)

KinClaw splits persistent state across **two homes** by concern.
`~/.localkin/` is **family-shared runtime** (memory, souls, skills
that any LocalKin product reads). `~/.kinclaw/` is **kinclaw
product state** (harvest, serve recordings, learned doctrine).

**`~/.localkin/` — shared family runtime** (LocalKin / KinClaw /
KinClaw Mac / kinclaw-ios all read these):

| File | Purpose |
|---|---|
| `~/.localkin/memory.db` | conversation history + durable user-facts (SQLite) |
| `~/.localkin/souls/` | user-level souls usable by any product |
| `~/.localkin/skills/` | cross-product skills (kin_audio / image_gen / etc.) |
| `~/.localkin/auth.json` | LocalKin auth |
| `~/.localkin/cron.yaml` + `cron_state/` | LocalKin cron daemon |

**`~/.kinclaw/` — kinclaw product state** (this binary's outputs;
nothing else writes here):

| File | Purpose |
|---|---|
| `~/.kinclaw/learned.md` | technical doctrine across sessions (injected at boot; index + tail past 8KB) |
| `~/.kinclaw/KINCLAW.md` | your standing instructions, appended to every soul prompt (v1.18) |
| `~/.kinclaw/tool-results/` | oversized tool outputs spilled from the context (7-day prune) |
| `~/.kinclaw/serve-sessions/<ts>.jsonl` | `kinclaw serve` event recordings (replayable) |
| `~/.kinclaw/harvest/` | external skill candidates pulled by `kinclaw harvest` |
| `~/.kinclaw/harvest.toml` | harvest source manifest |

KinClaw Mac (the macOS dock app) writes its UI state here too —
`~/.kinclaw/sessions/<agentSlug>/<UUID>.json` for multi-session
chat history. Two products share `~/.kinclaw/` cleanly because
each owns a different sub-tree.

`~/Library/Caches/kinclaw/` — screenshots + per-soul output
(big-blob OS cache, distinct from config-style state above).

**Why shared `memory.db`**: telling LocalKin's pilot "I live in
SF" should mean KinClaw's pilot also knows. The lobster family is
meant to feel like one brain regardless of which binary is the
entry point. Soul names are namespaced (`KinClaw <X>` vs `<X>`)
to prevent accidental cross-product session merges.

**Override for isolation**: set `$KINCLAW_DATA_DIR` to point
KinClaw's memory at its own directory:

```bash
KINCLAW_DATA_DIR=~/.kinclaw-isolated kinclaw -soul souls/pilot.soul.md
KINCLAW_DATA_DIR=/tmp/kinclaw-fresh kinclaw -soul ...   # ephemeral test
```

The override currently affects `memory.db` only; `learned.md` and
`serve-sessions/` always resolve under `~/.kinclaw/`. Per-path
overrides are a future step.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                  Soul (.soul.md)                            │
│  YAML frontmatter + Markdown system prompt                  │
│  template subs: {{platform}} {{tz}} {{location}} ...        │
│  + auto-loaded ~/.kinclaw/learned.md (cross-session)        │
├─────────────────────────────────────────────────────────────┤
│                       Brain (LLM)                           │
│  Claude · OpenAI · Ollama · Kimi · GLM · Qwen · any         │
│  multimodal images attached when brain supports vision      │
├─────────────────────────────────────────────────────────────┤
│                       Skills (Tools)                        │
│                                                             │
│  ─── The five claws ───                                     │
│  screen ─ eye          ─► sckit-go  (ScreenCaptureKit)      │
│  input  ─ hand         ─► input-go  (CGEvent)               │
│  ui     ─ visual cortex─► kinax-go  (AX, semantic UI)       │
│  record ─ memory       ─► kinrec    (video MP4 + audio)     │
│  web    ─ open net     ─► Playwright (DOM render + scrape)  │
│                                                             │
│  ─── Classic kernel ───                                     │
│  shell · file_read/write/edit · web_fetch · web_search      │
│                                                             │
│  ─── Self-evolution ───                                     │
│  forge   — author new skills (with kernel quality gate)     │
│  learn   — append cross-session lessons to learned.md       │
│  clone   — duplicate souls into N parallel workers (lib)    │
│  spawn   — dispatch subtask to specialist child (depth-1)   │
│  harvest — pull skills from other agent repos (CLI subcmd)  │
│                                                             │
│  ─── External SKILL.md plugins (./skills/) ───              │
│  tts / stt — Kokoro / SenseVoice via :8001 / :8000          │
│  location  — corelocationcli (real-time GPS)                │
│  app_open_clean — open + dismiss welcome modal              │
│  any forge'd or hand-written SKILL.md is auto-loaded        │
│                                                             │
├─────────────────────────────────────────────────────────────┤
│  Kernel guards (4-trigger circuit breaker)                  │
│  · same-error consecutive  · cumulative failures           │
│  · same-output no-progress · per-turn usage cap            │
│  + ui click ambiguity refusal · destructive-target refusal  │
├─────────────────────────────────────────────────────────────┤
│       SQLite memory per session + learned.md across them    │
└─────────────────────────────────────────────────────────────┘
```

### KinKit — the open-source claws

All four sibling libraries are MIT, zero cgo, `go install`-able:

| Library | Role | Dylib |
|---------|------|-------|
| [sckit-go](https://github.com/LocalKinAI/sckit-go) | ScreenCaptureKit — screenshots + live streams | ~130 KB |
| [kinrec](https://github.com/LocalKinAI/kinrec) | Screen + audio recorder (MP4, h264/hevc) | ~130 KB |
| [input-go](https://github.com/LocalKinAI/input-go) | CGEvent mouse + keyboard synthesis | ~85 KB |
| [kinax-go](https://github.com/LocalKinAI/kinax-go) | Accessibility API UI tree access | ~88 KB |

Each uses the **embedded dylib pattern** (`purego` + `//go:embed`),
so downstream users never need `clang` or `CGO_ENABLED`. See
[Paper #9 on localkin.dev](https://www.localkin.dev/papers/embedded-dylib)
for the full architectural story.

## The harness — permission gate, plan mode, compaction, hooks (v1.18)

Claws decide *what the agent can do*; the harness decides *how it is
allowed to do it for hours at a time*. v1.18 adds the four habits that
make Claude Code and Claude Desktop trustworthy, all opt-in per soul.

### Permission gate

```yaml
permissions:
  mode: ask            # default "auto" — existing souls are unchanged
  ask:   ["shell", "file_write", "file_edit", "forge", "mcp_*"]
  allow: ["shell(git status*)", "shell(ls*)", "file_write(~/.kinclaw*)"]
```

In `ask` mode a matching call stops and asks — a terminal prompt in the
REPL, an approval card in KinClaw Mac (**Allow / Always this session /
Deny**). Rule grammar: skill name, `prefix*`, or `skill(param-prefix*)`
on the skill's primary parameter (`shell(git push*)`, `ui(click*)`,
`file_write(~/.kinclaw*)`). `allow` beats `ask`. Shell commands that
look irreversible (`rm -r`, `sudo`, `git push`, `kill`, `diskutil
erase`, `curl … | sh`) always ask unless allowed. A denial becomes the
tool result, so the model explains and adapts instead of retrying.
GUI claws are not in the default ask set on purpose — in Cowork the
clicking *is* the work. Scripts: `kinclaw -permissions auto`.

### Plan mode

`/plan` (REPL) or the clipboard button in KinClaw Mac. The agent may
look — `screen screenshot`, `ui tree/find/read/watch`, `file_read`,
web, `kinbrain`, `memory`, `todo_write` — but every mutating call is
refused with "finish investigating, end with a numbered plan, wait for
approval". Per action: `ui tree` passes, `ui click` doesn't.

### Context compaction

```yaml
context:
  compact_at: 0.75     # fraction of brain.context_length
  keep_recent: 8       # trailing messages kept verbatim
```

When the provider-reported prompt size crosses the threshold, older
messages are folded into a summary the model writes (goals, what was
done, every path / URL / bundle id verbatim, todo state, open issues)
and the session continues. `/compact` or `POST /api/compact` on demand.
Old rows are archived under `"<soul>@<timestamp>"`, never deleted.
`/usage` shows the numbers; KinClaw Mac shows a meter.

### Hooks

```yaml
hooks:
  pre_tool:
    - match: "input"                  # skill name, prefix*, or empty = all
      run: "./hooks/no-typing-while-zoom.sh"
  post_tool:
    - match: "shell"
      run: "tee -a ~/.kinclaw/shell.log >/dev/null"
  stop:
    - run: "afplay /System/Library/Sounds/Glass.aiff"
```

Your shell commands at fixed points, JSON payload on stdin, Claude
Code's exit protocol: `0` continue, `2` block (pre_tool: the call never
runs and stderr is what the model sees), anything else logged and
ignored. A deterministic rule you can write in five lines of bash beats
a paragraph of prompt.

### Standing instructions — `KINCLAW.md`

`~/.kinclaw/KINCLAW.md` (global) and `./KINCLAW.md` (per directory) are
appended to every soul's prompt. Souls are shared identity; this is your
house rules.

### `kinclaw memory`

```
kinclaw memory                 overview
kinclaw memory list [-all]     durable facts
kinclaw memory search <query>  facts + conversation history + notebook
kinclaw memory forget <key>
kinclaw memory learned [topic] the notebook (~/.kinclaw/learned.md)
kinclaw memory sessions        live + archived conversation buckets
```

## MCP servers — tools from outside this repo

kinclaw can use tools published by any [Model Context Protocol](https://modelcontextprotocol.io)
server. Configure them in `~/.localkin/mcp.json`, using the same `mcpServers`
block Claude Desktop uses — a server's published install snippet pastes in
unchanged:

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "~/Desktop"]
    }
  }
}
```

Then let a soul see them via `skills.enable`:

```yaml
skills:
  enable:
    - "mcp_*"                 # every configured server
    - "mcp_filesystem_*"      # or just one
```

Tools register as `mcp_<server>_<tool>` alongside the built-ins and reach the
model through the same path. Namespacing isn't cosmetic: server tool names come
from third parties and collide freely with built-ins like `read` or `search`.

Startup reports each server's fate, and a failure points at its own output:

```
[mcp] ✓ filesystem: 14 tool(s)
[mcp] ✗ github: fork/exec ...: no such file (server output: ~/.localkin/logs/mcp-server-github.log)
[mcp] – slack: disabled
```

`mcpServers` only ever describes **local stdio servers** — remote ones are
added through OAuth in a host UI, not this file, which is why there are no
`url`/`headers` fields.

**MCP or a skill?** MCP is for capabilities someone else maintains and that
change under you — GitHub's API, Slack's auth. A skill is for your own domain
logic, for anything needing kinclaw's internals (soul, memory, spawn), and for
stable algorithms worth owning. `kinclaw harvest` sits between them: it takes
*ideas* from external libraries and forges them into skills you then maintain.

---

## Soul schema

A soul file is YAML frontmatter + a Markdown system prompt.

```yaml
---
name: "KinClaw Pilot"
brain:
  provider: "ollama"
  model: "kimi-k2.5:cloud"
permissions:
  shell: false
  network: false
  screen: true       # sckit-go capability
  input: true        # input-go capability
  ui: true           # kinax-go capability
  record: true       # kinrec capability — video MP4 + audio
  spawn: false       # opt in to sub-agent dispatch (default off)
skills:
  enable: ["screen", "input", "ui", "record", "file_read", "tts", "stt"]
---

# You are KinClaw Pilot...
```

The `screen / input / ui / record` bits are the KinClaw additions. Each
corresponds to one or two TCC prompts and one KinKit library. If a bit
is false, the matching skill returns `permission denied: soul does not
grant X capability` regardless of what the LLM asks for. `record`
shares Screen Recording TCC with `screen`; mic capture additionally
requires Microphone TCC.

## The five claws in action

### `ui` — semantic UI control (the killer feature)

Click a button by its **title**, not pixel coordinates. This is what
makes KinClaw different from Computer Use / Operator, which look at
screenshots and guess.

```
user: Click "Save"
LLM:  ui action=click role=AXButton title=Save
      → clicked AXButton "Save" (matched role=AXButton title="Save")
```

Other `ui` actions: `focused_app`, `tree` (dump the AX tree),
`find` (list matching elements), `read` (read element value),
`at_point` (hit-test a coordinate), `watch` (subscribe to AX
events — see below).

**`ui action=watch`** (v1.7+) blocks for `duration_ms` collecting
push-based AX notifications via kinax-go's `Observer`. Cheaper than
polling `ui tree` for "did anything change":

```
ui action=watch events=AXFocusedWindowChanged duration_ms=5000
ui action=watch events=AXValueChanged,AXMenuOpened duration_ms=3000 pid=12345
```

Returns the events that fired during the window. Use it when you
need to wait for a specific UI event (window focus shifted, dialog
appeared, value updated post-click) instead of guessing when to
re-tree.

### `input` — raw mouse + keyboard

When there's no AX element (canvas apps, games, some WebGL), fall
back to coordinates:

```
LLM: input action=click x=842 y=523
LLM: input action=type text="hello 世界 👋"
LLM: input action=hotkey mods="cmd+shift" key="t"
```

**Background mode** (v1.4+): pass `target_pid=<N>` and the event
routes directly to that process via `CGEventPostToPid` — the
targeted app receives the input but its window does **not** come
to front. The user's foreground app keeps focus, so the agent can
drive a background app (Music, Reminders, Slack, ...) while the
user keeps working in their editor. Verified on Lark / VSCode /
Chrome / Cursor and other Electron + WebKit hosts; some Apple
sandboxed apps (newer Mail / Messages) may ignore PID-targeted
events — fall back to omitting the param.

```
LLM: input action=click x=400 y=300 target_pid=12345
LLM: input action=type text="hello" target_pid=12345
```

### Two cascades — reading the screen, and driving an app (v1.7+)

KinClaw doctrine: try the **cheapest, most deterministic** tool
first; escalate only when it fails. Two independent ladders that
agents combine in real tasks.

**Reading the screen (what's on it?)**

```
Layer 1   ui claw           ~50ms      $0       deterministic
Layer 2   screen + vision   ~3s        ~$0.005  generic
```

- **Layer 1** — `ui find` / `ui tree` / `ui read`. 94% of macOS apps
  expose Accessibility; AX is semantic, fast, free. Use first.
- **Layer 2** — `screen action=screenshot` + `file_read` + multimodal
  brain. The catch-all when AX is empty (canvas apps, image-rendered
  UI) or you need *understanding* not just text — vision LLMs return
  text plus context in one call.

**Side tool: `screen action=ocr`** — local Vision-framework OCR
(~50-200ms, free) is also exposed. NOT in the default cascade — most
tasks should skip straight from Layer 1 to Layer 2. Reach for OCR
only in specific niches: bulk-extracting many numeric values where
vision-LLM cost would be prohibitive, pure text + bounding-box jobs
where you don't need semantics, or offline runs without brain
auth. Watch out for OCR's character-confusion failure modes
(`W↔H`, `M↔N`, `l↔I↔1`, `O↔0`, `B↔8`) — even at conf=1.0.

**Driving the app (do something)**

```
Layer 1   ui + input claw   semantic UI driving           default
Layer 2   shell (osascript / CLI)  programmatic shortcut  when CLI exists
Layer 3   forge a new SKILL.md     auto-author wrapper    repeated + parameterizable
```

- **Layer 1** — `ui click` / `ui click_sequence` / `input type` /
  `input hotkey`. Real UI driving; observable, demoable, portable
  across resolutions. Always try first.
- **Layer 2** — `shell` (`osascript -e ...`, `pmset`, `brightness`,
  `mdfind`, `defaults`, app-specific CLIs). Shortcut when an app or
  the OS already exposes a deterministic CLI. Don't make it the
  default — that's just AppleScript automator with extra steps.
- **Layer 3** — `forge` skill writes a new `SKILL.md` from a recipe
  (Layer 1 + 2 components) so future runs skip directly to the
  forged wrapper. Trigger only on "repeated + parameterizable"
  tasks; one-shots aren't worth it.

The pilot soul has both cascades baked in: never skip Layer 1 just
because Layer 3 is more flexible.

### `screen` — just take a picture

```
LLM: screen action=screenshot
     → ~/Library/Caches/kinclaw/screens/screen-20260424-001312.000.png
```

The LLM can then read the PNG back (if `file_read` is enabled) and
reason about it visually — Layer 2 of the read cascade above.

**Also available** (rare niche): `screen action=ocr` exposes local
Apple Vision OCR for bulk text-and-coordinate extraction without a
vision-LLM round-trip. See the cascade section above for when this
is actually the right tool — most read-screen tasks should skip
straight from `ui` (AX) to `screen + vision LLM`.

### `record` — non-blocking video capture

`start` returns a recording_id immediately; the agent keeps operating
the Mac while kinrec writes MP4 in the background. `stop` finalizes
the file. Audio sources are independent: `audio=true` taps system
output (everything coming out of your speakers), `mic=true` adds the
microphone track. Both can be on at once for live-narrated demos.

```
LLM: record action=start audio=true show_clicks=true
     → recording_id: rec-1745627812-1
       path: ~/Library/Caches/kinclaw/recordings/rec-20260425-225612.mp4
LLM: ui action=click title=Save
LLM: record action=stop id=rec-1745627812-1
     → path: ~/.../rec-20260425-225612.mp4
       duration: 12.4s  bytes: 8.3M  frames: 372
```

Other actions: `list` (active recordings), `stats id=...` (live frame
counters).

### `web` — drive the open internet

When the task lives outside macOS apps (login flows, dynamic SPAs,
sites without a public API), the `web` claw runs Playwright headless-
or-headed on top of Chromium. Ships as an external `SKILL.md` in
`skills/web/` so it stays a thin Python shim around `python3 web.py`
— forge can rewrite it without recompiling kinclaw.

```
LLM: web action=goto url="https://news.ycombinator.com"
LLM: web action=text selector="h1"
     → "Hacker News"
LLM: web action=click selector="text=login"
LLM: web action=type selector="input[name=acct]" text="..."
```

First launch downloads Chromium (~500 MB) into Playwright's cache.
Subsequent launches reuse it.

### `browser_session` — multi-step browser automation (super-skill, v1.9+)

When `web` (one-shot Playwright) isn't enough — login flows that
span 5 pages, forms gated behind authenticated state, JS-heavy SPAs
where DOM-numbered element targeting beats CSS selectors — KinClaw
hands off to [browser-use](https://github.com/browser-use/browser-use)
(91K stars, MIT). The framework runs its own LLM-driven planning
loop with persistent session, screenshot-based visual reasoning, and
DOM enumeration; KinClaw treats it as one tool call: in goes a
high-level task description, out comes the result.

```
LLM: browser_session task="Open Hacker News, find the top story, return title + URL"
     → "Top story: Dav2d (https://videolan.org), 215 points, 3h ago"

LLM: browser_session task="Search Zillow for 1bed apartments in SF
     under $2500 with ocean view, return the top 5 with addresses"
     → ...full ranked list with links...
```

Cost: ~10-20s cold start (browser warmup + LLM init), ~2-5s per
interaction step. Don't use for one-shot fetches (`web` is faster).
Pilot soul has a doctrine that auto-routes: ≥2 interaction verbs
in the task description = `browser_session`, otherwise `web`.

First-time setup (one machine, ~5 min):

```bash
cd skills/browser_session
./setup.sh        # creates ./.venv/, installs browser-use + Chromium
```

LLM provider is env-driven, in order: `ANTHROPIC_API_KEY` (Claude),
`OPENAI_API_KEY` (GPT-4o), `OLLAMA_BASE_URL` (local Ollama via
OpenAI-compat). Override the model with `BROWSER_USE_MODEL=...`.

#### The "super-skill" pattern

`browser_session` is the first instance of a deliberate pattern:
**wrap a battle-tested third-party OSS framework as a thin SKILL.md,
make it callable from any soul that opts in via
`permissions.skills.enable`**. The kinclaw kernel doesn't know or
care that there's an entire LLM-driven planning agent inside; it
just sees one tool, one input, one output.

Future super-skill candidates (each ~half day to wrap):

- `video_edit` — ffmpeg + AI scene detection + auto-subtitle
- `rag_search` — grep-is-all-you-need or any vector DB
- `audio_clone` — F5-TTS / OpenVoice for voice cloning
- `pdf_extract` — marker / unstructured.io
- `yt_upload` — Google API + auto metadata

The pattern is "thin soul, fat skill" pushed to its useful extreme:
LocalKin family hosts; we don't reinvent.

## Audio I/O — talk to your Mac, hear it back

`tts` and `stt` ship as **external SKILL.md plugins** in `skills/tts/`
and `skills/stt/`. They wrap [localkin-service-audio](https://github.com/LocalKinAI/localkin-service-audio)
— a local-first audio server running Kokoro (TTS) on `:8001` and
SenseVoice (STT) on `:8000` by default. See that repo's README for
install + run instructions; KinClaw discovers the endpoints via the
`TTS_ENDPOINT` / `STT_ENDPOINT` env vars (override the defaults if
you put the server elsewhere).

```
LLM: tts text="接下来打开计算器"
     → CJK auto-detected; speaker=zf_xiaoxiao; Kokoro synthesizes;
       afplay plays through speakers; record captures it as system
       audio if a recording is in flight.
LLM: tts text="Then I'll search for kinclaw" speaker=af_bella
     → English voice on demand.
LLM: stt path=~/Library/Caches/kinclaw/recordings/rec-XXXX.mp4
     → text: "今天天气怎么样"
       language: zh
```

> **Note on voice selection.** LocalKin Service Audio's `/synthesize`
> takes the parameter `speaker`, not `voice` — passing `voice=...` is
> silently ignored and falls back to the English-only Kokoro pipeline,
> which mispronounces Chinese text as the literal phrase "chinese
> letter". The `tts` SKILL.md auto-picks `zf_xiaoxiao` whenever the
> text contains non-ASCII characters; override with `speaker=...` for
> a different voice.

Why external SKILL.md and not native? Because they're HTTP wrappers,
exactly the shape `forge` would author. Keeping them external means
the kernel stays thin and users can fork either file without
recompiling. They also serve as forge templates for any next HTTP
service you want to integrate.

## Soul Clone (fission primitive #1)

```go
import (
    "github.com/LocalKinAI/kinclaw/pkg/clone"
    "github.com/LocalKinAI/kinclaw/pkg/soul"
)

// Make 10 parallel email readers, each assigned one email.
paths, _ := clone.Clone("souls/email_reader.soul.md", clone.CloneOptions{
    Count: 10,
    FrontmatterPatch: func(i int, meta *soul.Meta) {
        meta.Name = fmt.Sprintf("Email Reader #%d", i)
    },
})
// Clones land next to the parent, discovered on /reload.
```

Cheap (kilobytes), fast (milliseconds), no model calls. Task
fission becomes an N-way parallel tool invocation.

## Skill Forge (fission primitive #2)

Inherited from the `localkin` base. When the LLM asks for a skill
that doesn't exist, `forge` drafts a `SKILL.md` + implementation
script, validates syntax, registers it in the live registry, and
retries the original task. See `pkg/skill/native.go` for the forge
skill.

## `kinbrain` — query accumulated knowledge (2026-05-17)

KinClaw ships a `kinbrain` skill that shells out to the `kinbrain`
CLI from [LocalKinAI/localkin-core](https://github.com/LocalKinAI/localkin-core).
The CLI is a 4-root unified grep view over:

| Root | Contents |
|------|----------|
| `~/.kinbrain/notes/` | your manual notes (writable) |
| `$LOCALKIN_REPO/output/` | the swarm's distilled markdowns (~1,500 files / 14 MB across 50+ agents) |
| `$LOCALKIN_REPO/knowledge/` | curated canon (bible 5 versions / 19 MB) |
| `$LOCALKIN_REPO/input/` | bulk source corpora (~200 MB spiritual + TCM classics) |

Today's total: **3,150+ entries / 230 MB** queryable in one tool call.
No DB, no vector store — paper #4 grep-is-all-you-need extended to
a slow-accumulating personal corpus.

### Two actions

The LLM sees one tool, `kinbrain`, with an `action` parameter:

- **`recall`** — grep across all roots, returns matching paths grouped
  by root with hit counts. Call this BEFORE doing novel research:
  there's a very good chance the swarm or Jacky already wrote about it.
- **`save`** — append a short note to `~/.kinbrain/notes/<date>/kinclaw/`
  after finishing a task that produced a reusable insight.

Source bucket is hardcoded to `kinclaw` on save so claw-captured notes
are visually distinct from manual `kinbrain save thought` entries when
browsing `~/.kinbrain/notes/<date>/`.

### Install

```bash
# 1. Build + install the kinbrain CLI (one-time)
cd ~/Documents/Workspace/localkin && go install ./cmd/kinbrain
kinbrain version    # → kinbrain v0.2.0 (localkin-core, 4-root: ...)

# 2. Optional but strongly recommended for 100MB+ corpora:
brew install ripgrep
# → 60s cold cache (BSD grep) → 0.5s (ripgrep). kinbrain auto-detects.

# 3. Enable in your soul YAML:
#    permissions.skills.enable: [shell, kinbrain, ...]
```

Without `kinbrain` on PATH the skill stays registered but Execute()
returns a clear "install kinbrain" error — souls that never enable it
see nothing.

### Soul prompt hint (recommended)

Add to system prompt so the LLM remembers to use it:

```
KNOWLEDGE LOOKUP — before doing novel research or writing code, ALWAYS
try `kinbrain` with action=recall first. The swarm has written 1,500+
markdown analyses across 80+ agents over 6 months; the spiritual /
TCM corpora hold 230 MB of curated text. Find it before redoing the
work.
```

Source: [`pkg/skill/kinbrain.go`](pkg/skill/kinbrain.go) (KinClaw side),
[`LocalKinAI/localkin-core/pkg/kinbrain/`](https://github.com/LocalKinAI/localkin-core/tree/main/pkg/kinbrain) (the brain itself).

## Skill harvest — `kinclaw harvest`

`kinclaw harvest` pulls candidate `SKILL.md` files from other agent
repos (Claude Code, Hermes Agent, your own private repos), runs them
through the forge quality gate v2 + critic soul review, and stages
survivors at `~/.kinclaw/harvest/staged/` for human approval. Final
acceptance into `./skills/` is always manual — the pipeline never
auto-merges.

Three commands:

```bash
kinclaw harvest                          # scan all sources, curator triages → stage yes/maybe
kinclaw harvest --review                 # show what's staged + verdicts
kinclaw harvest --accept claude-code/foo # coder forges this one into ./skills/<name>/
```

### Scan = triage, not forge

`kinclaw harvest` runs the **curator** specialist soul
(`souls/curator.soul.md`, Kimi K2.6 / 1T params) over each external
candidate. Curator knows:

- KinClaw's architecture (5 claws, soul system, exec philosophy, non-goals)
- Your **actual** `./skills/` inventory (auto-injected at run start)
- The candidate's name + description + body excerpt

Curator returns one of three verdicts per candidate, with a one-line
reason:

| Verdict | Action |
|---|---|
| **yes** | obvious gap-filler that fits exec form → stage |
| **maybe** | partial overlap or unclear → stage with the doubt noted |
| **no** | already have it / pure LLM workflow / out of scope → drop |

Cost is small per call (~3s × ~500 tokens on Kimi K2.6). A full scan
over Hermes Agent's 85 skills runs in ~4 minutes / ~40k tokens —
much cheaper than forging anything.

### Forge happens at `--accept` time

When you've reviewed and want to actually use one of the staged
candidates, `kinclaw harvest --accept <source>/<skill-name>` spawns
the **coder** specialist (`souls/coder.soul.md`, DeepSeek V4 Pro)
to forge a real KinClaw exec-style `SKILL.md`. Three outcomes:

| Coder result | Lands at |
|---|---|
| **forged** + parses + passes forge gate v2 | `./skills/<forged_name>/` (runnable) |
| **defer_to_procedural** (capability needs LLM/AX/vision) | `./skills/library/<source>/<name>/original.md` (kept as inspiration) |
| forge errors (unparseable / forge gate fail / duplicate) | clear error, nothing written |

You only pay the forge cost (~30s / ~2k tokens) on candidates you
actually want — not on every procedural skill in the source repos.

### Cron mode

```bash
kinclaw harvest --no-judge               # cron-cheap: clone caches + count, no LLM
kinclaw harvest --diff                   # dry-run: scan + triage, write nothing
```

The launchd cron template (`scripts/com.localkin.kinclaw-harvest.plist`)
runs `--no-judge` — 3 AM jobs only refresh source caches + report counts.
Run `kinclaw harvest` (no flags) interactively when you want the curator
triage.

Manifest at `~/.kinclaw/harvest.toml`:

```toml
[[source]]
name         = "claude-code"
url          = "https://github.com/anthropics/claude-code"
skill_paths  = ["plugin-source/skills/**/SKILL.md"]
license_allow = ["MIT", "Apache-2.0"]

[[source]]
name         = "openclaw"
url          = "file:///Users/you/Code/openclaw"   # local, no clone
skill_paths  = ["skills/**/SKILL.md"]
license_allow = ["*"]                              # self-owned
```

See `harvest.example.toml` at the repo root for the canonical template.

A nightly cron template ships at
`scripts/com.localkin.kinclaw-harvest.plist` — runs `kinclaw harvest
--all --stage --no-critic` at 03:00 daily. Replace `USERNAME` then
`launchctl load` it. New candidates flow into staging while you sleep;
review them in the morning with `kinclaw harvest --review`.

## Sub-agent dispatch — `spawn`

When a subtask wants a different brain than the pilot's main lineage —
multimodal verification, deep web research, adversarial review — pilot
can dispatch to a specialist child:

```
spawn(soul=researcher, prompt="...", timeout_s=180)
  → child stdout (text)
```

The child boots from `souls/<name>.soul.md`, runs its own toolchain
on its own model, and returns a string. **Hierarchical** (not peer),
**synchronous** (not ambient), and kernel-capped at depth 1 — children
cannot themselves spawn. Sub-agent dispatch ≠ multi-agent: peer-swarm
coordination stays an explicit non-goal in the kernel.

Four specialists ship in `souls/`:

| Soul | Brain | When to dispatch |
|---|---|---|
| `researcher` | `kimi-k2.6:cloud` (1T, 256k ctx) | external facts, deep web research |
| `eye` | `kimi-k2.6:cloud` (multimodal) | AX-blind UI verification (canvas, dense icons) |
| `critic` | `minimax-m2.7:cloud` | adversarial second opinion on plans / forge'd skills |
| `coder` | `deepseek-v4-pro:cloud` | re-implement an external SKILL.md as KinClaw exec form (used by `harvest --inspire`) |

Different labs on purpose: pilot + researcher + eye on Moonshot Kimi,
critic on Minimax, coder on DeepSeek — different model lineage means
different blind spots, which is the whole point of asking for a second
opinion (or a different style).

Opt in via `permissions.spawn: true` in the soul. Specialists default
to `false` — even if a child somehow got the schema it can't dispatch.
See `pkg/skill/spawn.go` for the implementation.

## CLI reference

```
kinclaw -soul PATH                  Launch REPL with a specific soul
kinclaw -soul PATH -exec S          Run one message, print response, exit
kinclaw -soul PATH -cleanup-apps    On exit, quit any apps kinclaw started
                                    (preserves apps you already had open)
kinclaw -login                      Claude OAuth PKCE (Max subscription)
kinclaw -soul PATH -debug           Print tool calls & raw API traffic

Subcommands (own their own flag sets):
  kinclaw serve                     Floating chat UI in the browser
  kinclaw serve -port 8020          (default: 127.0.0.1:8020)
  kinclaw serve -soul X -port N
  kinclaw serve --replay <jsonl>    Replay a recorded session
  kinclaw serve --no-record         Skip session recording for this run
  kinclaw serve -h                  Full serve help

  kinclaw probe Notes               Audit one app's AX tree, get a verdict
  kinclaw probe -json com.apple.Notes
  kinclaw probe -batch < ids.txt    CSV scan for many apps (auto-cleanup)
  kinclaw probe -h                  Full probe help

  kinclaw harvest                   Scan external repos, curator triages
                                    candidates, stages yes/maybe ones
  kinclaw harvest --review          Show staged + verdict + reason
  kinclaw harvest --accept <id>     Coder forges this one → ./skills/<name>/
  kinclaw harvest --diff            Dry-run; triage but write nothing
  kinclaw harvest --no-judge        Cron-cheap: just refresh caches, no LLM
  kinclaw harvest -h                Full harvest help

In-REPL commands:
  /soul [name]     List / switch soul files
  /reload          Re-read the current soul + discover new skills
  /skills          List active skills
  /history         Show session messages
  /info            Version / soul / model / skill count / tokens
  /quit            Exit
```

### `kinclaw probe` — 1-second app audit

Before driving a new app, run `kinclaw probe <name>` to see if its AX
surface is rich enough for the `ui` claw, or whether you'll need to fall
back to `input` keystrokes / vision. Four verdicts:

```
🟢 rich     — `ui` claw alone drives it (≥ 50 nodes, ≥ 5 actionable)
🟡 shallow  — `ui` + `input` (cmd-keys / type-text) hybrid
🟠 blank    — needs `record` + screen + vision (menubar app, hostile shell)
🔴 dead     — process didn't open (TCC / sandbox / not installed)
```

The same probe, fed bundle IDs from stdin, produced the 50-app validation
that recorded **94% controllable, 88% pure-AX** on a real dev Mac — empirical
evidence that the 5-claw thesis holds. It now ships in the box.

## Benchmarks

kinclaw runs against multiple public computer-use benchmarks. See
[`benchmarks/`](benchmarks/) for the full portfolio + status board.

**First reference score (v1.15.0, 2026-05-08):**

```
kinclaw v1.15.0 + Kimi-K2.5(cloud) on macbench v0.1
  IMPLEMENTED:  101 / 150  =  67.3%
  STRICT:       101 / 369  =  27.4%
```

For context, Anthropic Computer Use scores ~38% on
[OSWorld](https://os-world.github.io) (Linux desktop). macbench
benchmarks a different surface (macOS-native apps), so the numbers
aren't directly comparable, but the methodology + scoring discipline
are the same.

**Home court — macbench (we wrote it):**

kinclaw is the reference agent for
**[macbench](https://github.com/LocalKinAI/macbench)**, the first
publicly published macOS-native computer-use benchmark. The v0.1
paper is archived on Zenodo at concept DOI
[`10.5281/zenodo.20094244`](https://doi.org/10.5281/zenodo.20094244)
(CC-BY-4.0, single PDF bundles EN + 中文; mirrored at
[localkin.dev/papers/macbench](https://www.localkin.dev/papers/macbench)).
**369 task slots** across 15 macOS categories (Finder / Safari /
Mail / Notes / Calendar / Reminders / Settings / Terminal / Pages /
Numbers / Keynote / Music / Photos / Maps / multi-app). Same
three-file pattern as [OSWorld](https://github.com/xlang-ai/OSWorld)
(`task.json` + `setup.sh` + `eval.sh`), but adapted for the macOS
app surface OSWorld can't reach. v0.1 ships 150 implemented + 219
stubs (real prompts, no setup/eval yet); fill rate over v0.2 → v1.0
is roughly 30-50 stubs/month.

```bash
git clone https://github.com/LocalKinAI/macbench ../macbench
cd kinclaw
make warmup        # pre-flight: build + sign + verify TCC + brain + kits
make bench         # auto-warmup + run all 369 task slots (219 stubs skip in 0 ms)
make bench-record  # same but records each task to mp4 via kinrec
SKIP_WARMUP=1 make bench   # skip warmup (fast dev iteration)
```

**Cross-validation portfolio (designed, not yet implemented):**

| Benchmark | What it tests | kinclaw fit | Status |
|---|---|---|---|
| [WebArena](benchmarks/webarena/) | Live web tasks (Reddit / GitLab / shopping clones in Docker) | 🟢 high — kinclaw's web claw IS Playwright | designed |
| [OSWorld](benchmarks/osworld/) | Linux desktop tasks in a VM | 🟡 vision-only fallback | designed (with caveats) |
| [Online-Mind2Web](benchmarks/online-mind2web/) | Live web tasks on real public sites | 🟡 likely high | investigation |
| Mind2Web (original) | Static action prediction | 🔴 architectural mismatch | skipped |

The benchmark portfolio is **agent-agnostic by design** — every
adapter accepts `AGENT=...` + `AGENT_ARGS='...{prompt}...'`, so
Anthropic Computer Use, OpenAI CUA, or any binary that takes a
prompt and drives macOS / a browser can plug in. kinclaw is our
reference implementation, not the only one allowed.

## Roadmap (post-1.4)

What's shipped, what's next, and what's an explicit non-goal.

**Shipped (1.0–1.5)**: 5 claws, soul clone, skill forge with v2 quality
gate, sub-agent dispatch (4 specialists), `kinclaw probe` AX audit,
`kinclaw harvest` skill ETL pipeline + `--inspire` re-forge mode,
background-safe input via `target_pid`, batched AX IPC
(`Element.GetMany`, 2-5× tree dump speedup), launchd cron template,
cross-session memory.

**Near-term v1.6+ candidates** (fluid):

- ~~**`kinclaw memory`**~~ — shipped in v1.18 (list / search / forget /
  learned / sessions).
- **`kinclaw doctor`** — sidecar health check (TTS / STT / SearXNG /
  Playwright / kinrec). New-user pain point #1.
- **Observer subscriptions** in `kinax-go` — push-based AX event
  notifications (`AXObserverCreate` + CFRunLoop). Pairs with a TTL
  element cache for quasi-realtime UI tracking. *Note: `kinax-go`
  v0.2.0 shipped `GetMany` batched fetch, not Observer — that's still
  ahead.*
- **Homebrew tap** — `brew install localkinai/tap/kinclaw`.
- **More specialists** — `quick` (DeepSeek Flash, fast yes/no
  verifications), `linguist` (GLM 5.1, EN ↔ 中文 style rewrite).

**Apple-cert blocked** (待 $99 Apple Developer Program 解锁):

- **DMG + signing + notarization** — `kinclaw.localkin.ai/download`
  one-click install for non-developers.
- **Wails console + 🦞 menu bar** — visible swarm UI on top of CLI.
- **Relay service** — reach your own KinClaw from anywhere
  (`kinclaw.localkin.ai`, the actual monetization layer).
- **iOS Shortcuts / Siri** integration.

**Explicit non-goals** (not changing):

- ❌ **Multi-agent peer swarm in the kernel.** Sub-agent dispatch
  (`spawn`, hierarchical, depth-1) is OK and shipped. AutoGen-style
  peer coordination belongs in the LocalKin platform layer, not in
  KinClaw itself.
- ⚠️ **Cross-platform — reversed 2026-05-12.** Linux Phase 2-5 + Windows
  Phase 6 both landed the same day. macOS remains the primary, tested
  daily-driver target. Linux and Windows are "code complete, awaiting
  community runtime testing" per
  [issue #1](https://github.com/LocalKinAI/kinclaw/issues/1) — see the
  feature-parity table at the top of this README + `souls/pilot_linux.soul.md`
  / `souls/pilot_windows.soul.md` for the per-OS surface.
- ❌ **Token-markup pricing.** Software stays MIT. Revenue model is
  the relay service when it ships.
- ❌ **Fine-tuned KinClaw-specific model.** Brain stays swappable.
- ❌ **OSWorld / benchmark leaderboard chasing.** Real-app,
  real-task validation (`kinclaw probe -batch` 50-app reports) is
  what we report.
- ❌ **Rewriting in Rust / Swift.** `openclaw` (private Rust port
  experiment) hit the `objc2` interop wall before the architectural
  fun parts. Go + purego is the right shape.

## Why not Computer Use / Operator?

Both of those products are architecturally single agents with a
fixed toolbelt, clicking around a virtualized browser in Anthropic's
or OpenAI's infrastructure. KinClaw:

- **Runs on your actual Mac**, not a container. macOS native
  (ScreenCaptureKit, CGEvent, Accessibility) rather than a
  virtualized X11.
- **Go, not Python** — single binary, no `pip install` drift, no
  environment setup.
- **Swarm, not singleton** — Soul Clone is table stakes.
- **Local-first brain option** — Ollama / Qwen3-VL for
  privacy-sensitive tasks.
- **Self-forges skills** — when a skill doesn't exist, it's written
  and registered at runtime. No competitor has this.

None of this is magic; it's boring engineering applied consistently.

## Contributing

```bash
git clone https://github.com/LocalKinAI/kinclaw
cd kinclaw
go build -o kinclaw ./cmd/kinclaw
go test ./...
```

A soul file that does something interesting is the best first
contribution. PRs adding `souls/community/<your_name>.soul.md`
welcome.

## License

MIT. See `LICENSE`.

## See also

- The four KinKit libraries (table above).
- [LocalKin.dev papers](https://www.localkin.dev/papers) —
  architectural essays: Grep Retrieval, Thin Soul + Fat Skill,
  Autonomous Heart, Embedded Dylib (paper #9 explains this repo's
  claw layer), more.
