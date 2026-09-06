---
name: learn
description: |
  The agent's cross-session notebook at ~/.kinclaw/learned.md, organized
  by bundle_id / topic. The kernel reads it at every boot and injects
  it into the system prompt (when it outgrows the prompt budget you see
  a topic index plus the newest notes — use action=recall for the rest).

  action=add (default) — append one lesson after a task:
    - AX schema quirks (depth needed, weird matchers)
    - Working keyboard / shell shortcuts that beat ui clicks
    - Failed approaches + their error codes (so you don't retry next time)
    - First-launch modal patterns
    - bundle_id spelling (some apps use lowercase like com.apple.mail)
  action=recall topic=X — print one section (case-insensitive match)
  action=list — print the topic index

  Appends go to a section keyed on `topic` (usually a bundle_id).
  Creates the section if it doesn't exist; appends bullet lines if it
  does. No-op if the exact same line already exists (idempotent).
command:
  - sh
  - -c
  - |
    ACTION="${1:-add}"
    TOPIC="$2"
    NOTE="$3"
    FILE="$HOME/.kinclaw/learned.md"
    mkdir -p "$HOME/.kinclaw"
    [ -f "$FILE" ] || printf '# KinClaw — learned across sessions\n\n' > "$FILE"

    case "$ACTION" in
      list)
        # Topic index: every ## header with its bullet count.
        awk '/^## /{ if (t!="") printf "%s (%d)\n", t, n; t=substr($0,4); n=0; next }
             /^- /{ n++ }
             END{ if (t!="") printf "%s (%d)\n", t, n }' "$FILE"
        exit 0 ;;
      recall)
        [ -z "$TOPIC" ] && { echo "topic required for recall" >&2; exit 1; }
        OUT=$(awk -v q="$(printf '%s' "$TOPIC" | tr 'A-Z' 'a-z')" '
             /^## /{ inhit = index(tolower(substr($0,4)), q) > 0 }
             inhit { print }' "$FILE")
        [ -z "$OUT" ] && { echo "no section matching: $TOPIC (try action=list)"; exit 0; }
        printf '%s\n' "$OUT"
        exit 0 ;;
      add|"") ;;
      *) echo "action must be add | recall | list (got $ACTION)" >&2; exit 1 ;;
    esac

    [ -z "$TOPIC" ] && { echo "topic required" >&2; exit 1; }
    [ -z "$NOTE" ] && { echo "note required" >&2; exit 1; }

    # Idempotent: if exact line already exists, no-op. The `--` is
    # important — leading "- " in $LINE would otherwise get parsed as
    # a grep flag. Same for the section header check below.
    LINE="- $NOTE"
    if grep -Fq -- "$LINE" "$FILE" 2>/dev/null; then
      echo "already known: $TOPIC :: $NOTE"
      exit 0
    fi

    # Append to existing section if header exists, else create section.
    HEADER="## $TOPIC"
    if grep -Fqx -- "$HEADER" "$FILE"; then
      # Insert line right after the section header using awk so order is preserved.
      awk -v hdr="$HEADER" -v line="$LINE" '
        $0 == hdr { print; print line; next }
        { print }
      ' "$FILE" > "$FILE.new" && mv "$FILE.new" "$FILE"
    else
      printf '\n%s\n%s\n' "$HEADER" "$LINE" >> "$FILE"
    fi
    echo "learned: $TOPIC :: $NOTE"
  - "_"
args:
  - "{{action}}"
  - "{{topic}}"
  - "{{note}}"
schema:
  action:
    type: string
    description: |
      add (default) | recall | list. add appends a note; recall prints one topic's section; list prints the topic index.
  topic:
    type: string
    description: |
      Section header to file the note under (add) or look up (recall) — typically a bundle_id like "com.apple.calculator", "com.apple.Notes", or a generic category like "Common: focus protection". Required for add and recall.
  note:
    type: string
    description: |
      Single-line lesson learned (add only). Concise. No outer "- " (added automatically). Examples — "AX tree depth ≥ 6 to see number buttons", "cmd+N + type more reliable than ui click 'New Note'", "AXError -25205 means the element is offscreen / collapsed".
timeout: 10
---

# learn — append to the cross-session notebook

KinClaw's persistence layer for Genesis Protocol. Every successful
task or hard-won failure is an opportunity to make next session
smarter. This skill is the standardized way to write into
`~/.kinclaw/learned.md` — kernel auto-loads that file at boot and
injects it into the agent's system prompt. (Until v1.18 this skill wrote
to `~/.localkin/learned.md` while the kernel read `~/.kinclaw/` — the
kernel still reads the old file so nothing written there is lost.)

## Idempotent

Calling `learn` with the same topic + note twice is a no-op. Safe to
spam without polluting the notebook.

## Examples

```
learn topic=com.apple.calculator note="AX tree depth ≥ 6 to see number buttons"
learn topic=com.apple.Notes note="cmd+N more reliable than ui click 'New Note'"
learn topic=com.apple.reminders note="ui click description='Add Reminder' fails with AXError -25205; use cmd+N + type"
learn topic="Common: focus protection" note="osascript activate from Terminal-driven KinClaw rarely takes frontmost"
learn action=list
learn action=recall topic=com.apple.Notes
```

## Why a SKILL.md and not native

Pure shell + awk. No Go state. The notebook lives at a known path
that the kernel already reads — this skill is just an idempotent
append helper. Nothing here belongs in the kernel.
