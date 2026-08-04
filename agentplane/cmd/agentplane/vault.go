package main

// The credential VAULT: a thin, per-user API over GCP Secret Manager. serve
// holds the cloud identity (Workload Identity); users store credentials once,
// and a session's HAND pulls them at runtime through a scoped grant (grant.go).
// Values are write-only over the API — never echoed back to a client, never
// placed in an AgentSpec, never seen by the brain.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Credential names are a short DNS-ish label; users are already validated by the
// token store. Both compose into a Secret Manager id: agentplane-cred-<user>-<name>.
var credNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// credPayload is what a version stores. `value` is the secret material; the rest
// tell the hand how to apply it.
type credPayload struct {
	Type     string `json:"type"`               // git | header | env
	Value    string `json:"value"`              // the secret material (PAT, token, …)
	Host     string `json:"host,omitempty"`     // git: e.g. github.com
	Username string `json:"username,omitempty"` // git: e.g. x-access-token
	VarName  string `json:"varName,omitempty"`  // env: the variable name to set
}

type vault struct {
	client  *secretmanager.Client
	project string
}

func newVault(ctx context.Context, project string) (*vault, error) {
	if project == "" {
		return nil, fmt.Errorf("no project id (set AGENTPLANE_PROJECT)")
	}
	c, err := secretmanager.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	return &vault{client: c, project: project}, nil
}

func (v *vault) parent() string { return "projects/" + v.project }

// secretID maps (user, name) to a Secret Manager id. The user is HASHED, not
// concatenated (threat-model F8): raw emails are neither id-safe (`@`, `.`)
// nor unambiguous — "a" + "b-c" and "a-b" + "c" would collide — and hashing
// also keeps user emails out of GCP resource names. `name` is validated by
// credNameRe before it reaches here.
// userTag is the stable, label-safe form of a user identity.
//
// It exists because GCP labels accept only [\p{Ll}\p{Lo}\p{N}_-]: an email's
// "." and "@" are rejected outright, so labelling secrets with the raw address
// made PUT /v1/credentials fail for every real user — the vault only ever
// worked for test names like "alice". Hex of a hash is label-safe by
// construction and matches the id prefix, so listing and naming agree.
func userTag(user string) string {
	sum := sha256.Sum256([]byte(user))
	return hex.EncodeToString(sum[:8])
}

func (v *vault) secretID(user, name string) string {
	return fmt.Sprintf("agentplane-cred-%s-%s", userTag(user), name)
}

// put creates the secret (if absent) and adds a new version holding the payload.
func (v *vault) put(ctx context.Context, user, name string, p credPayload) error {
	if !credNameRe.MatchString(name) {
		return fmt.Errorf("name must be a lowercase DNS-1123 label [a-z0-9-]")
	}
	id := v.secretID(user, name)
	_, err := v.client.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent:   v.parent(),
		SecretId: id,
		Secret: &secretmanagerpb.Secret{
			Replication: &secretmanagerpb.Replication{
				Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}},
			},
			// Label enables per-user listing without leaking cross-user names.
			Labels: map[string]string{"agentplane_user": userTag(user), "agentplane_cred": "1"},
		},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return err
	}
	data, _ := json.Marshal(p)
	_, err = v.client.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  v.parent() + "/secrets/" + id,
		Payload: &secretmanagerpb.SecretPayload{Data: data},
	})
	return err
}

// access reads the latest version. Used only by the hand-pull path (grant.go).
func (v *vault) access(ctx context.Context, user, name string) (credPayload, error) {
	var p credPayload
	r, err := v.client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: v.parent() + "/secrets/" + v.secretID(user, name) + "/versions/latest",
	})
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(r.Payload.Data, &p)
}

func (v *vault) delete(ctx context.Context, user, name string) error {
	return v.client.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{
		Name: v.parent() + "/secrets/" + v.secretID(user, name),
	})
}

func (v *vault) list(ctx context.Context, user string) ([]string, error) {
	it := v.client.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{
		Parent: v.parent(),
		Filter: "labels.agentplane_user=" + userTag(user),
	})
	// Must use the same tag as secretID, or the prefix strips nothing and the
	// filter matches nothing — listing silently returned empty before this.
	prefix := fmt.Sprintf("agentplane-cred-%s-", userTag(user))
	var out []string
	for {
		s, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		if parts := strings.Split(s.Name, "/secrets/"); len(parts) == 2 {
			out = append(out, strings.TrimPrefix(parts[1], prefix))
		}
	}
	return out, nil
}

// ---- HTTP handlers (client-facing; value is write-only) ---------------------

func (s *server) handleCredPut(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "vault not configured", nil)
		return
	}
	name := r.PathValue("name")
	if !credNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "invalid credential name (want lowercase letters, digits, hyphens)")
		return
	}
	var p credPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if p.Value == "" || (p.Type != "git" && p.Type != "header" && p.Type != "env") {
		writeErr(w, http.StatusBadRequest, `require non-empty "value" and "type" in {git,header,env}`)
		return
	}
	if err := s.vault.put(r.Context(), userOf(r), name, p); err != nil {
		s.fail(w, r, http.StatusBadGateway, "store credential", err)
		return
	}
	s.log.Info("credential stored", "user", userOf(r), "name", name, "type", p.Type) // never the value
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleCredList(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "vault not configured", nil)
		return
	}
	names, err := s.vault.list(r.Context(), userOf(r))
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "list credentials", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": names})
}

func (s *server) handleCredDelete(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "vault not configured", nil)
		return
	}
	if err := s.vault.delete(r.Context(), userOf(r), r.PathValue("name")); err != nil {
		s.fail(w, r, http.StatusBadGateway, "delete credential", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
