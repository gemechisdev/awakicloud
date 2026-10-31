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
