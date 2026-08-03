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
	"net/url"
	"os"
	"strings"
	"time"
)

// --- read-only project dashboard tool ---

var projectsTool = map[string]any{
	"name":        "projects",
	"description": "Read live Godloop project status without claiming work. Use `current` for the repository selected by its .godloop file, or `overview` for every project. Returns runner presence, task/Loop/Godloop counts, inbox counts, and the latest active or completed run.",
	"inputSchema": map[string]any{
		"type":     "object",
		"required": []string{"action"},
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": []string{"current", "overview"},
			},
			"project_id": map[string]any{
				"type":        "string",
				"description": "Optional override for `current`; defaults to the repository's .godloop file.",
			},
		},
	},
}

func callProjectsTool(args json.RawMessage) (string, error) {
	var in struct {
		Action    string `json:"action"`
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("bad arguments: %v", err)
	}
	switch in.Action {
	case "overview":
		return callAPI("GET", "/api/v1/mcp/projects", nil)
	case "current":
		pid := strings.TrimSpace(in.ProjectID)
		if pid == "" {
			pid = projectID()
		}
		if pid == "" {
			return "", fmt.Errorf("no project id: pass project_id or create a .godloop file")
		}
		return callAPI("GET", "/api/v1/mcp/projects?project_id="+url.QueryEscape(pid), nil)
	default:
		return "", fmt.Errorf("unknown action %q; valid: current, overview", in.Action)
	}
}

// --- native account masterplan tool ---

var masterplanTool = map[string]any{
	"name": "masterplan",
	"description": "Read or safely modify the authenticated user's native Godloop masterplan. " +
		"It contains workspace goals plus project-linked project, task, and milestone nodes with dates, status, progress, budget, colors, and KPI targets. " +
		"Always read first and pass the exact revision to create/update/delete. Delete requires confirm_node_id to exactly repeat node_id.",
	"inputSchema": map[string]any{
		"type":     "object",
		"required": []string{"action"},
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"read", "create", "update", "delete"},
				"description": "Read the plan or change one exact node.",
			},
			"project_id": map[string]any{
				"type":        "string",
				"description": "Optional project scope override. Defaults to the current repository's .godloop file.",
			},
			"base_revision": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "Required for create/update/delete: exact revision returned by read.",
			},
			"node_id": map[string]any{
				"type":        "string",
				"description": "Required for update/delete: exact node id returned by read.",
			},
			"confirm_node_id": map[string]any{
				"type":        "string",
				"description": "Required for delete and must exactly repeat node_id.",
			},
			"workspace_global": map[string]any{
				"type":        "boolean",
				"description": "For create only: true leaves the node account-global instead of linking it to the selected project.",
			},
			"node": map[string]any{
				"type":        "object",
				"description": "For create: kind, title, status, optional parent_id/summary/goal/start/end/progress/budget/color/sort_order/kpis. The connector generates a stable id when omitted.",
			},
			"fields": map[string]any{
				"type":        "object",
				"description": "For update: only requested node fields. project_id or parent_id may be null to unlink/make top-level.",
			},
		},
	},
}

func callMasterplanTool(args json.RawMessage) (string, error) {
	var in struct {
		Action          string         `json:"action"`
		ProjectID       string         `json:"project_id"`
		BaseRevision    int64          `json:"base_revision"`
		NodeID          string         `json:"node_id"`
		ConfirmNodeID   string         `json:"confirm_node_id"`
		WorkspaceGlobal bool           `json:"workspace_global"`
		Node            map[string]any `json:"node"`
		Fields          map[string]any `json:"fields"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("bad arguments: %v", err)
	}
	pid := strings.TrimSpace(in.ProjectID)
	if pid == "" {
		pid = projectID()
	}
	switch in.Action {
	case "read":
		path := "/api/v1/mcp/masterplan"
		if pid != "" {
			path += "?project_id=" + url.QueryEscape(pid)
		}
		return callAPI("GET", path, nil)
	case "create":
		if in.BaseRevision < 1 || len(in.Node) == 0 {
			return "", fmt.Errorf("base_revision and node are required")
		}
		node := cloneObject(in.Node)
		node["base_revision"] = in.BaseRevision
		if _, ok := node["id"]; !ok {
			encoded, _ := json.Marshal(node)
			sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:", pid, in.BaseRevision) + string(encoded)))
			node["id"] = fmt.Sprintf("mp_%x", sum[:10])
		}
		if _, supplied := node["project_id"]; !supplied && !in.WorkspaceGlobal && pid != "" {
			node["project_id"] = pid
		}
		return callAPI("POST", "/api/v1/mcp/masterplan/nodes", node)
	case "update":
		nodeID := strings.TrimSpace(in.NodeID)
		if in.BaseRevision < 1 || nodeID == "" || len(in.Fields) == 0 {
			return "", fmt.Errorf("base_revision, node_id, and fields are required")
		}
		fields := cloneObject(in.Fields)
		fields["base_revision"] = in.BaseRevision
		return callAPI("PATCH", "/api/v1/mcp/masterplan/nodes/"+url.PathEscape(nodeID), fields)
	case "delete":
		nodeID := strings.TrimSpace(in.NodeID)
		if in.BaseRevision < 1 || nodeID == "" || strings.TrimSpace(in.ConfirmNodeID) != nodeID {
			return "", fmt.Errorf("base_revision and an exact confirm_node_id are required")
		}
		return callAPI("DELETE", "/api/v1/mcp/masterplan/nodes/"+url.PathEscape(nodeID), map[string]any{
			"base_revision": in.BaseRevision, "confirm_node_id": nodeID,
		})
	default:
		return "", fmt.Errorf("unknown action %q; valid: read, create, update, delete", in.Action)
	}
}

func cloneObject(input map[string]any) map[string]any {
	out := make(map[string]any, len(input)+2)
	for key, value := range input {
		out[key] = value
	}
	return out
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
