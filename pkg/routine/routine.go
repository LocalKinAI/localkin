// Package routine schedules one-shot kinclaw runs — the "scheduled
// tasks" of Claude Desktop / Cowork, on top of launchd.
//
// A routine is a prompt, a soul and a schedule. Installing it writes a
// LaunchAgent that runs `kinclaw -permissions auto -soul <soul> -exec
// <prompt>` and logs to ~/.kinclaw/routines/<id>.log. The registry of
// routines lives in ~/.kinclaw/routines.json so the CLI, the serve API
// and the Mac settings tab all see one list.
//
// Runs are `-permissions auto` by necessity: there is nobody to answer
// an approval card at 03:00. The soul's filesystem.deny still holds, and
// a routine that needs a dangerous command is a routine that should not
// exist.
package routine

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Schedule is when a routine fires.
type Schedule struct {
	// Kind: daily | weekdays | weekly | hourly | interval
	Kind            string `json:"kind"`
	Hour            int    `json:"hour"`
	Minute          int    `json:"minute"`
	Weekday         int    `json:"weekday"`          // weekly: 0=Sun … 6=Sat
	IntervalMinutes int    `json:"interval_minutes"` // interval
	Raw             string `json:"raw"`              // what the user typed
}

// Routine is one scheduled run.
type Routine struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Prompt    string   `json:"prompt"`
	Soul      string   `json:"soul"` // soul file path; "" = the manager's default
	Schedule  Schedule `json:"schedule"`
	Enabled   bool     `json:"enabled"`
	CreatedAt string   `json:"created_at"`
}

// RunEnv is what a run needs that only the installer knows.
type RunEnv struct {
	Kinclaw  string            // binary path
	SoulPath string            // default soul when Routine.Soul is empty
	WorkDir  string            // working directory for the run
	Env      map[string]string // environment passed through to launchd
}

// Manager owns the registry file and the LaunchAgents it installs.
type Manager struct {
	File      string // routines.json
	LogDir    string // per-routine logs
	AgentsDir string // ~/Library/LaunchAgents
	Label     string // launchd label prefix
}

// DefaultManager stores under ~/.kinclaw.
func DefaultManager() *Manager {
	home, _ := os.UserHomeDir()
	return &Manager{
		File:      filepath.Join(home, ".kinclaw", "routines.json"),
		LogDir:    filepath.Join(home, ".kinclaw", "routines"),
		AgentsDir: filepath.Join(home, "Library", "LaunchAgents"),
		Label:     "dev.localkin.kinclaw.routine",
	}
}

