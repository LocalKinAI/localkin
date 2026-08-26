# Cross-language OS-control library survey — idea harvest for KinKit

**Date:** 2026-04-28
**Scope:** Map the live OS-control library ecosystem in Swift, Python, and
Rust, identify design patterns worth borrowing into KinKit (the four Go
libs powering KinClaw), and call out specific anti-patterns to avoid.
**This is not a migration plan.** Stay in Go; harvest ideas.

KinKit today:

| Lib | Role | Cross-lang cousin |
|---|---|---|
| `sckit-go` | ScreenCaptureKit screenshots / streams | xcap (Rust) |
| `input-go` | CGEvent mouse + keyboard | pyautogui (Py), enigo (Rust), autopilot-rs (Rust) |
| `kinax-go` | Accessibility API UI tree | AXSwift / AXorcist (Swift), axcli (Rust) |
| `kinrec` | Screen + audio recorder | xcap recorder mode (Rust) |

---

## TL;DR

The closest **active** Mac-native cousins worth tracking right now:

- **AXorcist** (Swift) — `v0.1.2` released **today** (2026-04-28). 266★. The modern descendant of the dormant AXSwift. `@MainActor`-isolated, `AsyncStream` for permission changes, fuzzy-match selector criteria. Skim before any kinax-go refactor: https://github.com/steipete/AXorcist
- **axcli** (Rust, macOS-only, 24★, active) — solo dev, hits exactly KinClaw's surface (AX + ScreenCaptureKit + `CGEventPostToPid`). Headline trick is **background-safe input via `CGEventPostToPid`**, which input-go should adopt.

The classical references (AXSwift, pyautogui, autopilot-rs) are all dormant
or stalled. autopilot-rs faded because it kept a single global namespace
and no traits; AXSwift punts caching/event-ordering to a separate library
(Swindler); pyautogui's maintainer admits 9-year-old gaps (CJK/IME,
multi-monitor, Wayland).

Most surprising finding: **no one in this space ships async APIs** — xcap,
enigo, axcli, terminator are all sync, recorders use threads + channels.
Validates Go's blocking-with-goroutines model. No pressure to add
`context.Context` everywhere just because Rust did.

The macOS gap is the moat. **terminator-rs** (the most-funded Rust
computer-use SDK, $2.8M raised, 1.4k★) is *explicitly Windows-only*. axcli
is the only active Mac-native Rust cousin and it has 24 stars. KinClaw's
Mac-native craft has no serious Rust competitor in 2026.

---

## Library map

### Swift / macOS AX

