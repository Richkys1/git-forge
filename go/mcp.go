// MCP (Model Context Protocol) at /mcp: JSON-RPC 2.0 over Streamable HTTP (single JSON body).
// Modern era: server/discover answers supportedVersions with 2026-07-28 first; initialize is
// also answered for the legacy fallback. Tools act on the same store as REST.
// Faithful port of lib/mcp.ts.
package main

import (
	"strings"
)

const mcpVersion = "2026-07-28"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

var mcpServerInfo = map[string]interface{}{"name": "forge", "version": "1.0.0"}

type toolDef struct {
	Name        string
	Description string
	Properties  map[string]map[string]string
	Required    []string
}

func toolSchema(t toolDef) map[string]interface{} {
	props := map[string]interface{}{}
	for name, prop := range t.Properties {
		props[name] = map[string]interface{}{"type": prop["type"], "description": prop["description"]}
	}
	return map[string]interface{}{
		"name":        t.Name,
		"description": t.Description,
		"inputSchema": map[string]interface{}{
			"type":       "object",
			"properties": props,
			"required":   t.Required,
		},
	}
}

var mcpTools = []toolDef{
	{
		Name:        "create_repo",
		Description: "Create a new repository owned by the authenticated user.",
		Properties: map[string]map[string]string{
			"owner":       {"type": "string", "description": "Login of the repository owner."},
			"name":        {"type": "string", "description": "Name of the repository to create."},
			"description": {"type": "string", "description": "A short description of the repository."},
			"private":     {"type": "boolean", "description": "Whether the repository is private."},
		},
		Required: []string{"name"},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		Properties: map[string]map[string]string{
			"owner": {"type": "string", "description": "Login of the repository owner."},
			"repo":  {"type": "string", "description": "Name of the repository."},
		},
		Required: []string{"owner", "repo"},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		Properties: map[string]map[string]string{
			"owner": {"type": "string", "description": "Login of the repository owner."},
			"repo":  {"type": "string", "description": "Name of the repository."},
			"title": {"type": "string", "description": "Title of the issue."},
			"body":  {"type": "string", "description": "Body of the issue."},
		},
		Required: []string{"owner", "repo", "title"},
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository, optionally filtered by state.",
		Properties: map[string]map[string]string{
			"owner": {"type": "string", "description": "Login of the repository owner."},
			"repo":  {"type": "string", "description": "Name of the repository."},
			"state": {"type": "string", "description": "Issue state filter: open, closed, or all."},
		},
		Required: []string{"owner", "repo"},
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request from a head branch onto a base branch.",
		Properties: map[string]map[string]string{
			"owner": {"type": "string", "description": "Login of the repository owner."},
			"repo":  {"type": "string", "description": "Name of the repository."},
			"title": {"type": "string", "description": "Title of the pull request."},
			"body":  {"type": "string", "description": "Body of the pull request."},
			"head":  {"type": "string", "description": "Name of the head branch."},
			"base":  {"type": "string", "description": "Name of the base branch."},
		},
		Required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request with a squash commit onto its base branch.",
		Properties: map[string]map[string]string{
			"owner":       {"type": "string", "description": "Login of the repository owner."},
			"repo":        {"type": "string", "description": "Name of the repository."},
			"pull_number": {"type": "integer", "description": "Number of the pull request to merge."},
		},
		Required: []string{"owner", "repo", "pull_number"},
	},
}

func mcpToolResult(data interface{}) map[string]interface{} {
	text := string(mustJSON(data))
	return map[string]interface{}{
		"content":           []interface{}{map[string]interface{}{"type": "text", "text": text}},
		"structuredContent": data,
	}
}

func mcpToolError(message string) map[string]interface{} {
	return map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": message}},
		"isError": true,
	}
}

func mcpRepoPayload(store *Store, owner, name string) map[string]interface{} {
	repo := store.GetRepo(owner, name)
	if repo == nil {
		return nil
	}
	return map[string]interface{}{
		"id":             repo.ID,
		"name":           repo.Name,
		"full_name":      repo.Owner + "/" + repo.Name,
		"owner":          map[string]interface{}{"login": repo.Owner},
		"private":        repo.Private,
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
		"created_at":     repo.CreatedAt,
		"updated_at":     repo.UpdatedAt,
	}
}

