package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/tools"
)

func TestInferSkillNamesFromToolCall_ReadFileSkillMarkdown(t *testing.T) {
	workspace := t.TempDir()
	skillDir := filepath.Join(workspace, "skills", "three-one")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: three-one\ndescription: test\n---\n# Three One\n"),
		0o644,
	); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cb := NewContextBuilder(workspace)
	ts := &turnState{
		workspace: workspace,
		agent: &AgentInstance{
			Workspace:      workspace,
			ContextBuilder: cb,
		},
	}

	got := inferSkillNamesFromToolCall(ts, "read_file", map[string]any{
		"path": filepath.Join(workspace, "skills", "three-one", "SKILL.md"),
	})
	if len(got) != 1 || got[0] != "three-one" {
		t.Fatalf("inferSkillNamesFromToolCall = %v, want [three-one]", got)
	}
}

func TestInferSkillNamesFromToolCall_NonSkillFileIgnored(t *testing.T) {
	workspace := t.TempDir()
	ts := &turnState{workspace: workspace}

	got := inferSkillNamesFromToolCall(ts, "read_file", map[string]any{
		"path": filepath.Join(workspace, "README.md"),
	})
	if len(got) != 0 {
		t.Fatalf("inferSkillNamesFromToolCall = %v, want empty", got)
	}
}

func TestShouldPublishToolResult(t *testing.T) {
	for _, tc := range []struct {
		name, tool                  string
		send, handled, silent, want bool
	}{
		{"normal turn exec", "exec", true, false, false, false},
		{"cron agent exec", "exec", true, false, false, false},
		{"subagent exec", "exec", false, false, false, false},
		{"async completion parent exec", "exec", true, false, false, false},
		{"handled exec hook", "exec", true, true, false, false},
		{"explicit message", "message", true, true, false, true},
		{"explicit subagent message", "message", false, true, false, true},
		{"explicit file", "send_file", true, true, false, true},
		{"ordinary result", "other", true, false, false, true},
		{"silent result", "other", true, false, true, false},
		{"disabled response", "other", false, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := &tools.ToolResult{ForLLM: "diagnostic", ForUser: "diagnostic", ResponseHandled: tc.handled, Silent: tc.silent}
			before := result.ContentForLLM()
			if got := shouldPublishToolResult(tc.tool, result, tc.send); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			if result.ContentForLLM() != before {
				t.Fatal("model context changed")
			}
		})
	}
}