| Lib | Status | Notes |
|---|---|---|
| [AXSwift](https://github.com/tmandry/AXSwift) | **Dormant** — last release 2021-09, last push 2023-07, 14 open issues since 2018 | The classical Swift wrapper. 406★. Still useful as a *reference* for typed enum constants and the `getMultipleAttributes` batch pattern. |
| [AXorcist](https://github.com/steipete/AXorcist) | **Active** — `v0.1.2` released 2026-04-28 | Modern descendant. `@MainActor` isolation, `AsyncStream<Bool>` permission changes, fuzzy match criteria (`exact` / `contains` / `regex` / `prefix` / `suffix`), JSON `AXCommandEnvelope` for CLI use. **Skim this.** |
| [Swindler](https://github.com/tmandry/Swindler) | Reference only | The caching/promise/event-ordering layer above AXSwift. Read for cache-invalidation strategy. |

### Python / cross-platform

| Lib | Status | Notes |
|---|---|---|
| [pyautogui](https://github.com/asweigart/pyautogui) | **Stalled** — last commit 2023-06, no GitHub releases | 12.4k★. Still the cultural reference for "input synthesis lib" but maintainer-acknowledged gaps in Wayland, multi-monitor, non-Latin keyboards, headless. |

### Rust / cross-platform & macOS

| Lib | Status | Notes |
|---|---|---|
| [enigo](https://github.com/enigo-rs/enigo) | **Active** — commits 2026-03 | 1.7k★. Dominant input successor. Trait-split `Mouse` + `Keyboard` + `Agent`. Recently moved off deprecated `cocoa` to `objc2`. |
| [xcap](https://github.com/nashaofu/xcap) | **Very active** — `v0.9.4` 2026-04-09 | 960★. Cross-platform screenshot + WIP recorder. Releases every 2-3 weeks. Channel-based frame delivery. |
| [autopilot-rs](https://github.com/autopilot-rs/autopilot-rs) | **Maintenance mode** — last commit 2025-10 | 420★, no crates.io release. Got an `objc2` migration in 2025 but no new features. Replaced in practice by enigo + xcap. |
| [terminator-rs](https://github.com/mediar-ai/terminator) | **Very active, Windows-only** | 1.4k★. Computer-use SDK with selector DSL + recorder + MCP agent. Raised $2.8M. macOS/Linux explicitly "No". |
| [axcli](https://github.com/andelf/axcli) | **Active, niche** — commit 2026-04-27 | 24★, solo. Closest Rust cousin to KinClaw. AX + ScreenCaptureKit + `CGEventPostToPid`. |
| [eiz/accessibility](https://github.com/eiz/accessibility) | **Stable, low-velocity** | 87★. Low-level macOS AX FFI floor used by axcli. |
| [AccessKit](https://github.com/AccessKit/accesskit) | Active | 1.4k★. **Wrong end of the wire** — for toolkits *publishing* a11y, not consumers reading it. Schema design worth a glance. |

---

## Harvest by target lib

### `kinax-go` (AX claw) — biggest opportunity

1. **Batch attribute fetcher** — `AXUIElementCopyMultipleAttributeValues`
   in one IPC instead of N. AXSwift's `getMultipleAttributes(.role, .title, ...)`
   is the model. **2-5× faster tree dumps.** Mechanical port.

   ```go
   func (e *Element) GetMany(attrs ...Attribute) (map[Attribute]any, error)
   ```

2. **Observer → Go channel bridge.** Port AXSwift's `Observer.swift` shape
   over purego (`AXObserverCreate`, `AXObserverGetRunLoopSource`,
   `CFRunLoopAddSource`). Push events instead of polling. Required for
   "react to focus change" agent skills and for invalidating any cache
   layer.

   ```go
   obs, _ := kinax.NewObserver(pid)
   events, _ := obs.Subscribe(elem, NotifFocusChanged, NotifWindowCreated)
   go obs.Run(ctx)  // CFRunLoop on a locked OS thread
   for ev := range events { ... }
   ```

3. **Typed enum constants** — copy AXSwift's [`Constants.swift`](https://github.com/tmandry/AXSwift/blob/main/Sources/Constants.swift)
   wholesale (100+ Attribute, 47 Role, 35 Notification cases). Backed by
   `string` so AX C strings interop unchanged. Pure DX win.

   ```go
   type Attribute string
   const (AttrRole Attribute = "AXRole"; AttrTitle Attribute = "AXTitle"; ...)
   ```

4. **3-return signature** for attribute getters: `(val, present, err)`.
   AX has three failure modes (truly errored / unsupported attribute /
   no value). Swift collapses to `throws + Optional` and forces `try!`
   spam. Go can do better.

5. **Selector enum DSL** — borrow from terminator's [`selector.rs`](https://github.com/mediar-ai/terminator/blob/main/crates/terminator/src/selector.rs):
   `Role{role,name}`, `Chain`, `Has`, `Nth`, `RightOf/LeftOf/Above/Below`,
   `And/Or/Not`. Today kinax exposes raw AX attributes; a typed `Selector`
   makes it Playwright-like. Pairs naturally with the `eye` specialist soul.

6. **TTL element cache invalidated by observer events.** The Swindler-
   layer idea AXSwift skips. Defer until #2 lands; pairs naturally.

### `input-go` (input claw)

1. **`CGEventPostToPid` for background-safe input** (axcli's headline
   trick). Click/type without focus steal — proven on Lark / VSCode /
   Chrome. **Highest differentiation value** vs pyautogui-style tools.
   Expose as `--target-pid` strategy.

2. **`Hold(key) func()` context-manager pattern** (pyautogui). Returns
   a release closure → `defer input.Hold("shift")()`. Panic-safe pairing
   that's currently leakable in input-go's `KeyDown`/`KeyUp` API.

3. **`MoveTo(x, y, WithDuration(d), WithTween(fn))`** (pyautogui). Linear
   default; tweens in a sub-pkg so core stays dependency-free. Humanizing
   motion is a frequent computer-use-agent ask.

4. **`Press(keys []string, WithRepeat, WithInterval)`** (pyautogui). Batched
   keystrokes with pacing as a discoverable arg, not buried in caller loops.

5. **Trait-split + serde Token type** (enigo). Split monolithic API into
   `Mouser`, `Keyboarder` interfaces plus a `Replayer` consuming a serialized
   `Token` stream. Genesis already records workflows — make the action type
   the wire format, not a parallel DSL.

   ```go
   type Token interface { ... }   // MoveMouse / Click / Text / Key / Scroll
   type Replayer interface { Apply(Token) error }
   // YAML/JSON marshal natively → recordings ARE the action language
   ```

### `kinrec` (recorder)

1. **Channel-based recorder API** (xcap). Replace any blocking `Record(path)`
   with `(*Recorder, <-chan Frame)` + `Start`/`Stop`. Lets callers tee
   frames to live OCR / streaming / frame-diff prompts without re-encoding.
   Idiomatic Go.

### `sckit-go` (screen claw)

1. **Domain-specific error type** with sentinel variants (`ErrNoPermission`,
   `ErrDisplayGone`, `ErrSCKitInternal`, etc.) — xcap's `error.rs` shape.
   Cleaner `errors.Is` than string-matching purego return codes.

---

## Anti-patterns to deliberately skip

- **No image search in input-go.** pyautogui's `locateOnScreen` is the
  most-cited footgun: 1-2s on 1080p, breaks under DPI/Retina, silently
  flips between `None` and `ImageNotFoundException` across versions.
  Vision belongs in the `eye` specialist soul, not in the input claw.
  ([pyautogui #4](https://github.com/asweigart/pyautogui/issues/4),
  [#321](https://github.com/asweigart/pyautogui/issues/321))

- **No package-level mutable globals.** pyautogui's `PAUSE` / `FAILSAFE` /
  `DARWIN_CATCH_UP_TIME` are unconfigurable per-call: every action sleeps
  `PAUSE` after itself, so a 10-key hotkey costs an extra second.
  Pacing is a per-call option in input-go; abort is a `context.Context`
  the caller owns.

- **No throws-everywhere AX wrapping.** AXSwift's example uses 8 `try!`s
  in a row because it collapses three AX failure modes into one. Go's
  `(val, present, err)` triple is cleaner — diverge from AXSwift here.

- **Don't punt caching to "another library".** AXSwift's design (caching
  lives in Swindler) leaks IPC cost to every caller. For an *agent*
  re-querying the same elements every tick, this hurts. kinax-go should
  expose an opt-in element snapshot cache (TTL ~50-200ms, observer-
  invalidated) once #2 above lands.

- **No global Mac-thread bypass.** AX functions are main-thread only
  ([Apple Forums #94878](https://developer.apple.com/forums/thread/94878);
  AXorcist enforces with `@MainActor`). Without an actor system, Go must
  use `runtime.LockOSThread()` on the goroutine that owns the CFRunLoop
  and funnel all `AXUIElement*` calls through it via a request channel.
  **Cross-thread AX calls = #1 source of `kAXErrorCannotComplete` flakiness.**
  Non-negotiable.

- **No autopilot-style global namespace.** autopilot-rs faded because
  it kept `autopilot::mouse::move_to` style flat APIs with no traits, no
  composition, no error type beyond `String`. Don't repeat that on the
  Go side — keep methods on `*Element` / `*Recorder` / `*Mouser` types.

---

## Top-12 unified harvest list (ROI-ordered)

| # | Idea | Target lib | Effort | Source |
|---|---|---|---|---|
| 1 | `GetMany` batch IPC for AX attributes | kinax-go | hours | AXSwift |
| 2 | `CGEventPostToPid` background-safe input | input-go | days | axcli |
| 3 | `Hold(key) func()` context-manager | input-go | hours | pyautogui |
| 4 | Typed enum constants (Attribute / Role / Notification) | kinax-go | hours | AXSwift |
| 5 | Channel-based recorder API `(*Recorder, <-chan Frame)` | kinrec | day | xcap |
| 6 | Domain-specific error sentinels (`ErrNoPermission` etc.) | sckit-go, kinrec | hours | xcap |
| 7 | `MoveTo(..., WithDuration, WithTween)` | input-go | day | pyautogui |
| 8 | `Press(keys, WithRepeat, WithInterval)` | input-go | hours | pyautogui |
| 9 | 3-return signature `(val, present, err)` | kinax-go | refactor | (own) |
| 10 | Observer → Go channel bridge | kinax-go | days | AXSwift |
| 11 | Selector enum DSL (Role / Chain / Has / Nth / spatial) | kinax-go | days | terminator-rs |
| 12 | Trait-split + serde `Token` type for replay | input-go | days | enigo |

**Top 3 are highest-leverage / lowest-effort:**

- **#1** ships measurable speedup (2-5× tree dumps) for mechanical work.
- **#2** is the differentiation play vs pyautogui-style tools — clicks
  without focus steal, proven on real apps.
- **#3** removes a leak class with ~30 LOC.

---

## Strategic notes

- **The macOS-native gap is the moat.** terminator-rs ($2.8M raised, 1.4k★)
  is explicitly Windows-only. axcli (the only active Mac-native Rust cousin)
  has 24 stars. KinClaw's Mac-native craft has no serious Rust competitor.
- **No one in this space is async.** xcap, enigo, axcli, terminator are
  all sync; recorders use threads + channels. Don't add `context.Context`
  everywhere just because Rust does — Go's blocking-with-goroutines model
  is correct here.
- **AccessKit is not a consumer.** It's for toolkits *publishing* a11y
  (Egui, Slint, etc.). Skip for kinax-go borrowing. Schema worth a glance
  if KinKit ever needs an export format for the AX tree.
- **The `objc2` wall is real.** autopilot-rs survived 2025 only because
  someone did the `cocoa → objc2` migration in July. enigo did the same.
  This is the Rust ObjC interop wall Jacky's "openclaw" experiment hit.
  Validates the Go + purego choice.
- **AXorcist (released today) is the canonical "what to skim before
  refactoring kinax-go".** It's the modern shape of what AXSwift would be
  if it had stayed maintained. Fuzzy match criteria + AsyncStream pattern
  are likely better fits for an LLM-driven agent than AXSwift's lower-
  level shape.

---

## Sources

**Swift:**
- https://github.com/tmandry/AXSwift
- https://github.com/steipete/AXorcist (active, released 2026-04-28)
- https://github.com/tmandry/Swindler
- https://developer.apple.com/forums/thread/94878 (AX threading)

**Python:**
- https://github.com/asweigart/pyautogui
- https://pyautogui.readthedocs.io
- Issues #4 / #33 / #111 / #137 / #321

**Rust:**
- https://github.com/enigo-rs/enigo
- https://github.com/nashaofu/xcap
- https://github.com/autopilot-rs/autopilot-rs
- https://github.com/mediar-ai/terminator
- https://github.com/andelf/axcli
- https://github.com/eiz/accessibility
- https://github.com/AccessKit/accesskit

**Compiled by:** 3 parallel research agents (web fetch + GitHub repo
inspection), synthesis on 2026-04-28.
