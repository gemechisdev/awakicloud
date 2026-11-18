package system

import (
	"context"
	"fmt"
	"strings"

	"github.com/gemechischala/awaki/internal/sshcreds"
	"github.com/gemechischala/awaki/internal/tools/types"
)

// SSHTools exposes identity list/resolve helpers so the agent picks the right
// Host alias before git clone/push/ssh.
func SSHTools(store *sshcreds.Store) []types.Tool {
	if store == nil {
		return nil
	}
	return []types.Tool{
		{
			Name:  "ssh_list",
			Label: "Listing SSH identities",
			Description: "List configured SSH Host aliases and GitHub usernames. " +
				"Call this before git clone/push over SSH when multiple accounts exist. " +
				"For @username GitHub repos use Host github.com-username and URL " +
				"git@github.com-username:username/repo.git (or git_clone with ssh_host).",
			Parameters: map[string]any{},
			Handler: func(_ context.Context, _ map[string]any) types.Result {
				list, err := store.List()
				if err != nil {
					return types.Errf("ssh_list: %v", err)
				}
				if len(list) == 0 {
					return types.Result{Output: "No SSH identities configured. Owner can add them via /ssh in Telegram."}
				}
				var b strings.Builder
				b.WriteString(fmt.Sprintf("%d SSH identities:\n\n", len(list)))
				for _, id := range list {
					fmt.Fprintf(&b, "Host: %s\n  HostName: %s\n  User: %s\n", id.Host, id.HostName, id.User)
					if id.GitHubUser != "" {
						fmt.Fprintf(&b, "  GitHub: @%s\n", id.GitHubUser)
						fmt.Fprintf(&b, "  Clone: git@%s:%s/REPO.git\n", id.Host, id.GitHubUser)
					}
					if id.GitName != "" || id.GitEmail != "" {
						fmt.Fprintf(&b, "  Git author: %s <%s>\n", id.GitName, id.GitEmail)
					}
					b.WriteByte('\n')
				}
				b.WriteString("Tip: git_clone url=... ssh_host=<Host> rewrites the URL to use that identity.\n")
				return types.Result{Output: b.String()}
			},
		},
		{
			Name:  "ssh_resolve",
			Label: "Resolving SSH host alias",
			Description: "Resolve which SSH Host alias to use for a GitHub username or Host name. " +
				"Returns the Host alias string to pass as ssh_host on git_clone, or to put in git@HOST:owner/repo.git.",
			Parameters: map[string]any{
				"github_user": map[string]any{
					"type":        "string",
					"description": "GitHub username (with or without @), e.g. gemechisdev",
				},
				"host": map[string]any{
					"type":        "string",
					"description": "Exact Host alias to look up (alternative to github_user)",
				},
			},
			Handler: func(_ context.Context, input map[string]any) types.Result {
				if h := strings.TrimSpace(types.GetStringOpt(input, "host")); h != "" {
					id, ok, err := store.Get(h)
					if err != nil {
						return types.Errf("%v", err)
					}
					if !ok {
						return types.Result{Output: fmt.Sprintf("No identity for Host %q. Use ssh_list.", h), IsErr: true}
					}
					return types.Result{Output: formatResolve(id)}
				}
				user := strings.TrimSpace(types.GetStringOpt(input, "github_user"))
				user = strings.TrimPrefix(user, "@")
				if user == "" {
					return types.Errf("pass github_user or host")
				}
				if host := store.ResolveHostForGitHubUser(user); host != "" {
					id, ok, _ := store.Get(host)
					if ok {
						return types.Result{Output: formatResolve(id)}
					}
					return types.Result{Output: "Host alias: " + host}
				}
				// Fallback: try Get by username
				id, ok, err := store.Get(user)
				if err != nil {
					return types.Errf("%v", err)
				}
				if !ok {
					return types.Result{
						Output: fmt.Sprintf("No SSH identity for GitHub @%s. Configure via /ssh (Host github.com-%s).", user, user),
						IsErr:  true,
					}
				}
				return types.Result{Output: formatResolve(id)}
			},
		},
	}
}

func formatResolve(id sshcreds.Identity) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Host alias: %s\nHostName: %s\nUser: %s\n", id.Host, id.HostName, id.User)
	if id.GitHubUser != "" {
		fmt.Fprintf(&b, "GitHub: @%s\n", id.GitHubUser)
		fmt.Fprintf(&b, "Clone example: git@%s:%s/REPO.git\n", id.Host, id.GitHubUser)
		fmt.Fprintf(&b, "git_clone: url=https://github.com/%s/REPO.git ssh_host=%s\n", id.GitHubUser, id.Host)
	} else {
		fmt.Fprintf(&b, "Clone example: git@%s:OWNER/REPO.git\n", id.Host)
		fmt.Fprintf(&b, "git_clone: url=... ssh_host=%s\n", id.Host)
	}
	return b.String()
}
