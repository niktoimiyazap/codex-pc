package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var sensitiveKeyRE = regexp.MustCompile(`(?i)(api[_-]?key|access[_-]?token|auth[_-]?token|token|password|passwd|secret|authorization|private[_-]?key|client[_-]?secret|access[_-]?key|cookie)`)

var sensitiveValueRE = regexp.MustCompile(`(?i)(sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9_]{20,}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{12,}|Bearer\s+[A-Za-z0-9._~+/-]{12,})`)

var inlineSecretAssignRE = regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?token|auth[_-]?token|token|password|passwd|secret|authorization|private[_-]?key|client[_-]?secret)\s*[=:]\s*)([^\s;,&|]+)`)

func containsSensitive(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, value := range x {
			if sensitiveKeyRE.MatchString(k) && fmt.Sprint(value) != "" {
				return true
			}
			if containsSensitive(value) {
				return true
			}
		}
	case []any:
		for _, value := range x {
			if containsSensitive(value) {
				return true
			}
		}
	case string:
		return sensitiveValueRE.MatchString(x) || inlineSecretAssignRE.MatchString(x)
	}
	return false
}

func redactString(s string) string {
	s = sensitiveValueRE.ReplaceAllString(s, "[REDACTED]")
	s = inlineSecretAssignRE.ReplaceAllString(s, `${1}[REDACTED]`)
	return s
}

func redactSensitive(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			if sensitiveKeyRE.MatchString(k) {
				out[k] = "[REDACTED]"
			} else {
				out[k] = redactSensitive(value)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = redactSensitive(value)
		}
		return out
	case string:
		return redactString(x)
	default:
		return v
	}
}

func connectorStateDir() string {
	if state := os.Getenv("CODEXPC_STATE_DIR"); state != "" {
		return state
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		return filepath.Join(local, "CodexPCConnector")
	}
	return filepath.Join(os.TempDir(), "CodexPCConnector")
}

func secretVaultDir() string {
	return filepath.Join(connectorStateDir(), "secrets")
}

func secretVaultFile() string {
	return filepath.Join(secretVaultDir(), "vault.json")
}

func secretRequestPath(id string) string {
	return filepath.Join(secretVaultDir(), "requests", id+".json")
}

func secretResponsePath(id string) string {
	return filepath.Join(secretVaultDir(), "responses", id+".json")
}

type secretVaultRecord struct {
	ID          string `json:"id,omitempty"`
	Title       string `json:"title,omitempty"`
	Name        string `json:"name,omitempty"`
	Label       string `json:"label,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Hint        string `json:"hint,omitempty"`
	LastPurpose string `json:"last_purpose,omitempty"`
	Ciphertext  string `json:"ciphertext,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	LastUsedAt  string `json:"last_used_at,omitempty"`
	UseCount    int    `json:"use_count,omitempty"`
}

func loadSecretVaultMetadata() ([]secretVaultRecord, error) {
	data, err := os.ReadFile(secretVaultFile())
	if errors.Is(err, os.ErrNotExist) {
		return []secretVaultRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	var payload struct {
		Secrets []secretVaultRecord `json:"secrets"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("invalid secret vault metadata: %w", err)
	}
	return payload.Secrets, nil
}

