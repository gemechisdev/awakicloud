package telegram

import (
	"testing"

	"github.com/go-telegram/bot/models"
)

func TestIsPriorityUpdate(t *testing.T) {
	msg := func(text, group string) *models.Update {
		return &models.Update{Message: &models.Message{Text: text, MediaGroupID: group}}
	}
	cases := []struct {
		name string
		u    *models.Update
		want bool
	}{
		{"nil", nil, false},
		{"stop", msg("/stop", ""), true},
		{"stop with bot name", msg("/stop@MyBot", ""), true},
		{"files with arg", msg("/files src", ""), true},
		{"plain text", msg("hello", ""), false},
		{"unknown command", msg("/definitely_unknown", ""), false},
		{"media group", msg("/stop", "g1"), false},
		{"callback", &models.Update{CallbackQuery: &models.CallbackQuery{}}, true},
		{"empty", &models.Update{}, false},
	}
	for _, c := range cases {
		if got := isPriorityUpdate(c.u); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
