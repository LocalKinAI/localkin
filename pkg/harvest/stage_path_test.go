package harvest

import "testing"

// TestSanitizeNameBlocksTraversal — candidate names come from frontmatter in
// third-party repos and are used to build paths. A traversal sequence there
// would write outside the harvest tree.
func TestSanitizeNameBlocksTraversal(t *testing.T) {
	cases := map[string]string{
		"pdf":                  "pdf",
		"apple-reminders":      "apple-reminders",
		"my_skill.v2":          "my_skill.v2",
		"../../../tmp/evil":    "tmp-evil",
		"..":                   "",
		".":                    "",
		"/etc/passwd":          "etc-passwd",
		"a/b/c":                "a-b-c",
		"":                     "",
		"名字":                   "",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A sanitized name must never contain a path separator, whatever the input.
func TestSanitizeNameNeverYieldsSeparator(t *testing.T) {
	for _, in := range []string{"a/b", "a\\b", "../x", "./x", "x/../y"} {
		got := sanitizeName(in)
		for _, r := range got {
			if r == '/' || r == '\\' {
				t.Errorf("sanitizeName(%q) = %q, contains a separator", in, got)
			}
		}
	}
}
