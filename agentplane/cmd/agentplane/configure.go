package main

// `agentplane configure` — bring-your-own-key setup for demo users (strategy
// deliverable #1). Upserts the namespace Secret the brain template references;
// keys are resolved at ResumeActor time, so newly-woken minds pick up the new
// key with no redeploy. Shells out to kubectl (a stated demo prerequisite)
// rather than vendoring client-go.

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func runConfigure(args []string) {
	fs := flag.NewFlagSet("configure", flag.ExitOnError)
	provider := fs.String("provider", "anthropic", "key provider: anthropic | openai | google (selects the default secret)")
	apiKey := fs.String("api-key", "", "API key (or env ANTHROPIC_API_KEY / OPENAI_API_KEY per provider)")
	ns := fs.String("namespace", "agentplane", "namespace holding the brain template + secret")
	secret := fs.String("secret", "", "secret name (default per provider: anthropic-api-key / openai-api-key)")
	printOnly := fs.Bool("print", false, "print the manifest instead of applying")
	_ = fs.Parse(args)
	def := map[string]struct{ secret, env string }{
		"anthropic": {"anthropic-api-key", "ANTHROPIC_API_KEY"},
		"openai":    {"openai-api-key", "OPENAI_API_KEY"},
		"google":    {"gemini-api-key", "GEMINI_API_KEY"},
	}[*provider]
	if def.secret == "" {
		fmt.Fprintf(os.Stderr, "configure: unknown -provider %q (anthropic | openai)\n", *provider)
		os.Exit(2)
	}
	if *secret == "" {
		*secret = def.secret
	}
	if *apiKey == "" {
		*apiKey = os.Getenv(def.env)
	}

	// anthropic/openai keys are sk-…; google keys are not — only shape-check
	// where the shape is known.
	if *apiKey == "" || (*provider != "google" && !strings.HasPrefix(*apiKey, "sk-")) {
		fmt.Fprintf(os.Stderr, "configure: provide -api-key (or set %s)\n", def.env)
		os.Exit(2)
	}

	// kubectl builds the manifest (correct escaping/encoding), we apply it.
	// The key is piped via stdin, NOT --from-literal — a literal would place the
	// secret in kubectl's argv, visible via /proc/<pid>/cmdline and `ps`.
	gen := exec.Command("kubectl", "create", "secret", "generic", *secret,
		"-n", *ns, "--from-file=api-key=/dev/stdin",
		"--dry-run=client", "-o", "yaml")
	gen.Stdin = strings.NewReader(*apiKey)
	manifest, err := gen.Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure: kubectl dry-run failed: %v\n", err)
		os.Exit(1)
	}
	if *printOnly {
		os.Stdout.Write(manifest)
		return
	}
	apply := exec.Command("kubectl", "apply", "-f", "-")
	apply.Stdin = strings.NewReader(string(manifest))
	apply.Stdout, apply.Stderr = os.Stdout, os.Stderr
	if err := apply.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "configure: apply failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf(`configured: secret %s/%s

NOTE: a restored mind keeps the environment it was BAKED with — new keys reach
agents on their next GOLDEN BAKE, not on wake. To pick up this key:
  new agents:      agentplane agent create -f spec.yaml     (bakes with it)
  existing agents: agentplane agent delete -name X && agent create …
                   (suspended sessions of a recreated agent resume fine)
Try it:
  agentplane chat -agent <name>
`, *ns, *secret)
}
