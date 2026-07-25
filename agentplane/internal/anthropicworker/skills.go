package anthropicworker

// Skills: the stock SDK (agenttoolset.SetupSkills) downloads skills to the
// *worker's* local Workdir, which is invisible to tools that execute inside a
// remote sandbox. We instead stream each skill's zip bundle straight into the
// sandbox at /root/.claude/skills/<name>/, guarded by a marker file that
// survives checkpoint/restore — installed once, free on every later event.
//
// Hard-won specifics (from the substrate-agents-api POCs):
//   - Skill-content download is an environment-scoped capability: it requires
//     the ENVIRONMENT key as a bearer (a plain API key gets 403), with any
//     X-Api-Key header dropped.
//   - The Get/Download endpoints take the numeric Version string, NOT the
//     version's tagged ID.
//   - Zip entries already carry the skill's top-level dir; extract straight
//     under skillsDir or paths double up.
//   - Only write the marker when every skill landed, so failures retry.

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/quantumnode/agentplane/pkg/sandbox"
)

const (
	skillsDir    = "/root/.claude/skills"
	skillsMarker = skillsDir + "/.installed"
)

// installSkills unpacks the session agent's skills into the sandbox, once.
func installSkills(ctx context.Context, client anthropic.Client, sb sandbox.Sandbox, sessionID, envKey string, log *slog.Logger) error {
	if r, err := sb.Exec(ctx, "test -f "+skillsMarker+" && echo yes || echo no"); err == nil && strings.TrimSpace(r.Stdout) == "yes" {
		return nil // already installed in this (checkpointed) sandbox
	}

	authOpts := bearerOpts(envKey)

	session, err := client.Beta.Sessions.Get(ctx, sessionID, anthropic.BetaSessionGetParams{}, authOpts...)
	if err != nil {
		return fmt.Errorf("get session %s: %w", sessionID, err)
	}
	var failed int
	for _, sk := range session.Agent.Skills {
		if err := installOne(ctx, client, sb, sk.SkillID, sk.Version, authOpts...); err != nil {
			log.Warn("skill install failed", slog.String("skill_id", sk.SkillID), slog.Any("error", err))
			failed++
			continue
		}
		log.Info("skill installed into sandbox", slog.String("skill_id", sk.SkillID))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d skills failed to install", failed, len(session.Agent.Skills))
	}
	return sb.WriteFile(ctx, skillsMarker, []byte("ok\n"))
}

// installOne resolves, downloads and extracts a single skill into the sandbox.
func installOne(ctx context.Context, client anthropic.Client, sb sandbox.Sandbox, skillID, wantVersion string, authOpts ...option.RequestOption) error {
	version, err := resolveVersion(ctx, client, skillID, wantVersion, authOpts...)
	if err != nil {
		return err
	}

	resp, err := client.Beta.Skills.Versions.Download(ctx, version, anthropic.BetaSkillVersionDownloadParams{SkillID: skillID}, authOpts...)
	if err != nil {
		return fmt.Errorf("download skill: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read skill archive: %w", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return fmt.Errorf("open skill zip: %w", err)
	}

	// path.Clean("/"+name) collapses any ../ so an entry can't escape skillsDir.
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		dest := skillsDir + path.Clean("/"+f.Name)
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("open %s in zip: %w", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("read %s in zip: %w", f.Name, err)
		}
		if err := sb.WriteFile(ctx, dest, data); err != nil {
			return fmt.Errorf("write %s to sandbox: %w", dest, err)
		}
	}
	return nil
}

// resolveVersion maps a skill's want-version to the concrete version string the
// Get/Download endpoints take (a numeric string — NOT the version's tagged ID).
// Mirrors the SDK's resolveSkillVersion: a numeric want is used as-is, otherwise
// the newest numeric version is selected.
func resolveVersion(ctx context.Context, client anthropic.Client, skillID, want string, authOpts ...option.RequestOption) (string, error) {
	if isNumeric(want) {
		return want, nil
	}
	page, err := client.Beta.Skills.Versions.List(ctx, skillID, anthropic.BetaSkillVersionListParams{}, authOpts...)
	if err != nil {
		return "", fmt.Errorf("list skill versions: %w", err)
	}
	var newest string
	for i := range page.Data {
		v := page.Data[i].Version
		if isNumeric(v) && (newest == "" || numGreater(v, newest)) {
			newest = v
		}
	}
	if newest == "" {
		return "", fmt.Errorf("skill %s has no concrete version to resolve %q against", skillID, want)
	}
	return newest, nil
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// numGreater compares two all-numeric version strings by value.
func numGreater(a, b string) bool {
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	return a > b
}

// bearerOpts authorizes session/skill calls with the environment key and drops
// any X-Api-Key the client carries (mirrors the SDK's internal bearerReqOpts).
func bearerOpts(envKey string) []option.RequestOption {
	return []option.RequestOption{
		option.WithHeaderDel("X-Api-Key"),
		option.WithAuthToken(envKey),
	}
}
