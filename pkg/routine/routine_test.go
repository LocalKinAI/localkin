package routine

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSchedule(t *testing.T) {
	cases := map[string]string{
		"daily 09:00":        "daily at 09:00",
		"every day 7:30":     "daily at 07:30",
		"每天 09:00":           "daily at 09:00",
		"weekdays 18:15":     "weekdays at 18:15",
		"工作日 9:00":           "weekdays at 09:00",
		"weekly mon 09:00":   "every Mon at 09:00",
		"every monday 09:00": "every Mon at 09:00",
		"每周一 09:00":          "every Mon at 09:00",
		"hourly":             "every hour",
		"每小时":                "every hour",
		"every 30m":          "every 30m",
		"every 2h":           "every 2h",
		"每 30 分钟":            "every 30m",
	}
	for in, want := range cases {
		s, err := ParseSchedule(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got := s.Human(); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "sometimes", "daily", "daily 25:00", "every 1m", "weekly funday 09:00"} {
		if _, err := ParseSchedule(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestPlistXML(t *testing.T) {
	s, _ := ParseSchedule("weekdays 09:00")
	xml, err := PlistXML("dev.localkin.kinclaw.routine.x", []string{"/bin/kinclaw", "-exec", `say "hi" & <bye>`},
		RunEnv{WorkDir: "/tmp", Env: map[string]string{"PATH": "/usr/bin"}}, s, "/tmp/x.log")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>say &#34;hi&#34; &amp; &lt;bye&gt;</string>", "<key>Weekday</key>", "<integer>5</integer>",
		"<key>StandardOutPath</key>", "<key>PATH</key>", "<key>WorkingDirectory</key>"} {
		if !strings.Contains(xml, want) {
			t.Errorf("plist missing %q", want)
		}
	}
	if strings.Count(xml, "<key>Weekday</key>") != 5 {
		t.Errorf("weekdays should expand to 5 calendar entries")
	}
	iv, _ := ParseSchedule("every 45m")
	xml, _ = PlistXML("l", []string{"x"}, RunEnv{}, iv, "/tmp/l.log")
	if !strings.Contains(xml, "<key>StartInterval</key>\n\t<integer>2700</integer>") {
		t.Errorf("interval plist wrong:\n%s", xml)
	}
}

func TestManagerRegistry(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{File: filepath.Join(dir, "routines.json"), LogDir: filepath.Join(dir, "logs"),
		AgentsDir: filepath.Join(dir, "agents"), Label: "test.routine"}
	// Install will try launchctl on darwin; make it fail fast by giving
	// no binary so the registry path is what's under test.
	r, err := m.Add(Routine{Name: "Morning brief", Prompt: "summarize my day", Schedule: Schedule{Raw: "daily 08:00"}}, RunEnv{})
	if err == nil || !strings.Contains(err.Error(), "saved, but") {
		t.Fatalf("expected saved-but-not-scheduled error without a binary, got %v", err)
	}
	if !strings.HasPrefix(r.ID, "morning-brief-") || !r.Enabled {
		t.Fatalf("routine: %+v", r)
	}
	list, _ := m.List()
	if len(list) != 1 || list[0].Schedule.Human() != "daily at 08:00" {
		t.Fatalf("list: %+v", list)
	}
	if _, err := m.Add(Routine{Name: "", Prompt: "x", Schedule: Schedule{Raw: "hourly"}}, RunEnv{}); err == nil {
		t.Fatal("name required")
	}
	if err := m.Remove(r.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := m.List(); len(list) != 0 {
		t.Fatal("remove should empty the registry")
	}
	if err := m.Remove("nope"); err == nil {
		t.Fatal("removing unknown id should error")
	}
}