// List returns the routines, oldest first.
func (m *Manager) List() ([]Routine, error) {
	data, err := os.ReadFile(m.File)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Routine
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", m.File, err)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

func (m *Manager) save(list []Routine) error {
	if err := os.MkdirAll(filepath.Dir(m.File), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.File, data, 0o644)
}

// Get finds a routine by id.
func (m *Manager) Get(id string) (Routine, bool) {
	list, _ := m.List()
	for _, r := range list {
		if r.ID == id {
			return r, true
		}
	}
	return Routine{}, false
}

// Add stores and installs a routine. Name, Prompt and Schedule.Raw are
// required; the id is derived from the name plus a timestamp.
func (m *Manager) Add(r Routine, env RunEnv) (Routine, error) {
	r.Name = strings.TrimSpace(r.Name)
	r.Prompt = strings.TrimSpace(r.Prompt)
	if r.Name == "" || r.Prompt == "" {
		return r, fmt.Errorf("name and prompt are required")
	}
	sched, err := ParseSchedule(r.Schedule.Raw)
	if err != nil {
		return r, err
	}
	r.Schedule = sched
	r.ID = slug(r.Name) + "-" + time.Now().Format("20060102-150405")
	r.CreatedAt = time.Now().Format(time.RFC3339)
	r.Enabled = true
	list, err := m.List()
	if err != nil {
		return r, err
	}
	list = append(list, r)
	if err := m.save(list); err != nil {
		return r, err
	}
	if err := m.Install(r, env); err != nil {
		return r, fmt.Errorf("saved, but scheduling failed: %w", err)
	}
	return r, nil
}

// Remove uninstalls and deletes a routine.
func (m *Manager) Remove(id string) error {
	list, err := m.List()
	if err != nil {
		return err
	}
	kept := make([]Routine, 0, len(list))
	found := false
	for _, r := range list {
		if r.ID == id {
			found = true
			_ = m.Uninstall(r)
			continue
		}
		kept = append(kept, r)
	}
	if !found {
		return fmt.Errorf("no routine %q", id)
	}
	return m.save(kept)
}

// SetEnabled pauses or resumes a routine (the LaunchAgent is removed or
// re-installed; the registry entry stays).
func (m *Manager) SetEnabled(id string, on bool, env RunEnv) error {
	list, err := m.List()
	if err != nil {
		return err
	}
	for i := range list {
		if list[i].ID != id {
			continue
		}
		list[i].Enabled = on
		if err := m.save(list); err != nil {
			return err
		}
		if on {
			return m.Install(list[i], env)
		}
		return m.Uninstall(list[i])
	}
	return fmt.Errorf("no routine %q", id)
}

// LogPath is where a routine's runs append their output.
func (m *Manager) LogPath(id string) string { return filepath.Join(m.LogDir, id+".log") }

// LastRun is the log's modification time, if it has ever run.
func (m *Manager) LastRun(id string) (time.Time, bool) {
	info, err := os.Stat(m.LogPath(id))
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// PlistPath is the LaunchAgent file for a routine.
func (m *Manager) PlistPath(id string) string {
	return filepath.Join(m.AgentsDir, m.Label+"."+id+".plist")
}

// Command is the argv a run executes.
func (m *Manager) Command(r Routine, env RunEnv) []string {
	soul := r.Soul
	if soul == "" {
		soul = env.SoulPath
	}
	return []string{env.Kinclaw, "-permissions", "auto", "-soul", soul, "-exec", r.Prompt}
}

// Install writes the LaunchAgent and loads it. On non-macOS it only
// reports that scheduling is unavailable — the registry still works so
// `routine run` can execute by hand.
func (m *Manager) Install(r Routine, env RunEnv) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("automatic scheduling needs launchd (macOS); use `kinclaw routine run %s` or cron", r.ID)
	}
	if env.Kinclaw == "" {
		return fmt.Errorf("kinclaw binary path unknown")
	}
	if err := os.MkdirAll(m.LogDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(m.AgentsDir, 0o755); err != nil {
		return err
	}
	plist := m.PlistPath(r.ID)
	xmlBody, err := PlistXML(m.Label+"."+r.ID, m.Command(r, env), env, r.Schedule, m.LogPath(r.ID))
	if err != nil {
		return err
	}
	// Unload a previous version first; bootstrap refuses duplicates.
	_ = launchctl("bootout", domain(), plist)
	if err := os.WriteFile(plist, []byte(xmlBody), 0o644); err != nil {
		return err
	}
	if err := launchctl("bootstrap", domain(), plist); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}
	return nil
}

