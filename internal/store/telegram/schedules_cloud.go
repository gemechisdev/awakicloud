package telegram

type SchedulesCloud struct {
	S *Store
}

func (p SchedulesCloud) LoadSchedulesJSON() ([]byte, error) {
	return p.S.LoadConfigJSON(ConfigSchedules)
}

func (p SchedulesCloud) SaveSchedulesJSON(data []byte) error {
	return p.S.SaveConfigJSON(ConfigSchedules, data)
}
