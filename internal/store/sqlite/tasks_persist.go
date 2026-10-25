package sqlite

import (
	"encoding/json"

	"github.com/gemechischala/awaki/internal/tasks"
)

// TasksPersister stores the background task board in SQLite kv (synced via db snapshots).
type TasksPersister struct {
	S *Store
}

func (p TasksPersister) SaveTasks(list []tasks.Task) error {
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return p.S.SetConfigJSON("bg_tasks", data)
}

func (p TasksPersister) LoadTasks() ([]tasks.Task, error) {
	data, err := p.S.GetConfigJSON("bg_tasks")
	if err != nil {
		return nil, nil // empty board
	}
	var out []tasks.Task
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}
