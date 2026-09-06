package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/LocalKinAI/kinclaw/pkg/routine"
	"github.com/LocalKinAI/kinclaw/pkg/server"
)

// routineRunEnv is what a scheduled run needs from the installer: the
// kinclaw binary, the default soul, the working directory and the
// discovery env (skill dirs, soul dirs, GPS, search endpoint) — captured
// from this process so a routine sees the same world the helper does.
func routineRunEnv(soulPath, workDir string) routine.RunEnv {
	bin, _ := os.Executable()
	if abs, err := filepath.Abs(soulPath); err == nil {
		soulPath = abs
	}
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	return routine.RunEnv{Kinclaw: bin, SoulPath: soulPath, WorkDir: workDir, Env: routine.CurrentEnv()}
}

// routineInfo renders a routine for the API / CLI.
func routineInfo(m *routine.Manager, r routine.Routine) server.RoutineInfo {
	info := server.RoutineInfo{
		ID: r.ID, Name: r.Name, Prompt: r.Prompt, Soul: r.Soul,
		Schedule: r.Schedule.Human(), Raw: r.Schedule.Raw, Enabled: r.Enabled,
		Installed: m.Installed(r.ID), LogPath: m.LogPath(r.ID), CreatedAt: r.CreatedAt,
	}
	if t, ok := m.LastRun(r.ID); ok {
		info.LastRun = t.Format(time.RFC3339)
	}
	return info
}

// runRoutineNow executes a routine's command immediately, detached, with
// output appended to its log — what the UI's "Run now" does. The CLI
// `routine run` streams to the terminal instead (see runRoutine).
func runRoutineNow(m *routine.Manager, r routine.Routine, env routine.RunEnv) error {
	argv := m.Command(r, env)
	if err := os.MkdirAll(m.LogDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(m.LogPath(r.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	fmt.Fprintf(f, "\n=== run now %s ===\n", time.Now().Format(time.RFC3339))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = env.WorkDir
	cmd.Stdout, cmd.Stderr = f, f
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		f.Close()
		return err
	}
	go func() {
		_ = cmd.Wait()
		f.Close()
	}()
	return nil
}

// routineLogTail returns the last ~8KB of a routine's log.
func routineLogTail(m *routine.Manager, id string) (string, error) {
	data, err := os.ReadFile(m.LogPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return "(no runs yet)", nil
		}
		return "", err
	}
	const tail = 8 * 1024
	if len(data) > tail {
		data = data[len(data)-tail:]
		if i := strings.IndexByte(string(data), '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	return string(data), nil
}

// runRoutine is `kinclaw routine` — scheduled one-shot runs on launchd.
func runRoutine(args []string) {
	fs := flag.NewFlagSet("routine", flag.ExitOnError)
	name := fs.String("name", "", "add: display name")
	at := fs.String("at", "", `add: schedule — "daily 09:00", "weekdays 18:00", "weekly mon 09:00", "hourly", "every 30m"`)
	soulPath := fs.String("soul", "", "add/run: soul file (default: ./souls/pilot.soul.md or the first soul found)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `kinclaw routine — schedule one-shot runs (launchd)

Usage:
  kinclaw routine list
  kinclaw routine add -name "Morning brief" -at "weekdays 09:00" [-soul PATH] "prompt…"
  kinclaw routine remove ID
  kinclaw routine run ID              Run now in the foreground
  kinclaw routine enable|disable ID
  kinclaw routine log ID

Runs are kinclaw -permissions auto -soul SOUL -exec PROMPT, logged to
~/.kinclaw/routines/ID.log. The registry is ~/.kinclaw/routines.json.

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
	m := routine.DefaultManager()
	soul := findSoulFile(*soulPath)

	switch sub {
	case "", "list":
		list, err := m.List()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if len(list) == 0 {
			fmt.Println("(no routines — kinclaw routine add -name … -at \"daily 09:00\" \"prompt\")")
			return
		}
		for _, r := range list {
			info := routineInfo(m, r)
			state := "on "
			if !r.Enabled {
				state = "off"
			} else if !info.Installed {
				state = "on (not scheduled)"
			}
			last := info.LastRun
			if last == "" {
				last = "never"
			}
			fmt.Printf("%-34s %-22s %-18s last: %s\n    %s\n", r.ID, r.Schedule.Human(), state, last, truncate(r.Prompt, 90))
		}
	case "add":
		prompt := strings.Join(rest, " ")
		if *name == "" || *at == "" || prompt == "" {
			fmt.Fprintln(os.Stderr, `usage: kinclaw routine add -name "X" -at "daily 09:00" "prompt"`)
			os.Exit(2)
		}
		if soul == "" {
			fmt.Fprintln(os.Stderr, "no soul found; pass -soul PATH")
			os.Exit(1)
		}
		r, err := m.Add(routine.Routine{Name: *name, Prompt: prompt, Soul: mustAbs(soul), Schedule: routine.Schedule{Raw: *at}},
			routineRunEnv(soul, ""))
		if err != nil {
			if r.ID != "" {
				fmt.Fprintf(os.Stderr, "warning: %v\n", err)
			} else {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		fmt.Printf("added %s — %s\n  log: %s\n", r.ID, r.Schedule.Human(), m.LogPath(r.ID))
	case "remove", "rm":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "usage: kinclaw routine remove ID")
			os.Exit(2)
		}
		if err := m.Remove(rest[0]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("removed %s\n", rest[0])
	case "enable", "disable":
		if len(rest) != 1 {
			fmt.Fprintf(os.Stderr, "usage: kinclaw routine %s ID\n", sub)
			os.Exit(2)
		}
		if err := m.SetEnabled(rest[0], sub == "enable", routineRunEnv(soul, "")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("%s %sd\n", rest[0], sub)
	case "run":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "usage: kinclaw routine run ID")
			os.Exit(2)
		}
		r, ok := m.Get(rest[0])
		if !ok {
			fmt.Fprintf(os.Stderr, "no routine %q\n", rest[0])
			os.Exit(1)
		}
		env := routineRunEnv(soul, "")
		argv := m.Command(r, env)
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.Env = os.Environ()
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "log":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "usage: kinclaw routine log ID")
			os.Exit(2)
		}
		text, err := routineLogTail(m, rest[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Print(text)
	default:
		fs.Usage()
		os.Exit(2)
	}
}

func mustAbs(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
