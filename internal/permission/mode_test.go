package permission

import "testing"

func TestParse(t *testing.T) {
	m, err := Parse("")
	if err != nil || m != ModeAuto {
		t.Fatalf("empty: %v %v", m, err)
	}
	m, err = Parse("ask")
	if err != nil || m != ModeAsk {
		t.Fatalf("ask: %v %v", m, err)
	}
	m, err = Parse("plan")
	if err != nil || m != ModePlan {
		t.Fatalf("plan: %v %v", m, err)
	}
	if _, err := Parse("nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestRequiresApproval(t *testing.T) {
	if RequiresApproval("read_file") {
		t.Fatal("read_file")
	}
	if !RequiresApproval("write_file") {
		t.Fatal("write_file")
	}
	if !RequiresApproval("mcp__x") {
		t.Fatal("mcp")
	}
}

func TestIsPlanDenied(t *testing.T) {
	for _, n := range []string{"bash", "write_file", "edit_file", "apply_patch", "git_commit", "memory_write"} {
		if !IsPlanDenied(n) {
			t.Errorf("%s must be denied in plan mode", n)
		}
	}
	for _, n := range []string{"read_file", "grep", "git_diff", "memory_search", "no_such_tool"} {
		if IsPlanDenied(n) {
			t.Errorf("%s must not be denied", n)
		}
	}
	// the helper and the list must agree for every entry
	for _, d := range PlanDenylist {
		if !IsPlanDenied(d) {
			t.Errorf("%s is in PlanDenylist but IsPlanDenied says no", d)
		}
	}
}