func saveSecretVaultMetadata(records []secretVaultRecord) error {
	payload := struct {
		Version int                 `json:"version"`
		Secrets []secretVaultRecord `json:"secrets"`
	}{Version: 1, Secrets: records}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(secretVaultDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(secretVaultFile(), data, 0o600)
}

func credentialRefMap(args map[string]any) map[string]any {
	refs, _ := args["credential_refs"].(map[string]any)
	return refs
}

func secretRefTitles(args map[string]any) []string {
	refs := credentialRefMap(args)
	if len(refs) == 0 {
		return nil
	}
	records, err := loadSecretVaultMetadata()
	if err != nil {
		return nil
	}
	byID := make(map[string]secretVaultRecord, len(records))
	for _, record := range records {
		id := record.ID
		if id == "" {
			id = record.Name
		}
		byID[strings.ToLower(id)] = record
	}
	seen := map[string]bool{}
	var titles []string
	for _, raw := range refs {
		id := strings.TrimSpace(fmt.Sprint(raw))
		record, ok := byID[strings.ToLower(id)]
		if !ok {
			continue
		}
		title := strings.TrimSpace(record.Title)
		if title == "" {
			title = strings.TrimSpace(record.Kind)
		}
		if title == "" {
			title = "Saved secret"
		}
		if !seen[strings.ToLower(title)] {
			seen[strings.ToLower(title)] = true
			titles = append(titles, title)
		}
	}
	return titles
}

func (s *Server) injectSecretRefs(args map[string]any) error {
	refs := credentialRefMap(args)
	if len(refs) == 0 {
		return nil
	}
	records, err := loadSecretVaultMetadata()
	if err != nil {
		return err
	}
	byID := make(map[string]int, len(records))
	for i, record := range records {
		id := record.ID
		if id == "" {
			id = record.Name
		}
		if id != "" {
			byID[strings.ToLower(id)] = i
		}
	}
	env, _ := args["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	purpose := strings.TrimSpace(stringValue(args["intent"]))
	if purpose == "" {
		purpose = strings.TrimSpace(stringValue(args["approval_reason"]))
	}
	if purpose == "" {
		purpose = "Used by command_exec"
	}
	now := time.Now().Format(time.RFC3339)
	for envName, rawID := range refs {
		envName = strings.TrimSpace(envName)
		if envName == "" {
			return fmt.Errorf("credential_refs contains an empty environment variable name")
		}
		id := strings.TrimSpace(fmt.Sprint(rawID))
		idx, ok := byID[strings.ToLower(id)]
		if !ok {
			return fmt.Errorf("secret id %q is not saved in CodexPC Secret Vault", id)
		}
		plain, err := unprotectSecret(records[idx].Ciphertext)
		if err != nil {
			return fmt.Errorf("decrypt saved secret %q: %w", id, err)
		}
		env[envName] = plain
		records[idx].LastUsedAt = now
		records[idx].LastPurpose = purpose
		records[idx].UseCount++
	}
	args["env"] = env
	if err := saveSecretVaultMetadata(records); err != nil {
		return fmt.Errorf("update Secret Vault usage metadata: %w", err)
	}
	return nil
}

func redactSecretVaultResult(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(m))
	for k, value := range m {
		if k == "secret" || k == "value" {
			out[k] = "[REDACTED]"
		} else {
			out[k] = value
		}
	}
	return out
}

func (s *Server) secretVault(args map[string]any, callID string) (map[string]any, error) {
	action := strings.ToLower(strings.TrimSpace(stringValue(args["action"])))
	if action != "list" {
		return nil, fmt.Errorf("secret_vault action must be: list")
	}
	records, err := loadSecretVaultMetadata()
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(records))
	for _, record := range records {
		id := record.ID
		if id == "" {
			id = record.Name
		}
		items = append(items, map[string]any{"id": id, "title": record.Title, "kind": record.Kind, "hint": record.Hint, "last_purpose": record.LastPurpose, "created_at": record.CreatedAt, "last_used_at": record.LastUsedAt, "use_count": record.UseCount})
	}
	return map[string]any{"status": "ok", "count": len(items), "secrets": items}, nil
}

func (s *Server) credentialValue(args map[string]any, callID string) (map[string]any, error) {
	action := strings.ToLower(strings.TrimSpace(stringValue(args["action"])))
	switch action {
	case "request":
		if !boolValue(args["user_requested_exact_value"], false) {
			return nil, fmt.Errorf("direct value access requires user_requested_exact_value=true after an explicit user request")
		}
		id := strings.TrimSpace(stringValue(args["id"]))
		purpose := strings.TrimSpace(stringValue(args["purpose"]))
		if id == "" || purpose == "" {
			return nil, fmt.Errorf("id and purpose are required")
		}
		records, err := loadSecretVaultMetadata()
		if err != nil {
			return nil, err
		}
		var title string
		found := false
		for _, record := range records {
			rid := record.ID
			if rid == "" {
				rid = record.Name
			}
			if strings.EqualFold(rid, id) {
				title = record.Title
				if title == "" {
					title = record.Kind
				}
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("credential id %q is not saved in CodexPC vault", id)
		}
		requestID := fmt.Sprintf("secret-%d", time.Now().UnixNano())
		request := map[string]any{"request_id": requestID, "id": id, "title": title, "purpose": purpose, "mode": "reveal_to_model", "call_id": callID, "created_at": time.Now().Format(time.RFC3339)}
		data, _ := json.Marshal(request)
		if err := os.MkdirAll(filepath.Dir(secretRequestPath(requestID)), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(secretRequestPath(requestID), data, 0o600); err != nil {
			return nil, err
		}
		_ = os.Remove(secretResponsePath(requestID))
		return map[string]any{"status": "awaiting_user", "request_id": requestID, "id": id, "title": title, "purpose": purpose, "next_action": "Keep this assistant turn alive: call wait, then credential_value action=poll with request_id; repeat while awaiting_user."}, nil
	case "poll":
		requestID := strings.TrimSpace(stringValue(args["request_id"]))
		if !strings.HasPrefix(requestID, "secret-") || len(requestID) > 128 {
			return nil, fmt.Errorf("valid request_id is required")
		}
		data, err := os.ReadFile(secretResponsePath(requestID))
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{"status": "awaiting_user", "request_id": requestID}, nil
		}
		if err != nil {
			return nil, err
		}
		var response struct {
			Approved bool   `json:"approved"`
			ID       string `json:"id"`
			Secret   string `json:"secret"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, fmt.Errorf("invalid credential response: %w", err)
		}
		_ = os.Remove(secretResponsePath(requestID))
		_ = os.Remove(secretRequestPath(requestID))
		if !response.Approved {
			return map[string]any{"status": "denied", "request_id": requestID, "id": response.ID, "reason": response.Reason}, nil
		}
		if response.Secret == "" {
			return nil, fmt.Errorf("approved response contained no value")
		}
		return map[string]any{"status": "completed", "request_id": requestID, "id": response.ID, "value": response.Secret}, nil
	default:
		return nil, fmt.Errorf("credential_value action must be one of: request, poll")
	}
}
