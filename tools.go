// CRUD tools: `loops` (your loop templates) and `godloop` (compose/inspect
// godloops). Thin wrappers over the key-authed /api/v1/mcp/* REST surface —
// action-discriminated so the tool list stays small.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// --- public masterplan tool ---

var masterplanTool = map[string]any{
	"name": "masterplan",
	"description": "Add or update public items in Joe's masterplan through Godloop's scoped server-side connector. " +
		"The tool cannot delete items, access private app data, or reveal the integration credential. " +
		"Read https://joetann.com/api/masterplan first and pass its current revision as base_revision.",
	"inputSchema": map[string]any{
		"type":     "object",
		"required": []string{"base_revision", "operations"},
		"properties": map[string]any{
			"base_revision": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "Current revision from the public masterplan JSON.",
			},
			"idempotency_key": map[string]any{
				"type":        "string",
				"description": "Optional stable key for this intended change. A deterministic key is generated when omitted.",
			},
			"operations": map[string]any{
				"type":     "array",
				"minItems": 1,
				"maxItems": 20,
				"items": map[string]any{
					"type":        "object",
					"description": "Use {op:'add',node:{...}} for a full public node, or {op:'update',id:'node-id',fields:{...}}. Update fields are title, summary, goal, status, start, end, lane, progress, budget, and url. Delete, id, parent_id, and kind updates are unavailable.",
					"properties": map[string]any{
						"op":     map[string]any{"type": "string", "enum": []string{"add", "update"}},
						"id":     map[string]any{"type": "string"},
						"node":   map[string]any{"type": "object"},
						"fields": map[string]any{"type": "object"},
					},
					"required": []string{"op"},
				},
			},
		},
	},
}

