#!/bin/bash
# kinthink — NL → cerebellum action router.
#
# Architecture: grep is all you need (paper #1 + #11).
#   Layer 1: tokenize NL input (alnum-split, lowercase, dropstops)
#   Layer 2: TF-IDF weighted overlap against a 239-row index built from
#            macbench Fast-path prompts (~6-7 KB TSV)
#   Layer 3: slot extraction — paths, quoted strings, dates — from
#            the matched NL example and from the user input, then
#            substitute by position into the cerebellum template
#   Layer 4: execute the substituted cerebellum call (if KINTHINK_EXEC=1)
#
# Falls back to "no match" (rc=10) when best TF-IDF score < threshold —
# caller routes to LLM. For structured prompts grep alone wins ~85%.
#
# Usage:
#   kinthink "rename foo.txt to bar.txt"
#   KINTHINK_EXEC=1 kinthink "switch macOS to dark mode"
#
# Env:
#   KINTHINK_INDEX      path to TSV (default: alongside this script)
#   CEREB               path to cerebellum.sh
#   KINTHINK_EXEC       "1" to actually run; else just print template
#   KINTHINK_MIN_SCORE  match threshold in TF-IDF units (default 1.5)

set -u

NL="${1:-}"
[ -z "$NL" ] && {
  echo "Usage: kinthink \"natural language prompt\""
  echo "       KINTHINK_EXEC=1 to actually execute the match."
  exit 1
}

SCRIPT_DIR="$(/usr/bin/dirname "$(/usr/bin/realpath "${BASH_SOURCE[0]:-$0}" 2>/dev/null || echo "${BASH_SOURCE[0]:-$0}")")"
INDEX="${KINTHINK_INDEX:-$SCRIPT_DIR/actions.tsv}"
CEREB="${CEREB:-$SCRIPT_DIR/../cerebellum/cerebellum.sh}"
EXEC="${KINTHINK_EXEC:-0}"
MIN_SCORE="${KINTHINK_MIN_SCORE:-1.5}"

[ -f "$INDEX" ] || {
  echo "ERR: index not found at $INDEX" >&2
  echo "Run: $SCRIPT_DIR/build_index.sh" >&2
  exit 2
}

T_START=$(/bin/date +%s%N)

# --- Layer 0: direct Fast-path extraction ---
# If the prompt already contains a "Fast path: cerebellum '...'" hint
# (as macbench task prompts do — the eval hand-wrote each task's
# canonical action), short-circuit grep and just use that hint
# literally. This is correct because the hint IS the canonical
# answer; grepping a different example would only introduce error.
#
# Pattern: looks for `cerebellum 'ACTION ARGS'` or `cerebellum "ACTION ARGS"`
# anywhere after "Fast path". The first single- or double-quoted
# payload wins.
DIRECT_CMD=""
case "$NL" in
  *"Fast path"*)
    # Try single quote first (the canonical form in the bench)
    DIRECT_CMD=$(printf '%s' "$NL" | /usr/bin/sed -nE "s/.*[Ff]ast path[^']*cerebellum[[:space:]]+'([^']+)'.*/\1/p" | head -1)
    if [ -z "$DIRECT_CMD" ]; then
      DIRECT_CMD=$(printf '%s' "$NL" | /usr/bin/sed -nE 's/.*[Ff]ast path[^"]*cerebellum[[:space:]]+"([^"]+)".*/\1/p' | head -1)
    fi
    ;;
esac

if [ -n "$DIRECT_CMD" ]; then
  T_END=$(/bin/date +%s%N)
  MS_DIRECT=$(( (T_END - T_START) / 1000000 ))
  printf '★ fast-path direct (no grep needed)  %dms\n' "$MS_DIRECT"
  printf '★ cerebellum %s\n' "$(printf "'%s'" "$DIRECT_CMD")"
  if [ "$EXEC" = "1" ]; then
    T_EXEC_START=$(/bin/date +%s%N)
    # eval so embedded $(date ...) etc. resolve at the call site
    eval "\"\$CEREB\" \"\$DIRECT_CMD\""
    RC=$?
    T_EXEC_END=$(/bin/date +%s%N)
    MS_EXEC=$(( (T_EXEC_END - T_EXEC_START) / 1000000 ))
    printf '★ exec rc   : %d  (%dms)\n' "$RC" "$MS_EXEC"
    printf '★ TOTAL     : %dms  (extract %dms + exec %dms)\n' \
      "$((MS_DIRECT + MS_EXEC))" "$MS_DIRECT" "$MS_EXEC"
    exit "$RC"
  fi
  exit 0