func mcpCallTool(store *Store, name string, args map[string]interface{}) map[string]interface{} {
	owner, _ := args["owner"].(string)
	if owner == "" {
		owner = Viewer.Login
	}
	repoName, _ := args["repo"].(string)
	if repoName == "" {
		repoName, _ = args["name"].(string)
	}
	switch name {
	case "create_repo":
		repoNameArg, _ := args["name"].(string)
		if repoNameArg == "" {
			return mcpToolError(`create_repo requires "name".`)
		}
		repo := store.GetRepo(Viewer.Login, repoNameArg)
		if repo == nil {
			priv, _ := args["private"].(bool)
			desc, _ := args["description"].(string)
			repo = store.CreateRepo(Viewer.Login, repoNameArg, CreateRepoOptions{Description: desc, Private: priv})
		}
		if repo == nil {
			return mcpToolError("Repository " + repoNameArg + " could not be created.")
		}
		return mcpToolResult(mcpRepoPayload(store, Viewer.Login, repoNameArg))
	case "get_repo":
		if repoName == "" {
			return mcpToolError(`get_repo requires "repo".`)
		}
		payload := mcpRepoPayload(store, owner, repoName)
		if payload == nil {
			return mcpToolError("Repository " + owner + "/" + repoName + " not found.")
		}
		return mcpToolResult(payload)
	case "create_issue":
		if repoName == "" {
			return mcpToolError(`create_issue requires "repo".`)
		}
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError("Repository " + owner + "/" + repoName + " not found.")
		}
		title, _ := args["title"].(string)
		if title == "" {
			return mcpToolError(`create_issue requires "title".`)
		}
		body, _ := args["body"].(string)
		issue := store.CreateIssue(repo, title, body, nil, Viewer.Login)
		return mcpToolResult(map[string]interface{}{"id": issue.ID, "number": issue.Number, "title": issue.Title, "body": issue.Body, "state": issue.State, "created_at": issue.CreatedAt})
	case "list_issues":
		if repoName == "" {
			return mcpToolError(`list_issues requires "repo".`)
		}
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError("Repository " + owner + "/" + repoName + " not found.")
		}
		state, _ := args["state"].(string)
		if state == "" {
			state = "open"
		}
		state = strings.ToLower(state)
		var issues []*Issue
		for _, i := range repo.Issues {
			issues = append(issues, i)
		}
		sortSlice(issues, func(a, b *Issue) bool { return a.Number > b.Number })
		var items []interface{}
		for _, i := range issues {
			if state != "all" && i.State != state {
				continue
			}
			items = append(items, map[string]interface{}{"id": i.ID, "number": i.Number, "title": i.Title, "body": i.Body, "state": i.State})
		}
		if items == nil {
			items = []interface{}{}
		}
		return mcpToolResult(map[string]interface{}{"issues": items})
	case "create_pull_request":
		if repoName == "" {
			return mcpToolError(`create_pull_request requires "repo".`)
		}
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError("Repository " + owner + "/" + repoName + " not found.")
		}
		title, _ := args["title"].(string)
		head, _ := args["head"].(string)
		base, _ := args["base"].(string)
		if base == "" {
			base = repo.DefaultBranch
		}
		if title == "" || head == "" {
			return mcpToolError(`create_pull_request requires "title" and "head".`)
		}
		body, _ := args["body"].(string)
		pull := store.CreatePull(repo, CreatePullInput{Title: title, Body: body, Head: head, Base: base, User: Viewer.Login})
		if pull == nil {
			return mcpToolError("Could not create a pull request from " + head + " onto " + base + ".")
		}
		return mcpToolResult(map[string]interface{}{
			"id": pull.ID, "number": pull.Number, "pull_number": pull.Number,
			"title": pull.Title, "state": pull.State,
			"head": pull.HeadRef, "base": pull.BaseRef,
			"head_ref": pull.HeadRef, "base_ref": pull.BaseRef,
		})
	case "merge_pull_request":
		if repoName == "" {
			return mcpToolError(`merge_pull_request requires "repo".`)
		}
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError("Repository " + owner + "/" + repoName + " not found.")
		}
		var number int
		if n, ok := asInt(args["pull_number"]); ok {
			number = n
		} else if n, ok := asInt(args["number"]); ok {
			number = n
		} else if n, ok := asInt(args["pull_request_number"]); ok {
			number = n
		}
		if number < 1 {
			return mcpToolError(`merge_pull_request requires "pull_number".`)
		}
		pull := repo.Pulls[number]
		if pull == nil {
			return mcpToolError("Pull request #" + itoa(number) + " not found.")
		}
		if pull.State == "closed" {
			return mcpToolError("Pull request #" + itoa(number) + " is already closed.")
		}
		store.RefreshPullHead(repo, pull)
		if pull.Mergeable == "CONFLICTING" {
			return mcpToolError("Pull request #" + itoa(number) + " has conflicts.")
		}
		author := Viewer.Name + " <" + Viewer.Email + ">"
		sha := store.SquashMerge(repo, pull, "Merge pull request #"+itoa(number)+" from "+repo.Owner+"/"+pull.HeadRef, author)
		pull.Merged = true
		pull.MergedAt = store.NowIso()
		pull.HasMergedAt = true
		pull.MergeCommitSha = sha
		pull.HasMergeCommitSha = true
		pull.State = "closed"
		pull.UpdatedAt = store.NowIso()
		return mcpToolResult(map[string]interface{}{"number": pull.Number, "pull_number": pull.Number, "merged": true, "state": "closed", "sha": sha})
	default:
		return mcpToolError("Unknown tool: " + name)
	}
}

