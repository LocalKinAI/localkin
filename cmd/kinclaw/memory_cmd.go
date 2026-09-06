package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/LocalKinAI/kinclaw/pkg/compact"
	"github.com/LocalKinAI/kinclaw/pkg/memory"
	"github.com/LocalKinAI/kinclaw/pkg/soul"
)

// runMemory is `kinclaw memory` — the CLI over everything the agent
// remembers. Memory was write-mostly: the agent saved facts and notes,
// the kernel injected them at boot, and the only way to see or fix
// them was sqlite3 and a text editor. This makes it visible and
// curatable, which is what makes a memory trustworthy.
func runMemory(args []string) {
	fs := flag.NewFlagSet("memory", flag.ExitOnError)
	all := fs.Bool("all", false, "list: include transient `_`-prefixed working memory")
	limit := fs.Int("limit", 20, "search: max conversation excerpts")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `kinclaw memory — see and curate what the agent remembers

Usage:
  kinclaw memory                     Overview: facts, notebook topics, sessions
  kinclaw memory list [-all]         Durable facts (memories table)
  kinclaw memory search <query>      Facts + conversation history + notebook
  kinclaw memory forget <key>        Delete one fact
  kinclaw memory learned [topic]     Print the notebook, or one ## section
  kinclaw memory sessions            Conversation buckets (live + archived)
  kinclaw memory paths               Where everything lives

Flags:
`)
		fs.PrintDefaults()
	}
	sub := ""
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, rest = args[0], args[1:]
	}
	if err := fs.Parse(rest); err != nil {
		os.Exit(2)
	}
	rest = fs.Args()

	switch sub {
	case "paths":
		fmt.Printf("memory.db:   %s\n", memory.DefaultDBPath())
		fmt.Printf("notebook:    %s\n", soul.LearnedPath())
		for _, p := range soul.UserInstructionPaths() {
			mark := " (absent)"
			if _, err := os.Stat(p); err == nil {
				mark = ""
			}
			fmt.Printf("KINCLAW.md:  %s%s\n", p, mark)
		}
		return
	case "learned":
		topic := strings.Join(rest, " ")
		data, err := os.ReadFile(soul.LearnedPath())
		if err != nil {
			fmt.Fprintf(os.Stderr, "no notebook at %s\n", soul.LearnedPath())
			os.Exit(1)
		}
		if topic == "" {
			fmt.Print(string(data))
			return
		}
		sec := notebookSection(string(data), topic)
		if sec == "" {
			fmt.Fprintf(os.Stderr, "no section matching %q\n", topic)
			os.Exit(1)
		}
		fmt.Print(sec)
		return
	}

	store, err := memory.OpenMemory(memory.DefaultDBPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "open memory: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	switch sub {
	case "", "overview":
		facts, _ := store.ListMemories(false)
		fmt.Printf("Facts:      %d durable (kinclaw memory list)\n", len(facts))
		if data, err := os.ReadFile(soul.LearnedPath()); err == nil {
			n := strings.Count(string(data), "\n## ")
			fmt.Printf("Notebook:   %d topics, %d bytes (kinclaw memory learned)\n", n, len(data))
		} else {
			fmt.Printf("Notebook:   none yet (%s)\n", soul.LearnedPath())
		}
		sessions, _ := store.Sessions()
		live := 0
		for _, s := range sessions {
			if !strings.Contains(s.ID, "@") && !strings.Contains(s.ID, "#once") {
				live++
			}
		}
		fmt.Printf("Sessions:   %d live, %d total buckets (kinclaw memory sessions)\n", live, len(sessions))
		for _, p := range soul.UserInstructionPaths() {
			if _, err := os.Stat(p); err == nil {
				fmt.Printf("KINCLAW.md: %s\n", p)
			}
		}
	case "list":
		entries, err := store.ListMemories(*all)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		if len(entries) == 0 {
			fmt.Println("(no facts saved yet — the agent saves them with `memory action=save`)")
			return
		}
		for _, e := range entries {
			fmt.Printf("%-28s %s\n    \033[2m%s\033[0m\n", e.Key, e.Value, e.UpdatedAt)
		}
	case "search":
		q := strings.Join(rest, " ")
		if q == "" {
			fmt.Fprintln(os.Stderr, "usage: kinclaw memory search <query>")
			os.Exit(2)
		}
		facts, _ := store.Recall(q)
		fmt.Println("## facts")
		fmt.Println(facts)
		msgs, _ := store.RecallMessages(q, *limit)
		fmt.Println("\n## conversation history")
		fmt.Println(msgs)
		if data, err := os.ReadFile(soul.LearnedPath()); err == nil {
			fmt.Println("\n## notebook")
			hits := 0
			for _, line := range strings.Split(string(data), "\n") {
				if strings.Contains(strings.ToLower(line), strings.ToLower(q)) {
					fmt.Println(line)
					hits++
				}
			}
			if hits == 0 {
				fmt.Println("(no notebook lines match)")
			}
		}
	case "forget":
		key := strings.Join(rest, " ")
		if key == "" {
			fmt.Fprintln(os.Stderr, "usage: kinclaw memory forget <key>")
			os.Exit(2)
		}
		ok, err := store.Forget(key)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		if !ok {
			fmt.Fprintf(os.Stderr, "no fact with key %q (kinclaw memory list to see keys)\n", key)
			os.Exit(1)
		}
		fmt.Printf("forgot %s\n", key)
	case "sessions":
		sessions, err := store.Sessions()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].Last > sessions[j].Last })
		fmt.Printf("%-48s %8s  %-16s  %s\n", "session", "msgs", "first", "last")
		for _, s := range sessions {
			tag := ""
			if strings.Contains(s.ID, "@") {
				tag = "  (archived)"
			} else if strings.Contains(s.ID, "#once") {
				tag = "  (one-shot)"
			}
			fmt.Printf("%-48s %8d  %-16s  %s%s\n", truncate(s.ID, 48), s.Messages, s.First, s.Last, tag)
		}
		// Point at compaction summaries so the user knows they exist.
		if h := store.LoadHistory("KinClaw Pilot", 5); len(h) > 0 && compact.IsSummary(h[0]) {
			fmt.Println("\n(KinClaw Pilot currently starts from a compaction summary — `kinclaw memory search \"Context compacted\"` to read them)")
		}
	default:
		fs.Usage()
		os.Exit(2)
	}
}

// notebookSection returns the `## topic` section (case-insensitive
// substring match on the header) including its header.
func notebookSection(text, topic string) string {
	lines := strings.Split(text, "\n")
	var sb strings.Builder
	in := false
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			if in {
				break
			}
			if strings.Contains(strings.ToLower(line), strings.ToLower(topic)) {
				in = true
			}
		}
		if in {
			sb.WriteString(line + "\n")
		}
	}
	return sb.String()
}
