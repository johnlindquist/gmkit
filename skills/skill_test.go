// Package skills_test verifies that every SKILL.md in this directory is
// well-formed (has YAML frontmatter with name + description, and a
// non-empty body). Catches accidental edits that would silently disable a
// skill in the harness.
package skills_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillFiles(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "skills", "*", "SKILL.md"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		// also try same-dir glob in case CWD differs
		matches, _ = filepath.Glob(filepath.Join(".", "*", "SKILL.md"))
	}
	if len(matches) == 0 {
		t.Fatal("no SKILL.md files found")
	}
	for _, path := range matches {
		t.Run(path, func(t *testing.T) {
			validateSkill(t, path)
		})
	}
}

func validateSkill(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	contents := string(raw)
	if !strings.HasPrefix(contents, "---\n") {
		t.Fatal("missing opening --- frontmatter marker")
	}
	rest := contents[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatal("missing closing --- frontmatter marker")
	}
	frontmatter := rest[:end]
	body := strings.TrimSpace(rest[end+5:])
	if body == "" {
		t.Fatal("empty body after frontmatter")
	}

	keys := map[string]string{}
	var current string
	for _, line := range strings.Split(frontmatter, "\n") {
		// Continuation lines (start with whitespace) belong to the previous key.
		if current != "" && (strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")) {
			keys[current] += " " + strings.TrimSpace(line)
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		k := strings.TrimSpace(line[:idx])
		v := strings.TrimSpace(line[idx+1:])
		keys[k] = v
		current = k
	}

	if name, ok := keys["name"]; !ok || name == "" {
		t.Error("frontmatter missing required `name` key")
	} else if strings.ContainsAny(name, " \t/\\") {
		t.Errorf("frontmatter `name` must be a slug (no spaces or path separators): %q", name)
	}
	if desc, ok := keys["description"]; !ok || len(desc) < 40 {
		t.Errorf("frontmatter `description` is too short (must explain trigger conditions): %q", desc)
	}

	// Defense against silently erasing the prompt-injection guidance during
	// future edits. Read-only skills MUST surface untrusted-content warnings.
	if !strings.Contains(strings.ToLower(body), "untrusted") {
		t.Error("body is missing the untrusted-content warning — required for any skill that surfaces user-supplied data to the assistant")
	}
}
