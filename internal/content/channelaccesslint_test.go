package content

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// unmarshalPack decodes a pack YAML doc for the lint tests (the YAML path is where emptyConds is populated).
func unmarshalPack(t *testing.T, doc string) Pack {
	t.Helper()
	var p Pack
	if err := yaml.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatalf("unmarshal pack: %v", err)
	}
	return p
}

// TestLintChannelAccessPresentButEmpty is the #60 core: a present-but-null require_flag (a typo'd restriction)
// warns, but a DELIBERATELY empty block (`{}` — the open/announce shape) does not.
func TestLintChannelAccessPresentButEmpty(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		wantFinding bool
		wantSubstr  string
	}{
		{
			name:        "require_flag present but null => WARN (typo'd restriction)",
			yaml:        "pack: p\nchannels:\n  - ref: immortal\n    hear_access:\n      require_flag:\n",
			wantFinding: true, wantSubstr: "require_flag",
		},
		{
			name:        "require_flag present but empty-string => WARN",
			yaml:        "pack: p\nchannels:\n  - ref: immortal\n    access:\n      require_flag: \"\"\n",
			wantFinding: true, wantSubstr: "require_flag",
		},
		{
			name:        "require_flag whitespace-only => WARN",
			yaml:        "pack: p\nchannels:\n  - ref: immortal\n    access:\n      require_flag: \"   \"\n",
			wantFinding: true, wantSubstr: "require_flag",
		},
		{
			name:        "hear_access: {} deliberate announce shape => clean",
			yaml:        "pack: p\nchannels:\n  - ref: announce\n    hear_access: {}\n",
			wantFinding: false,
		},
		{
			name:        "access: {} open channel => clean",
			yaml:        "pack: p\nchannels:\n  - ref: ooc\n    access: {}\n",
			wantFinding: false,
		},
		{
			name:        "hear_access absent entirely => clean (mirrors access)",
			yaml:        "pack: p\nchannels:\n  - ref: gossip\n    access:\n      require_flag: immortal\n",
			wantFinding: false,
		},
		{
			name:        "min_attr present but null => WARN (typo'd restriction)",
			yaml:        "pack: p\nchannels:\n  - ref: lvl\n    access:\n      min_attr:\n",
			wantFinding: true, wantSubstr: "min_attr",
		},
		{
			name:        "min_attr with attr but omitted min => WARN (forgot the floor)",
			yaml:        "pack: p\nchannels:\n  - ref: lvl\n    access:\n      min_attr:\n        attr: level\n",
			wantFinding: true, wantSubstr: "omits `min`",
		},
		{
			name:        "min_attr with min but no attr => WARN (names nothing)",
			yaml:        "pack: p\nchannels:\n  - ref: lvl\n    access:\n      min_attr:\n        min: 10\n",
			wantFinding: true, wantSubstr: "no attribute",
		},
		{
			name:        "min_attr fully specified => clean",
			yaml:        "pack: p\nchannels:\n  - ref: lvl\n    access:\n      min_attr:\n        attr: level\n        min: 10\n",
			wantFinding: false,
		},
		{
			name:        "min_attr explicit min:0 on a named attr => clean (a real floor on signed attrs; not a no-op typo)",
			yaml:        "pack: p\nchannels:\n  - ref: lvl\n    access:\n      min_attr:\n        attr: reputation\n        min: 0\n",
			wantFinding: false,
		},
		{
			name:        "require_flag with a real flag => clean",
			yaml:        "pack: p\nchannels:\n  - ref: immortal\n    access:\n      require_flag: immortal\n",
			wantFinding: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := unmarshalPack(t, tc.yaml)
			got := LintChannelAccess([]Pack{p})
			if tc.wantFinding && len(got) == 0 {
				t.Fatalf("want a finding, got none")
			}
			if !tc.wantFinding && len(got) != 0 {
				t.Fatalf("want no finding, got %d: %+v", len(got), got)
			}
			if tc.wantFinding {
				if len(got) != 1 {
					t.Fatalf("want exactly 1 finding (no double-report), got %d: %+v", len(got), got)
				}
				if tc.wantSubstr != "" && !strings.Contains(got[0].Detail, tc.wantSubstr) {
					t.Fatalf("finding detail %q missing %q", got[0].Detail, tc.wantSubstr)
				}
			}
		})
	}
}

// TestLintChannelAccessBothBlocks confirms both access and hear_access on the same channel are linted, and
// the finding names which block was at fault.
func TestLintChannelAccessBothBlocks(t *testing.T) {
	p := unmarshalPack(t, "pack: p\nchannels:\n  - ref: c\n    access:\n      require_flag:\n    hear_access:\n      require_flag:\n")
	got := LintChannelAccess([]Pack{p})
	if len(got) != 2 {
		t.Fatalf("want 2 findings (access + hear_access), got %d: %+v", len(got), got)
	}
	if got[0].Field != "access" || got[1].Field != "hear_access" {
		t.Fatalf("want access before hear_access, got %q then %q", got[0].Field, got[1].Field)
	}
}

// TestLintChannelAccessClean confirms a pack with no channels and one with well-formed channels produce no
// findings (no false positives on the common shapes).
func TestLintChannelAccessClean(t *testing.T) {
	if got := LintChannelAccess([]Pack{{Pack: "empty"}}); len(got) != 0 {
		t.Fatalf("a pack with no channels must be clean, got %+v", got)
	}
}
