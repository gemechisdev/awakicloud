package core

import "sync"

// uiWizard tracks multi-step Telegram input for provider/model forms.
type uiWizard struct {
	Kind       string // add_provider | edit_provider | add_model | edit_model_field
	Step       string // name | base_url | protocol | api_key | model_id | display_name | context_window | field
	ProviderID string
	ModelID    string
	Field      string // when editing a single field
	Draft      map[string]string
	Page       int
}

type uiState struct {
	mu     sync.Mutex
	wizard *uiWizard
}

func (s *uiState) clear() {
	s.mu.Lock()
	s.wizard = nil
	s.mu.Unlock()
}

func (s *uiState) get() *uiWizard {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wizard == nil {
		return nil
	}
	// return a shallow copy of draft pointer is fine for same process
	cp := *s.wizard
	if s.wizard.Draft != nil {
		cp.Draft = map[string]string{}
		for k, v := range s.wizard.Draft {
			cp.Draft[k] = v
		}
	}
	return &cp
}

func (s *uiState) set(w *uiWizard) {
	s.mu.Lock()
	s.wizard = w
	s.mu.Unlock()
}

func (s *uiState) setDraft(key, val string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wizard == nil {
		return
	}
	if s.wizard.Draft == nil {
		s.wizard.Draft = map[string]string{}
	}
	s.wizard.Draft[key] = val
}

func (s *uiState) setStep(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wizard != nil {
		s.wizard.Step = step
	}
}

const (
	pageSizeProviders = 5
	pageSizeModels    = 6
	pageSizeHistory   = 5
)