func callMasterplanTool(args json.RawMessage) (string, error) {
	var in struct {
		BaseRevision   int             `json:"base_revision"`
		IdempotencyKey string          `json:"idempotency_key"`
		Operations     json.RawMessage `json:"operations"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("bad arguments: %v", err)
	}
	pid := projectID()
	if pid == "" {
		return "", fmt.Errorf("no project id: set GODLOOP_PROJECT or create a .godloop file")
	}
	if in.BaseRevision < 1 || len(in.Operations) == 0 {
		return "", fmt.Errorf("base_revision and operations are required")
	}
	idempotencyKey := strings.TrimSpace(in.IdempotencyKey)
	if idempotencyKey == "" {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:", pid, in.BaseRevision) + string(in.Operations)))
		idempotencyKey = fmt.Sprintf("godloop:auto:%x", sum[:16])
	}
	return callAPI("POST", "/api/v1/mcp/integrations/masterplan/changes", map[string]any{
		"project_id":      pid,
		"base_revision":   in.BaseRevision,
		"idempotency_key": idempotencyKey,
		"operations":      in.Operations,
	})
}

// callAPI performs one authenticated REST call and renders the {data:...}
// envelope as indented JSON (agents parse it fine and humans can read logs).
// API errors come back as Go errors carrying the server's code+message so the
// agent can self-correct.
func callAPI(method, path string, body any) (string, error) {
	key := os.Getenv("GODLOOP_KEY")
	if key == "" {
		return "", fmt.Errorf("GODLOOP_KEY not set")
	}
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, baseURL()+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Godloop-Key", key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var apiErr struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Details json.RawMessage `json:"details"`
		}
		if json.Unmarshal(out, &apiErr) == nil && apiErr.Code != "" {
			msg := fmt.Sprintf("%s: %s", apiErr.Code, apiErr.Message)
			if len(apiErr.Details) > 0 {
				msg += " details=" + string(apiErr.Details)
			}
			return "", fmt.Errorf("%s", msg)
		}
		return "", fmt.Errorf("godloop api %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil || len(envelope.Data) == 0 {
		return strings.TrimSpace(string(out)), nil
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, envelope.Data, "", "  ") != nil {
		return string(envelope.Data), nil
	}
	return pretty.String(), nil
}

// --- loops tool ---

var loopsTool = map[string]any{
	"name": "loops",
	"description": "Manage YOUR loop templates on godloop.ai: list, get, create, update, delete. " +
		"Loops are reusable step recipes (the units godloops chain). `get` an existing loop " +
		"to learn the config_json steps shape before authoring one. Visibility can be set to " +
		"private or unlisted here; publishing to the marketplace requires the dashboard.",
	"inputSchema": map[string]any{
		"type":     "object",
		"required": []string{"action"},
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": []string{"list", "get", "create", "update", "delete"},
			},
			"id": map[string]any{
				"type":        "integer",
				"description": "loop template id (required for get/update/delete)",
			},
			"name":        map[string]any{"type": "string"},
			"description": map[string]any{"type": "string"},
			"category":    map[string]any{"type": "string"},
			"visibility": map[string]any{
				"type":        "string",
				"enum":        []string{"private", "unlisted"},
				"description": "update only; 'public' needs the dashboard",
			},
			"config_json": map[string]any{
				"type":        "object",
				"description": "the loop's steps blueprint ({version, steps:[{step_key,name,kind,role,order_index,config}]}); omit on create for a one-step scaffold",
			},
		},
	},
}

func callLoopsTool(args json.RawMessage) (string, error) {
	var in struct {
		Action      string          `json:"action"`
		ID          int64           `json:"id"`
		Name        *string         `json:"name"`
		Description *string         `json:"description"`
		Category    *string         `json:"category"`
		Visibility  *string         `json:"visibility"`
		ConfigJSON  json.RawMessage `json:"config_json"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
	}
	needID := func() error {
		if in.ID <= 0 {
			return fmt.Errorf("action %q requires id", in.Action)
		}
		return nil
	}
	switch in.Action {
	case "list":
		return callAPI("GET", "/api/v1/mcp/loop-templates", nil)
	case "get":
		if err := needID(); err != nil {
			return "", err
		}
		return callAPI("GET", fmt.Sprintf("/api/v1/mcp/loop-templates/%d", in.ID), nil)
	case "create":
		body := map[string]any{}
		if in.Name != nil {
			body["name"] = *in.Name
		}
		if in.Description != nil {
			body["description"] = *in.Description
		}
		if in.Category != nil {
			body["category"] = *in.Category
		}
		if len(in.ConfigJSON) > 0 {
			body["config_json"] = in.ConfigJSON
		}
		return callAPI("POST", "/api/v1/mcp/loop-templates", body)
	case "update":
		if err := needID(); err != nil {
			return "", err
		}
		body := map[string]any{}
		if in.Name != nil {
			body["name"] = *in.Name
		}
		if in.Description != nil {
			body["description"] = *in.Description
		}
		if in.Category != nil {
			body["category"] = *in.Category
		}
		if in.Visibility != nil {
			body["visibility"] = *in.Visibility
		}
		if len(in.ConfigJSON) > 0 {
			body["config_json"] = in.ConfigJSON
		}
		return callAPI("PATCH", fmt.Sprintf("/api/v1/mcp/loop-templates/%d", in.ID), body)
	case "delete":
		if err := needID(); err != nil {
			return "", err
		}
		return callAPI("DELETE", fmt.Sprintf("/api/v1/mcp/loop-templates/%d", in.ID), nil)
	default:
		return "", fmt.Errorf("unknown action %q; valid: list, get, create, update, delete", in.Action)
	}
}

// --- godloop tool ---

