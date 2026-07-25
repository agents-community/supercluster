package anthropicworker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/quantumnode/agentplane/pkg/sandbox"
)

// Tools returns the agent_toolset_20260401 tool names (bash/read/write/edit/
// glob/grep) backed by a sandbox instead of the local workdir. The worker's
// ToolsFunc returns these; the SessionToolRunner dispatches the agent's tool
// calls to them by name.
//
// Every returned tool implements io.Closer; the worker calls agenttoolset.CloseAll
// when the session ends, which suspends the sandbox exactly once (checkpoint).
func Tools(sb sandbox.Sandbox) []anthropic.BetaTool {
	var once sync.Once
	closeFn := func() error {
		once.Do(func() { _ = sb.Suspend(context.Background()) })
		return nil
	}
	mk := func(name, desc string, schema anthropic.BetaToolInputSchemaParam,
		run func(context.Context, json.RawMessage) (string, error)) anthropic.BetaTool {
		return &sbxTool{name: name, desc: desc, schema: schema, run: run, closeFn: closeFn}
	}

	return []anthropic.BetaTool{
		mk("bash", "Run a shell command in the sandbox and return its output.",
			schema(map[string]any{"command": prop("string", "The shell command to run.")}, "command"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var in struct {
					Command string `json:"command"`
				}
				if err := json.Unmarshal(raw, &in); err != nil {
					return "", err
				}
				r, err := sb.Exec(ctx, in.Command)
				if err != nil {
					return "", err
				}
				return formatExec(r), nil
			}),

		mk("read", "Read a file from the sandbox filesystem.",
			schema(map[string]any{"file_path": prop("string", "Path of the file to read.")}, "file_path"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var in struct {
					FilePath string `json:"file_path"`
				}
				if err := json.Unmarshal(raw, &in); err != nil {
					return "", err
				}
				data, err := sb.ReadFile(ctx, in.FilePath)
				return string(data), err
			}),

		mk("write", "Write a file to the sandbox filesystem.",
			schema(map[string]any{
				"file_path": prop("string", "Path of the file to write."),
				"content":   prop("string", "The file contents."),
			}, "file_path", "content"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var in struct {
					FilePath string `json:"file_path"`
					Content  string `json:"content"`
				}
				if err := json.Unmarshal(raw, &in); err != nil {
					return "", err
				}
				if err := sb.WriteFile(ctx, in.FilePath, []byte(in.Content)); err != nil {
					return "", err
				}
				return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.FilePath), nil
			}),

		mk("edit", "Replace old_string with new_string in a sandbox file.",
			schema(map[string]any{
				"file_path":  prop("string", "Path of the file to edit."),
				"old_string": prop("string", "Exact text to replace."),
				"new_string": prop("string", "Replacement text."),
			}, "file_path", "old_string", "new_string"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var in struct {
					FilePath  string `json:"file_path"`
					OldString string `json:"old_string"`
					NewString string `json:"new_string"`
				}
				if err := json.Unmarshal(raw, &in); err != nil {
					return "", err
				}
				data, err := sb.ReadFile(ctx, in.FilePath)
				if err != nil {
					return "", err
				}
				if !strings.Contains(string(data), in.OldString) {
					return "", fmt.Errorf("edit %s: old_string not found", in.FilePath)
				}
				updated := strings.Replace(string(data), in.OldString, in.NewString, 1)
				if err := sb.WriteFile(ctx, in.FilePath, []byte(updated)); err != nil {
					return "", err
				}
				return fmt.Sprintf("edited %s", in.FilePath), nil
			}),

		mk("glob", "Find files matching a glob pattern in the sandbox.",
			schema(map[string]any{
				"pattern": prop("string", "Glob pattern, e.g. **/*.go or *.txt."),
				"path":    prop("string", "Directory to search from (default: current)."),
			}, "pattern"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var in struct {
					Pattern string `json:"pattern"`
					Path    string `json:"path"`
				}
				if err := json.Unmarshal(raw, &in); err != nil {
					return "", err
				}
				root := in.Path
				if root == "" {
					root = "."
				}
				r, err := sb.Exec(ctx, fmt.Sprintf("find %q -name %q 2>/dev/null | head -200", root, in.Pattern))
				if err != nil {
					return "", err
				}
				return formatExec(r), nil
			}),

		mk("grep", "Search file contents with a regex in the sandbox.",
			schema(map[string]any{
				"pattern": prop("string", "Regex to search for."),
				"path":    prop("string", "Directory to search from (default: current)."),
			}, "pattern"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var in struct {
					Pattern string `json:"pattern"`
					Path    string `json:"path"`
				}
				if err := json.Unmarshal(raw, &in); err != nil {
					return "", err
				}
				root := in.Path
				if root == "" {
					root = "."
				}
				r, err := sb.Exec(ctx, fmt.Sprintf("grep -rn %q %q 2>/dev/null | head -200", in.Pattern, root))
				if err != nil {
					return "", err
				}
				return formatExec(r), nil
			}),
	}
}

// sbxTool implements anthropic.BetaTool (+ io.Closer).
type sbxTool struct {
	name    string
	desc    string
	schema  anthropic.BetaToolInputSchemaParam
	run     func(context.Context, json.RawMessage) (string, error)
	closeFn func() error
}

func (t *sbxTool) Name() string                                    { return t.name }
func (t *sbxTool) Description() string                             { return t.desc }
func (t *sbxTool) InputSchema() anthropic.BetaToolInputSchemaParam { return t.schema }
func (t *sbxTool) Close() error                                    { return t.closeFn() }

func (t *sbxTool) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	out, err := t.run(ctx, input)
	if err != nil {
		return nil, err
	}
	return []anthropic.BetaToolResultBlockParamContentUnion{{OfText: &anthropic.BetaTextBlockParam{Text: out}}}, nil
}

func schema(props map[string]any, required ...string) anthropic.BetaToolInputSchemaParam {
	return anthropic.BetaToolInputSchemaParam{Properties: props, Required: required}
}

func prop(typ, description string) map[string]any {
	return map[string]any{"type": typ, "description": description}
}

func formatExec(r sandbox.ExecResult) string {
	var b strings.Builder
	b.WriteString(r.Stdout)
	if r.Stderr != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("[stderr] " + r.Stderr)
	}
	if r.ExitCode != 0 {
		fmt.Fprintf(&b, "\n[exit code %d]", r.ExitCode)
	}
	if b.Len() == 0 {
		return "(no output)"
	}
	return b.String()
}
