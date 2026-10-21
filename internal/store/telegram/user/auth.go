package user

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/rs/zerolog/log"
)

// codeAuth implements auth.UserAuthenticator for headless / server login.
// Code sources (first match wins):
//  1. AWAKI_TG_CODE / TELEGRAM_CODE environment variable
//  2. code file at CodeFile path (written by operator or bot helper)
//  3. stdin prompt (interactive)
type codeAuth struct {
	phone    string
	password string // 2FA cloud password, optional env AWAKI_TG_PASSWORD
	codeFile string
	mu       sync.Mutex
}

func (a *codeAuth) Phone(_ context.Context) (string, error) {
	if a.phone == "" {
		return "", fmt.Errorf("phone number required (AWAKI_TG_PHONE)")
	}
	return a.phone, nil
}

func (a *codeAuth) Password(_ context.Context) (string, error) {
	if a.password != "" {
		return a.password, nil
	}
	if v := os.Getenv("AWAKI_TG_PASSWORD"); v != "" {
		return v, nil
	}
	if v := os.Getenv("TELEGRAM_2FA_PASSWORD"); v != "" {
		return v, nil
	}
	fmt.Fprint(os.Stderr, "Telegram 2FA password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (a *codeAuth) Code(ctx context.Context, sentCode *tg.AuthSentCode) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	log.Info().Msg("telegram user auth: waiting for login code")

	if v := firstEnv("AWAKI_TG_CODE", "TELEGRAM_CODE"); v != "" {
		return strings.TrimSpace(v), nil
	}
	if a.codeFile != "" {
		if data, err := os.ReadFile(a.codeFile); err == nil {
			code := strings.TrimSpace(string(data))
			if code != "" {
				_ = os.Remove(a.codeFile) // one-shot
				return code, nil
			}
		}
		// Poll code file a few times for non-interactive deploys
		log.Info().Str("file", a.codeFile).Msg("write the login code into this file")
	}

	fmt.Fprint(os.Stderr, "Telegram login code: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read code: %w (or set AWAKI_TG_CODE / write code file)", err)
	}
	return strings.TrimSpace(line), nil
}

// AcceptTermsOfService is required by auth.UserAuthenticator.
func (a *codeAuth) AcceptTermsOfService(_ context.Context, tos tg.HelpTermsOfService) error {
	return nil
}

// SignUp is not supported — account must already exist.
func (a *codeAuth) SignUp(_ context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, fmt.Errorf("sign-up not supported: use an existing Telegram account")
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// Ensure codeAuth implements the interfaces gotd expects.
var _ auth.UserAuthenticator = (*codeAuth)(nil)
