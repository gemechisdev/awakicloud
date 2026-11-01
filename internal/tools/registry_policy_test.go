package tools

import (
	"sort"
	"testing"

	"github.com/gemechischala/awaki/internal/permission"
)

// Every tool the registry exposes must have an explicit permission decision:
// read-only (no approval in ask mode) or listed in permission.PlanDenylist.
// A new mutating tool that is in neither would be available in plan mode and
// would still require approval only by accident of the fail-closed default —
// so adding a tool without classifying it fails this test.
func TestEveryRegisteredToolIsPermissionClassified(t *testing.T) {
	r := NewRegistry(nil, nil)
	var missing []string
	for _, d := range r.Definitions() {
		if !permission.Classified(d.Name) {
			missing = append(missing, d.Name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("tools without a permission classification (add to permission.readOnlyAllow or permission.PlanDenylist, and core/agent readOnlyDenylist if they mutate): %v", missing)
	}
}

// Plan mode and the read-only role must strip the same mutating tools.
func TestPlanDenylistNamesRealTools(t *testing.T) {
	r := NewRegistry(nil, nil)
	// Tools registered outside NewRegistry (agent loop, delegate, soul/user, schedule, bg tasks, MCP).
	external := map[string]bool{
		"create_file": true, "soul_write": true, "user_write": true, "delegate": true, "promote_project": true,
		"schedule_add": true, "schedule_remove": true, "schedule_pause": true,
		"bg_task_add": true, "bg_task_start": true, "bg_task_stop": true,
	}
	for _, name := range permission.PlanDenylist {
		if _, ok := r.Lookup(name); !ok && !external[name] {
			t.Errorf("PlanDenylist names %q but no such tool is registered — stale entry or typo", name)
		}
	}
}
