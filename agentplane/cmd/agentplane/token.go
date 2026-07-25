package main

// `agentplane token` — issue / list / revoke per-user access tokens for the
// control-plane API. Tokens are opaque random strings stored (labeled by user)
// in a Kubernetes Secret that serve mounts and hot-reloads:
//
//	agentplane token issue <user>     mint a token for <user>, print it ONCE
//	agentplane token list             list users (never prints tokens)
//	agentplane token revoke <user>    remove <user>'s token (takes effect ~1m)
//
// Verification is serve's job: it constant-time-compares a presented Bearer
// token against this set. Issue = add an entry; revoke = remove one. No DB.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

const (
	tokenSecret = "agentplane-serve-tokens" // the Secret serve mounts
	tokenKey    = "tokens.json"             // its single JSON key: {"user":"token"}
)

func runTokenCmd(args []string) {
	if len(args) < 1 {
		tokenUsage()
	}
	ns := env("BRAIN_TEMPLATE_NS", "agentplane")
	switch args[0] {
	case "issue":
		if len(args) < 2 {
			tokenUsage()
		}
		tokenIssue(ns, args[1])
	case "list":
		tokenList(ns)
	case "revoke":
		if len(args) < 2 {
			tokenUsage()
		}
		tokenRevoke(ns, args[1])
	default:
		tokenUsage()
	}
}

func tokenUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  agentplane token issue <user>     mint + print a token for <user>
  agentplane token list             list users with tokens
  agentplane token revoke <user>    remove <user>'s token`)
	os.Exit(2)
}

// loadTokens reads the current {user: token} map from the Secret (empty if none).
func loadTokens(ns string) map[string]string {
	out, err := exec.Command("kubectl", "get", "secret", tokenSecret, "-n", ns,
		"-o", "jsonpath={.data."+strings.ReplaceAll(tokenKey, ".", "\\.")+"}").Output()
	m := map[string]string{}
	if err != nil || len(out) == 0 {
		return m
	}
	// jsonpath returns base64; decode via kubectl's own -o go-template is fiddly,
	// so decode here.
	dec, err := exec.Command("bash", "-c",
		fmt.Sprintf("kubectl get secret %s -n %s -o jsonpath='{.data.%s}' | base64 -d",
			tokenSecret, ns, strings.ReplaceAll(tokenKey, ".", "\\."))).Output()
	if err == nil && len(dec) > 0 {
		_ = json.Unmarshal(dec, &m)
	}
	return m
}

// saveTokens writes the map back, replacing the Secret (create+apply, dry-run).
func saveTokens(ns string, m map[string]string) error {
	blob, _ := json.Marshal(m)
	gen := exec.Command("kubectl", "create", "secret", "generic", tokenSecret,
		"-n", ns, "--from-literal="+tokenKey+"="+string(blob),
		"--dry-run=client", "-o", "yaml")
	manifest, err := gen.Output()
	if err != nil {
		return fmt.Errorf("render secret: %w", err)
	}
	apply := exec.Command("kubectl", "apply", "-f", "-")
	apply.Stdin = strings.NewReader(string(manifest))
	apply.Stderr = os.Stderr
	return apply.Run()
}

func newToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "apl_" + hex.EncodeToString(b)
}

func tokenIssue(ns, user string) {
	if !isTokenUser(user) {
		fmt.Fprintf(os.Stderr, "token: user must be a short slug [a-z0-9-], got %q\n", user)
		os.Exit(2)
	}
	m := loadTokens(ns)
	tok := newToken()
	m[user] = tok
	if err := saveTokens(ns, m); err != nil {
		fmt.Fprintf(os.Stderr, "token: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf(`issued token for %q (takes effect within ~1 min):

  %s

Give it to the user ONCE — it is not stored anywhere you can read it back.
They call the API with:  Authorization: Bearer %s
Revoke with:  agentplane token revoke %s
`, user, tok, tok, user)
}

func tokenList(ns string) {
	m := loadTokens(ns)
	if len(m) == 0 {
		fmt.Println("(no tokens issued)")
		return
	}
	users := make([]string, 0, len(m))
	for u := range m {
		users = append(users, u)
	}
	sort.Strings(users)
	fmt.Println("USER")
	for _, u := range users {
		fmt.Println(u)
	}
}

func tokenRevoke(ns, user string) {
	m := loadTokens(ns)
	if _, ok := m[user]; !ok {
		fmt.Fprintf(os.Stderr, "token: no token for %q\n", user)
		os.Exit(1)
	}
	delete(m, user)
	if err := saveTokens(ns, m); err != nil {
		fmt.Fprintf(os.Stderr, "token: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("revoked %q (stops working within ~1 min)\n", user)
}

func isTokenUser(s string) bool {
	if s == "" || len(s) > 40 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}
