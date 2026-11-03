package core

import "sync"

// uiWizard tracks multi-step Telegram input for provider/model forms and data export/import.
type uiWizard struct {
	Kind       string // add_provider | edit_provider | add_model | edit_model_field | export | import
	Step       string // name | base_url | protocol | api_key | model_id | display_name | context_window | field | cats | items | confirm
	ProviderID string
	ModelID    string
	Field      string // when editing a single field
	Draft      map[string]string
	Page       int
	// Selection state for export/import (category keys and optional item names).
	SelectedCats map[string]bool
	SelectedItems map[string]map[string]bool // cat → name → selected
	Overwrite    bool
	PendingData  []byte // import archive held until confirm
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
	// return a copy so callers can mutate without racing the live wizard
	cp := *s.wizard
	if s.wizard.Draft != nil {
		cp.Draft = map[string]string{}
		for k, v := range s.wizard.Draft {
			cp.Draft[k] = v
		}
	}
	if s.wizard.SelectedCats != nil {
		cp.SelectedCats = map[string]bool{}
		for k, v := range s.wizard.SelectedCats {
			cp.SelectedCats[k] = v
		}
	}
	if s.wizard.SelectedItems != nil {
		cp.SelectedItems = map[string]map[string]bool{}
		for cat, m := range s.wizard.SelectedItems {
			cp.SelectedItems[cat] = map[string]bool{}
			for k, v := range m {
				cp.SelectedItems[cat][k] = v
			}
		}
	}
	if s.wizard.PendingData != nil {
		cp.PendingData = append([]byte(nil), s.wizard.PendingData...)
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
	pageSizeSkills    = 6
)