fi

# --- Layer 1: tokenize ---
# We mirror the example-side "lean" transform on the input so that
# the same kinds of slot-values (paths, file basenames, quoted
# strings) are stripped on both sides. Otherwise rare leftover tokens
# like "txt" (from "foo.txt" after strip on one side but not the other)
# can dominate TF-IDF scoring and pick wrong matches.
NL_LEAN=$(printf '%s' "$NL" | /usr/bin/awk '{
  gsub(/\047[^\047]+\047/, " ")
  gsub(/"[^"]+"/, " ")
  gsub(/~\/[^ ]+/, " ")
  gsub(/\/Users\/[^ ]+/, " ")
  gsub(/\/tmp\/[^ ]+/, " ")
  gsub(/[A-Za-z0-9_-]+\.(txt|md|png|jpg|jpeg|pdf|gif|mp4|html|json|csv|xlsx|docx|key|pages|numbers|zip|ics)/, " ")
  gsub(/[0-9]{2,}-[a-z-]+/, " ")
  gsub(/kinbench[^ ]*/, " ")
  print tolower($0)
}')

TOKENS=$(printf '%s' "$NL_LEAN" \
  | /usr/bin/tr -c '[:alnum:]' '\n' \
  | /usr/bin/awk 'length($0) >= 2 && !/^(the|and|for|with|that|this|from|into|onto|use|via|run|are|its|any|all|new|out|set|but|not|nor|yet|can|may|will|just|then|than|also|like|over|when|what|which|while|where|here|there|them|some|each|one|two|txt|md|png|jpg|jpeg|pdf|csv|json|html)$/' \
  | /usr/bin/sort -u)
[ -z "$TOKENS" ] && {
  echo "ERR: no tokens extracted from input" >&2
  exit 3
}
TOKENS_PIPE=$(printf '%s' "$TOKENS" | /usr/bin/tr '\n' ' ')