var godloopTool = map[string]any{
	"name": "godloop",
	"description": "Compose and inspect godloops (ordered pipelines of loops). Template actions " +
		"(*_template) manage the reusable recipe: send the FULL ordered loop_template_ids list to " +
		"add, remove, or reorder members in one call. Instance actions (list/get/reorder/trigger) " +
		"drive a godloop assigned to a project: `get` returns members in order plus an `active` " +
		"block with the running member index and cycle number. `reorder` takes the full ordered " +
		"loop_instance_ids list; mid-cycle the running loop keeps running and the new order " +
		"applies after its position. Sharing/visibility is dashboard-only.",
	"inputSchema": map[string]any{
		"type":     "object",
		"required": []string{"action"},
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": []string{
					"list_templates", "get_template", "create_template", "update_template", "delete_template",
					"list", "get", "reorder", "trigger",
				},
			},
			"id": map[string]any{
				"type":        "integer",
				"description": "godloop TEMPLATE id for *_template actions; godloop INSTANCE id for get/reorder/trigger",
			},
			"name":        map[string]any{"type": "string"},
			"description": map[string]any{"type": "string"},
			"config": map[string]any{
				"type":        "object",
				"description": "{cadence_seconds: int, between_loop_gate: bool}",
			},
			"loop_template_ids": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "integer"},
				"description": "FULL ordered member list for create_template/update_template",
			},
			"loop_instance_ids": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "integer"},
				"description": "FULL ordered member list for instance reorder (use ids from `get`)",
			},
			"project_id": map[string]any{
				"type":        "string",
				"description": "for `list`; defaults to the .godloop project file",
			},
		},
	},
}

func callGodloopTool(args json.RawMessage) (string, error) {
	var in struct {
		Action          string          `json:"action"`
		ID              int64           `json:"id"`
		Name            *string         `json:"name"`
		Description     *string         `json:"description"`
		Config          json.RawMessage `json:"config"`
		LoopTemplateIDs *[]int64        `json:"loop_template_ids"`
		LoopInstanceIDs *[]int64        `json:"loop_instance_ids"`
		ProjectID       string          `json:"project_id"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
	}
	needID := func() error {
		if in.ID <= 0 {
			return fmt.Errorf("action %q requires id", in.Action)
		}
		return nil
	}
	metaBody := func() map[string]any {
		body := map[string]any{}
		if in.Name != nil {
			body["name"] = *in.Name
		}
		if in.Description != nil {
			body["description"] = *in.Description
		}
		if len(in.Config) > 0 {
			body["config"] = in.Config
		}
		if in.LoopTemplateIDs != nil {
			body["loop_template_ids"] = *in.LoopTemplateIDs
		}
		return body
	}
	switch in.Action {
	case "list_templates":
		return callAPI("GET", "/api/v1/mcp/godloop-templates", nil)
	case "get_template":
		if err := needID(); err != nil {
			return "", err
		}
		return callAPI("GET", fmt.Sprintf("/api/v1/mcp/godloop-templates/%d", in.ID), nil)
	case "create_template":
		return callAPI("POST", "/api/v1/mcp/godloop-templates", metaBody())
	case "update_template":
		if err := needID(); err != nil {
			return "", err
		}
		return callAPI("PATCH", fmt.Sprintf("/api/v1/mcp/godloop-templates/%d", in.ID), metaBody())
	case "delete_template":
		if err := needID(); err != nil {
			return "", err
		}
		return callAPI("DELETE", fmt.Sprintf("/api/v1/mcp/godloop-templates/%d", in.ID), nil)
	case "list":
		pid := in.ProjectID
		if pid == "" {
			pid = projectID()
		}
		if pid == "" {
			return "", fmt.Errorf("no project id: pass project_id or create a .godloop file")
		}
		return callAPI("GET", "/api/v1/mcp/godloops?project_id="+pid, nil)
	case "get":
		if err := needID(); err != nil {
			return "", err
		}
		return callAPI("GET", fmt.Sprintf("/api/v1/mcp/godloops/%d", in.ID), nil)
	case "reorder":
		if err := needID(); err != nil {
			return "", err
		}
		if in.LoopInstanceIDs == nil || len(*in.LoopInstanceIDs) == 0 {
			return "", fmt.Errorf("reorder requires loop_instance_ids (the full ordered member list from `get`)")
		}
		return callAPI("PUT", fmt.Sprintf("/api/v1/mcp/godloops/%d/order", in.ID),
			map[string]any{"loop_instance_ids": *in.LoopInstanceIDs})
	case "trigger":
		if err := needID(); err != nil {
			return "", err
		}
		return callAPI("POST", fmt.Sprintf("/api/v1/mcp/godloops/%d/trigger", in.ID), nil)
	default:
		return "", fmt.Errorf("unknown action %q; valid: list_templates, get_template, create_template, update_template, delete_template, list, get, reorder, trigger", in.Action)
	}
}
