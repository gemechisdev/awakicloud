package telegram

// ProvidersCloud adapts Store into providers.Cloud.
type ProvidersCloud struct {
	S *Store
}

func (p ProvidersCloud) LoadProvidersJSON() ([]byte, error) {
	return p.S.LoadConfigJSON(ConfigProviders)
}

func (p ProvidersCloud) SaveProvidersJSON(data []byte) error {
	return p.S.SaveConfigJSON(ConfigProviders, data)
}
