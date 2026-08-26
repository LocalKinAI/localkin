---
name: imsg_send
description: Send an iMessage or SMS via the imsg CLI (requires brew install steipete/tap/imsg)
command: [imsg, send]
args: ["--to", "{{recipient}}", "--text", "{{message}}"]
schema:
  recipient:
    type: string
    description: Phone number (e.g. +14155551212) or Apple ID email address
  message:
    type: string
    description: Text content of the message to send
---

# imsg_send

Sends a single iMessage or SMS through macOS Messages.app using the `imsg` CLI.
The binary decides iMessage vs SMS automatically; force with `--service` via raw imsg.

Not covered by this wrapper: listing chats, reading history, watching for new messages,
or attaching files. Use `imsg chats`, `imsg history`, `imsg watch`, or `imsg send --file`
directly for those.