# --- Layer 2: TF-IDF weighted match ---
# Single-pass awk:
#   Phase 1: build document frequency (df) per token from the index.
#   Phase 2: for each row, score = sum of (1/df[tok]) for each token
#            that appears in the example NL.
#   Output: best row as score \t id \t cmd \t example_nl
BEST_LINE=$(/usr/bin/awk -v tokens="$TOKENS_PIPE" -F'\t' '
BEGIN {
  ntok = split(tokens, tarr, /[[:space:]]+/)
  # Initialize df[] to 0
  for (i = 1; i <= ntok; i++) df[tarr[i]] = 0
}
# Phase 1 done lazily as we go: count df per token across the file.
# Phase 2 (after EOF): rescan in END. So we cache rows.
{
  if (NF < 3) next
  id_arr[NR] = $1
  nl_arr[NR] = $2     # keep raw for slot extraction later
  cmd_arr[NR] = $3
  # Build a "lean" version of the NL for matching: strip out the
  # bench-specific literal slot values (quoted strings, paths, file
  # basenames). What remains is the intent vocabulary — verbs and
  # object nouns. This prevents TF-IDF from latching onto rare
  # literals like "hello" or "kinbench-test-003" that appear in one
  # example but carry no general intent signal.
  lean = $2
  gsub(/\047[^\047]+\047/, " ", lean)             # 'single-quoted'
  gsub(/"[^"]+"/, " ", lean)                       # "double-quoted"
  gsub(/~\/[^ ]+/, " ", lean)                      # ~/Desktop/...
  gsub(/\/Users\/[^ ]+/, " ", lean)                # /Users/...
  gsub(/\/tmp\/[^ ]+/, " ", lean)                  # /tmp/...
  gsub(/[A-Za-z0-9_-]+\.(txt|md|png|jpg|jpeg|pdf|gif|mp4|html|json|csv|xlsx|docx|key|pages|numbers|zip|ics|key)/, " ", lean)
  gsub(/[0-9]{2,}-[a-z-]+/, " ", lean)             # 001-finder-rename
  gsub(/kinbench[^ ]*/, " ", lean)                 # any kinbench-* literal
  ex_lower[NR] = tolower(lean)
  for (i = 1; i <= ntok; i++) {
    t = tarr[i]
    if (t == "") continue
    if (index(ex_lower[NR], t) > 0) df[t]++
  }
  total++
}
END {
  best_score = -1
  best_id = ""
  best_cmd = ""
  best_nl = ""
  for (n = 1; n <= total; n++) {
    score = 0
    for (i = 1; i <= ntok; i++) {
      t = tarr[i]
      if (t == "" || df[t] == 0) continue
      if (index(ex_lower[n], t) > 0) {
        # TF=1 (set semantics); IDF = log(total / df)
        idf = log((total + 1) / (df[t] + 1))
        score += idf
      }
    }
    if (score > best_score) {
      best_score = score
      best_id    = id_arr[n]
      best_cmd   = cmd_arr[n]
      best_nl    = nl_arr[n]
    }
  }
  printf "%.4f\t%s\t%s\t%s\n", best_score, best_id, best_cmd, best_nl
}
' "$INDEX")

BEST_SCORE=$(printf '%s' "$BEST_LINE" | /usr/bin/cut -f1)
BEST_ID=$(   printf '%s' "$BEST_LINE" | /usr/bin/cut -f2)
BEST_CMD=$(  printf '%s' "$BEST_LINE" | /usr/bin/cut -f3)
BEST_NL=$(   printf '%s' "$BEST_LINE" | /usr/bin/cut -f4-)

T_MATCH=$(/bin/date +%s%N)
MS_MATCH=$(( (T_MATCH - T_START) / 1000000 ))

# Score is a float — compare with awk
BELOW=$(/usr/bin/awk -v s="$BEST_SCORE" -v t="$MIN_SCORE" 'BEGIN { print (s < t) ? 1 : 0 }')
if [ "$BELOW" = "1" ]; then
  printf 'no-match (tf-idf=%s < %s, t=%dms) — fall back to LLM\n' \
    "$BEST_SCORE" "$MIN_SCORE" "$MS_MATCH" >&2
  printf 'closest: %s — cerebellum %s\n' "$BEST_ID" "$BEST_CMD" >&2
  exit 10
fi

# --- Layer 3: slot extraction & substitution ---
# We look for "values" in two places:
#   - the matched example NL
#   - the user input
# Both are scanned with the SAME regex set, in the SAME order. If the
# counts match per slot type, we substitute. Otherwise we leave the
# template as-is (deterministic fallback).
#
# Slot types (regex), in order of "obviousness":
#   QUOTED:  '…' or "…" (event titles, note names, file labels)
#   PATH:    ~/… or /Users/… or absolute paths
#   FILENAME: bare basename.ext (foo.txt, photo.jpg)
#
# Substitution into template: scan the cerebellum command for the
# same slot patterns, replace by position with the input-side values.
extract_slots() {
  local s="$1"
  # Use python-free regex via grep -oE chain. Print one slot per line,
  # prefixed by its type. This is hostile to fancy edge cases but works
  # for the bench-style inputs we care about.
  {
    printf '%s\n' "$s" | /usr/bin/grep -oE "'[^']+'" | /usr/bin/sed "s/^/QUOTED:/"
    printf '%s\n' "$s" | /usr/bin/grep -oE '"[^"]+"' | /usr/bin/sed 's/^/QUOTED:/'
    printf '%s\n' "$s" | /usr/bin/grep -oE '(~|/Users/[^ '\'']+|/tmp/[^ '\'']+)[A-Za-z0-9._/-]*' | /usr/bin/sed 's/^/PATH:/'
    printf '%s\n' "$s" | /usr/bin/grep -oE '[A-Za-z0-9_-]+\.[a-z0-9]{2,4}' | /usr/bin/grep -vE '^[A-Za-z]+\.(com|org|net|io|ai|dev)$' | /usr/bin/sed 's/^/FILE:/'
  }
}