// Uninstall unloads and removes the LaunchAgent.
func (m *Manager) Uninstall(r Routine) error {
	plist := m.PlistPath(r.ID)
	if runtime.GOOS == "darwin" {
		_ = launchctl("bootout", domain(), plist)
	}
	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Installed reports whether the LaunchAgent file exists.
func (m *Manager) Installed(id string) bool {
	_, err := os.Stat(m.PlistPath(id))
	return err == nil
}

func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func launchctl(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ─── Schedule parsing ────────────────────────────────────────────────

var weekdays = map[string]int{
	"sun": 0, "sunday": 0, "周日": 0, "星期日": 0, "周天": 0,
	"mon": 1, "monday": 1, "周一": 1, "星期一": 1,
	"tue": 2, "tuesday": 2, "周二": 2, "星期二": 2,
	"wed": 3, "wednesday": 3, "周三": 3, "星期三": 3,
	"thu": 4, "thursday": 4, "周四": 4, "星期四": 4,
	"fri": 5, "friday": 5, "周五": 5, "星期五": 5,
	"sat": 6, "saturday": 6, "周六": 6, "星期六": 6,
}

// ParseSchedule accepts:
//
//	daily 09:00 · every day 9:00 · 每天 09:00
//	weekdays 09:00 · 工作日 09:00
//	weekly mon 09:00 · every monday 09:00 · 每周一 09:00
//	hourly · 每小时
//	every 30m · every 2h · 每 30 分钟 · 每 2 小时
func ParseSchedule(raw string) (Schedule, error) {
	s := Schedule{Raw: strings.TrimSpace(raw)}
	text := strings.ToLower(strings.TrimSpace(raw))
	text = strings.NewReplacer("每天", "daily ", "每日", "daily ", "工作日", "weekdays ", "每小时", "hourly",
		"每周", "weekly 周", "每", "every ", "分钟", "m", "小时", "h", "点", ":00").Replace(text)
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return s, fmt.Errorf("empty schedule")
	}
	switch fields[0] {
	case "hourly":
		s.Kind = "hourly"
		return s, nil
	case "daily", "weekdays":
		s.Kind = fields[0]
		if len(fields) < 2 {
			return s, fmt.Errorf("%s needs a time, e.g. %q", fields[0], fields[0]+" 09:00")
		}
		h, m, err := parseClock(fields[1])
		if err != nil {
			return s, err
		}
		s.Hour, s.Minute = h, m
		return s, nil
	case "weekly":
		if len(fields) < 3 {
			return s, fmt.Errorf("weekly needs a day and a time, e.g. \"weekly mon 09:00\"")
		}
		wd, ok := weekdays[strings.TrimSuffix(fields[1], "s")]
		if !ok {
			return s, fmt.Errorf("unknown weekday %q", fields[1])
		}
		h, m, err := parseClock(fields[2])
		if err != nil {
			return s, err
		}
		s.Kind, s.Weekday, s.Hour, s.Minute = "weekly", wd, h, m
		return s, nil
	case "every":
		if len(fields) < 2 {
			return s, fmt.Errorf("every what? e.g. \"every 30m\", \"every day 09:00\", \"every monday 09:00\"")
		}
		if fields[1] == "day" && len(fields) >= 3 {
			return ParseSchedule("daily " + fields[2])
		}
		if wd, ok := weekdays[strings.TrimSuffix(fields[1], "s")]; ok && len(fields) >= 3 {
			h, m, err := parseClock(fields[2])
			if err != nil {
				return s, err
			}
			s.Kind, s.Weekday, s.Hour, s.Minute = "weekly", wd, h, m
			return s, nil
		}
		// interval: 30m / 2h / "30 m"
		iv := fields[1]
		if len(fields) >= 3 && (fields[2] == "m" || fields[2] == "h" || fields[2] == "min" || fields[2] == "hours" || fields[2] == "minutes") {
			iv += fields[2]
		}
		mins, err := parseInterval(iv)
		if err != nil {
			return s, err
		}
		s.Kind, s.IntervalMinutes = "interval", mins
		return s, nil
	}
	return s, fmt.Errorf("can't read schedule %q — try \"daily 09:00\", \"weekdays 09:00\", \"weekly mon 09:00\", \"hourly\", \"every 30m\"", raw)
}

func parseClock(s string) (int, int, error) {
	s = strings.TrimSpace(s)
	parts := strings.SplitN(s, ":", 2)
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, 0, fmt.Errorf("bad hour in %q", s)
	}
	m := 0
	if len(parts) == 2 {
		if m, err = strconv.Atoi(parts[1]); err != nil || m < 0 || m > 59 {
			return 0, 0, fmt.Errorf("bad minute in %q", s)
		}
	}
	return h, m, nil
}

func parseInterval(s string) (int, error) {
	s = strings.TrimSpace(s)
	mult := 1
	switch {
	case strings.HasSuffix(s, "h") || strings.HasSuffix(s, "hours"):
		mult = 60
		s = strings.TrimSuffix(strings.TrimSuffix(s, "hours"), "h")
	case strings.HasSuffix(s, "minutes"):
		s = strings.TrimSuffix(s, "minutes")
	case strings.HasSuffix(s, "min"):
		s = strings.TrimSuffix(s, "min")
	case strings.HasSuffix(s, "m"):
		s = strings.TrimSuffix(s, "m")
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bad interval %q", s)
	}
	if n*mult < 5 {
		return 0, fmt.Errorf("interval must be at least 5 minutes")
	}
	return n * mult, nil
}

