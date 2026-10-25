package sqlite

// ConfigCloud implements providers.Cloud / scheduler.Cloud over SQLite kv.
type ConfigCloud struct {
	S *Store
}

func (c ConfigCloud) LoadProvidersJSON() ([]byte, error) {
	return c.S.GetConfigJSON("providers")
}

func (c ConfigCloud) SaveProvidersJSON(data []byte) error {
	return c.S.SetConfigJSON("providers", data)
}

func (c ConfigCloud) LoadSchedulesJSON() ([]byte, error) {
	return c.S.GetConfigJSON("schedules")
}

func (c ConfigCloud) SaveSchedulesJSON(data []byte) error {
	return c.S.SetConfigJSON("schedules", data)
}
