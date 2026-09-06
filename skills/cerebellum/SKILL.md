---
name: cerebellum
description: |
  Single fast-execution skill for macOS / Linux / Windows app operations.
  The "cerebellum" to the LLM's "cerebrum" — the brain decides intent,
  this skill executes the canonical multi-step pattern in one syscall
  sequence (no LLM round-trip per step).

  Inspired by the same architecture as the LocalKin robot car: the
  high-level brain plans ("rename this file to X"), the cerebellum
  daemon executes deterministically (`mv old new`) at near-zero
  latency.

  USAGE:
    cerebellum "<category> <action> [args...]"

  Categories:
    macOS    finder, notes, mail, calendar, reminders, settings,
             safari, music, photos, maps, pages, numbers, keynote,
             terminal, multi, web                       (16 cats, 478 actions)
    Linux    linux-files, linux-apps, linux-settings,
             linux-clipboard                            (4 cats)
    Windows  windows-files, windows-apps, windows-settings,
             windows-clipboard                          (4 cats)
  Run with no args to see the full action menu.

  Examples:
    cerebellum "finder rename /old.txt /new.txt"
    cerebellum "finder set_view list"
    cerebellum "finder set_sort Date"
    cerebellum "notes create 'Title' 'body content'"
    cerebellum "notes pin 'KinBench Pinned 164'"
    cerebellum "notes export_pdf 'My Note' /Users/me/out.pdf"
    cerebellum "mail draft 'subject' 'body' /path/attach.pdf"

  Why this exists: Cloud-brain LLM round-trips are 5-15s each. Solving
  "rename a file" via raw shell + verify takes 4-6 round-trips ≈ 30-60s.
  Solving via cerebellum takes 1 round-trip + a 50ms shell call. 6-9× speedup
  on file-system tasks. The brain still has to PARSE the prompt and pick
  the right cerebellum command — that part is irreducible thinking. The
  goal is to remove the *non-thinking* round-trips (running a command,
  reading its output, deciding it succeeded, calling verify).

  When NOT to use: tasks that genuinely require multi-step planning where
  each step depends on the result of the previous (e.g. "find the file
  whose name contains X then move it"). For those, fall back to the raw
  `shell` claw and let the LLM compose the steps.
command:
  - sh
  - -c
  - |
    "$SKILL_DIR/cerebellum.sh" "$1"
  - "_"
args:
  - "{{cmd}}"
schema:
  cmd:
    type: string
    description: |
      Full cerebellum command as one string. Use shell-style quoting for
      values containing spaces. Format: "<category> <action> [args...]".
      e.g. "finder rename /old.txt /new.txt" or "notes pin 'KinBench Pinned 164'".
      Pass empty string to print the action menu.
    required: true
timeout: 60
---

# cerebellum — fast cross-platform operation dispatcher

A single skill that wraps battle-tested canonical sequences for
common app patterns. Single LLM round-trip in, one syscall sequence
out. Designed to eliminate the LLM-tax on "obvious" multi-step
operations. Per-OS backends:
  - macOS   — AppleScript / osascript / shell
  - Linux   — shell + POSIX tools (xdg-open, gtk-launch, gsettings, pactl, …)
  - Windows — bash → powershell.exe (Move-Item, AppActivate, WMI, UIA, …)

## When to use

For ANY of these patterns, prefer cerebellum over raw `shell` + multi-step:
- File ops (rename, move, copy, zip, mkdir, trash)
- Finder view/sort/sidebar settings
- Notes CRUD (create, append, delete, pin, format, export)
- Mail draft creation
- Tag operations

## Architecture

```
LLM (cerebrum)         cerebellum               OS
──────────────         ──────────               ──
"rename old.txt
 to new.txt"     ───►  parse intent       ───►  mv old.txt new.txt
                       run canonical      ◄───  return ok
                       (handles retries,
                        iCloud sync waits,
                        kHasCustomIcon flag,
                        etc — all without
                        bothering the LLM)
                                          ◄───  ok
                 ◄───  "done"
```

Internal verify, retry, and platform quirks are baked in — the LLM
doesn't need to know that macOS 14+ removed the AppleScript `pinned`
property, or that Notes' table requires `Cmd+Opt+T` after AXFocus,
or that Mail draft saving needs `tell app Mail to launch` + sleep.
All of that lives in the cerebellum.