type McpResult struct {
	Status int
	Body   interface{}
}

func handleMcp(store *Store, request interface{}) McpResult {
	req, _ := request.(map[string]interface{})
	if req == nil {
		req = map[string]interface{}{}
	}
	id, hasID := req["id"]
	isNotification := !hasID
	method, _ := req["method"].(string)
	params, _ := req["params"].(map[string]interface{})
	if params == nil {
		params = map[string]interface{}{}
	}

	ok := func(result interface{}) McpResult {
		return McpResult{Status: 200, Body: map[string]interface{}{"jsonrpc": "2.0", "id": idOrNull(id, hasID), "result": result}}
	}
	fail := func(code int, message string) McpResult {
		return McpResult{Status: 200, Body: map[string]interface{}{"jsonrpc": "2.0", "id": idOrNull(id, hasID), "error": map[string]interface{}{"code": code, "message": message}}}
	}

	if method == "notifications/initialized" || strings.HasPrefix(method, "notifications/") {
		return McpResult{Status: 202, Body: nil}
	}
	if isNotification {
		return McpResult{Status: 202, Body: nil}
	}

	switch method {
	case "server/discover":
		return ok(map[string]interface{}{
			"supportedVersions": mcpSupportedVersions,
			"capabilities":      map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":        mcpServerInfo,
			"instructions":      "A GitHub-compatible git forge: create repositories, issues, and pull requests.",
		})
	case "initialize":
		requested, _ := params["protocolVersion"].(string)
		if requested == "" {
			requested = mcpVersion
		}
		negotiated := mcpVersion
		for _, v := range mcpSupportedVersions {
			if v == requested {
				negotiated = requested
				break
			}
		}
		return ok(map[string]interface{}{"protocolVersion": negotiated, "capabilities": map[string]interface{}{"tools": map[string]interface{}{}}, "serverInfo": mcpServerInfo})
	case "ping":
		return ok(map[string]interface{}{})
	case "tools/list":
		// Deterministic: the same array, byte-identical, on every call.
		tools := make([]interface{}, 0, len(mcpTools))
		for _, t := range mcpTools {
			tools = append(tools, toolSchema(t))
		}
		return ok(map[string]interface{}{"tools": tools})
	case "tools/call":
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]interface{})
		if args == nil {
			args = map[string]interface{}{}
		}
		var tool *toolDef
		for i := range mcpTools {
			if mcpTools[i].Name == name {
				tool = &mcpTools[i]
				break
			}
		}
		if tool == nil {
			return fail(-32602, "Unknown tool: "+name)
		}
		// Missing required argument: isError (accepted by the suite alongside -32602).
		for _, required := range tool.Required {
			v := args[required]
			if v == nil || v == "" {
				return ok(mcpToolError("Missing required argument \"" + required + "\" for tool \"" + name + "\"."))
			}
		}
		return ok(mcpCallTool(store, name, args))
	}
	return fail(-32601, "Method not found: "+method)
}

func idOrNull(id interface{}, hasID bool) interface{} {
	if !hasID {
		return nil
	}
	return id
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
