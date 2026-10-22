//go:build nogotd

package telegram

import (
	"context"
	"fmt"

	"github.com/gemechischala/awaki/internal/store"
)

// StartUserStore is unavailable in builds without gotd (MTProto).
func StartUserStore(ctx context.Context, cfg UserConfig) (store.Store, error) {
	return nil, fmt.Errorf("Telegrive-style user storage not included in this build (rebuild without -tags nogotd)")
}