// Human renders a schedule for lists.
func (s Schedule) Human() string {
	names := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	switch s.Kind {
	case "daily":
		return fmt.Sprintf("daily at %02d:%02d", s.Hour, s.Minute)
	case "weekdays":
		return fmt.Sprintf("weekdays at %02d:%02d", s.Hour, s.Minute)
	case "weekly":
		return fmt.Sprintf("every %s at %02d:%02d", names[s.Weekday%7], s.Hour, s.Minute)
	case "hourly":
		return "every hour"
	case "interval":
		if s.IntervalMinutes%60 == 0 {
			return fmt.Sprintf("every %dh", s.IntervalMinutes/60)
		}
		return fmt.Sprintf("every %dm", s.IntervalMinutes)
	}
	return s.Raw
}

// ─── launchd plist ───────────────────────────────────────────────────

// passthroughEnv is the environment a run inherits from the installer:
// skill/soul discovery, GPS context, search endpoint, PATH, HOME.
var passthroughEnv = []string{
	"PATH", "HOME", "KINCLAW_SKILL_DIRS", "KINCLAW_SOUL_DIRS", "KINCLAW_LOCATION",
	"KINCLAW_DATA_DIR", "SEARXNG_ENDPOINT", "OLLAMA_HOST",
}

// CurrentEnv captures the passthrough variables from this process.
func CurrentEnv() map[string]string {
	out := map[string]string{}
	for _, k := range passthroughEnv {
		if v := os.Getenv(k); v != "" {
			out[k] = v
		}
	}
	return out
}

// PlistXML renders the LaunchAgent for one routine.
func PlistXML(label string, argv []string, env RunEnv, s Schedule, logPath string) (string, error) {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	kv := func(k, v string) {
		fmt.Fprintf(&sb, "\t<key>%s</key>\n\t<string>%s</string>\n", esc(k), esc(v))
	}
	kv("Label", label)
	sb.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range argv {
		fmt.Fprintf(&sb, "\t\t<string>%s</string>\n", esc(a))
	}
	sb.WriteString("\t</array>\n")
	if env.WorkDir != "" {
		kv("WorkingDirectory", env.WorkDir)
	}
	kv("StandardOutPath", logPath)
	kv("StandardErrorPath", logPath)
	if len(env.Env) > 0 {
		sb.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		keys := make([]string, 0, len(env.Env))
		for k := range env.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&sb, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", esc(k), esc(env.Env[k]))
		}
		sb.WriteString("\t</dict>\n")
	}
	cal := func(entries ...map[string]int) {
		if len(entries) == 1 {
			sb.WriteString("\t<key>StartCalendarInterval</key>\n")
			writeCal(&sb, entries[0], "\t")
			return
		}
		sb.WriteString("\t<key>StartCalendarInterval</key>\n\t<array>\n")
		for _, e := range entries {
			writeCal(&sb, e, "\t\t")
		}
		sb.WriteString("\t</array>\n")
	}
	switch s.Kind {
	case "daily":
		cal(map[string]int{"Hour": s.Hour, "Minute": s.Minute})
	case "weekdays":
		var es []map[string]int
		for wd := 1; wd <= 5; wd++ {
			es = append(es, map[string]int{"Weekday": wd, "Hour": s.Hour, "Minute": s.Minute})
		}
		cal(es...)
	case "weekly":
		cal(map[string]int{"Weekday": s.Weekday, "Hour": s.Hour, "Minute": s.Minute})
	case "hourly":
		cal(map[string]int{"Minute": 0})
	case "interval":
		fmt.Fprintf(&sb, "\t<key>StartInterval</key>\n\t<integer>%d</integer>\n", s.IntervalMinutes*60)
	default:
		return "", fmt.Errorf("unknown schedule kind %q", s.Kind)
	}
	sb.WriteString("</dict>\n</plist>\n")
	return sb.String(), nil
}

func writeCal(sb *strings.Builder, e map[string]int, indent string) {
	sb.WriteString(indent + "<dict>\n")
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(sb, "%s\t<key>%s</key>\n%s\t<integer>%d</integer>\n", indent, k, indent, e[k])
	}
	sb.WriteString(indent + "</dict>\n")
}

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "routine"
	}
	if len(out) > 24 {
		out = out[:24]
	}
	return out
}