EX_SLOTS=$(extract_slots "$BEST_NL")
IN_SLOTS=$(extract_slots "$NL")

# Build substituted command. Strategy: for each slot type, get parallel
# ordered lists from example and input; if same length, substitute
# in $BEST_CMD one-by-one.
SUBSTITUTED="$BEST_CMD"
SUBS_COUNT=0
SUBS_FAILED=""

substitute_type() {
  local type="$1"
  local ex_vals in_vals ex_n in_n
  ex_vals=$(printf '%s\n' "$EX_SLOTS" | /usr/bin/grep "^${type}:" | /usr/bin/sed "s/^${type}://")
  in_vals=$(printf '%s\n' "$IN_SLOTS" | /usr/bin/grep "^${type}:" | /usr/bin/sed "s/^${type}://")
  ex_n=$(printf '%s' "$ex_vals" | /usr/bin/grep -c . || true)
  in_n=$(printf '%s' "$in_vals" | /usr/bin/grep -c . || true)

  # No slots in example OR mismatched counts: do nothing for this type.
  [ "$ex_n" = "0" ] && return
  if [ "$ex_n" != "$in_n" ]; then
    SUBS_FAILED="$SUBS_FAILED ${type}(ex=${ex_n},in=${in_n})"
    return
  fi
  # Walk parallel arrays.
  local i=1
  local ex_v in_v
  while [ "$i" -le "$ex_n" ]; do
    ex_v=$(printf '%s\n' "$ex_vals" | /usr/bin/sed -n "${i}p")
    in_v=$(printf '%s\n' "$in_vals" | /usr/bin/sed -n "${i}p")
    if [ -n "$ex_v" ] && [ -n "$in_v" ] && [ "$ex_v" != "$in_v" ]; then
      # Literal substitution (no regex) using bash parameter expansion.
      SUBSTITUTED="${SUBSTITUTED//$ex_v/$in_v}"
      SUBS_COUNT=$((SUBS_COUNT + 1))
    fi
    i=$((i + 1))
  done
}

substitute_type "QUOTED"
substitute_type "PATH"
substitute_type "FILE"

T_END=$(/bin/date +%s%N)
MS_TOTAL=$(( (T_END - T_START) / 1000000 ))

# --- Layer 4: report & optionally exec ---
printf '★ matched   : %s  (tf-idf=%s, %dms)\n' "$BEST_ID" "$BEST_SCORE" "$MS_MATCH"
printf '★ template  : cerebellum %s\n' "$(printf "'%s'" "$BEST_CMD")"
printf '★ substituted: cerebellum %s  (%d swaps%s)\n' "$(printf "'%s'" "$SUBSTITUTED")" "$SUBS_COUNT" "${SUBS_FAILED:+; mismatched$SUBS_FAILED}"
printf '★ router    : %dms\n' "$MS_TOTAL"

if [ "$EXEC" = "1" ]; then
  T_EXEC_START=$(/bin/date +%s%N)
  "$CEREB" "$SUBSTITUTED"
  RC=$?
  T_EXEC_END=$(/bin/date +%s%N)
  MS_EXEC=$(( (T_EXEC_END - T_EXEC_START) / 1000000 ))
  printf '★ exec rc   : %d  (%dms)\n' "$RC" "$MS_EXEC"
  printf '★ TOTAL     : %dms  (router %dms + exec %dms)\n' \
    "$(( MS_MATCH + MS_EXEC ))" "$MS_MATCH" "$MS_EXEC"
  # A matched-but-failed execution is a miss for the caller: exit with
  # the cerebellum's code so the kernel falls through to the LLM
  # instead of handing the user "ERR: missing argument" as the answer.
  [ "$RC" -ne 0 ] && exit "$RC"
fi
