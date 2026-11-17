// REST v3 (GitHub-compatible subset) under /api/v3.
// Every reply is application/json with a non-empty X-GitHub-Request-Id; errors are
// {"message": string}. Auth: Bearer <MAKEFUN_SHOP_TOKEN> is the viewer; no token is 401;
// a repo under any other owner is 404, never 403.
// Faithful port of lib/rest.ts.
package main

import (
	"encoding/base64"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type RestResult struct {
	Status  int
	Body    interface{}
	Headers map[string]string
}

type RestRequest struct {
	Method   string
	Segments []string // path segments after /api/v3, decoded
	Query    url.Values
	Headers  map[string]string
	Body     interface{} // parsed JSON, or nil when no body
	RawBody  string
	Host     string // for absolute Link URLs
}

func restJSON(status int, body interface{}, headers map[string]string) RestResult {
	return RestResult{Status: status, Body: body, Headers: headers}
}

func restErr(status int, message string) RestResult {
	return restJSON(status, map[string]interface{}{"message": message}, nil)
}

func restNotFound(what ...string) RestResult {
	msg := "Not Found"
	if len(what) > 0 && what[0] != "" {
		msg = what[0]
	}
	return restErr(404, msg)
}

func repoJSON(repo *Repo, host string) map[string]interface{} {
	ownerID := 0
	if repo.Owner == Viewer.Login {
		ownerID = Viewer.ID
	}
	return map[string]interface{}{
		"id":             repo.ID,
		"node_id":        "R_" + strconv.Itoa(repo.ID),
		"name":           repo.Name,
		"full_name":      repo.Owner + "/" + repo.Name,
		"owner":          map[string]interface{}{"login": repo.Owner, "id": ownerID, "type": "User"},
		"private":        repo.Private,
		"description":    repo.Description,
		"fork":           false,
		"created_at":     repo.CreatedAt,
		"updated_at":     repo.UpdatedAt,
		"pushed_at":      repo.UpdatedAt,
		"homepage":       "",
		"default_branch": repo.DefaultBranch,
		"html_url":       "http://" + host + "/" + repo.Owner + "/" + repo.Name,
		"clone_url":      "http://" + host + "/" + repo.Owner + "/" + repo.Name + ".git",
		"git_url":        "git://" + host + "/" + repo.Owner + "/" + repo.Name + ".git",
		"ssh_url":        "git@" + host + ":" + repo.Owner + "/" + repo.Name + ".git",
		"has_issues":     true,
		"has_projects":   true,
		"has_wiki":       true,
	}
}

func issueJSON(issue *Issue, repo *Repo) map[string]interface{} {
	labels := make([]interface{}, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		color := "ededed"
		if l, ok := repo.Labels[name]; ok {
			color = l.Color
		}
		labels = append(labels, map[string]interface{}{"name": name, "color": color, "description": ""})
	}
	var closedAt interface{}
	if issue.State == "closed" {
		closedAt = issue.UpdatedAt
	}
	return map[string]interface{}{
		"id":         issue.DatabaseID,
		"node_id":    issue.ID,
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"locked":     false,
		"user":       map[string]interface{}{"login": issue.User, "id": Viewer.ID, "type": "User"},
		"labels":     labels,
		"created_at": issue.CreatedAt,
		"updated_at": issue.UpdatedAt,
		"closed_at":  closedAt,
		"comments":   len(issue.Comments),
	}
}

func pullJSON(pull *Pull, repo *Repo) map[string]interface{} {
	mergeable := pull.Mergeable == "MERGEABLE"
	mergeableState := "unknown"
	if pull.Mergeable == "MERGEABLE" {
		mergeableState = "clean"
	} else if pull.Mergeable == "CONFLICTING" {
		mergeableState = "dirty"
	}
	var closedAt interface{}
	if pull.State == "closed" {
		closedAt = pull.UpdatedAt
	}
	var mergedAt interface{}
	if pull.HasMergedAt {
		mergedAt = pull.MergedAt
	}
	var mergeCommitSha interface{}
	if pull.HasMergeCommitSha {
		mergeCommitSha = pull.MergeCommitSha
	}
	return map[string]interface{}{
		"id":      pull.DatabaseID,
		"node_id": pull.ID,
		"number":  pull.Number,
		"title":   pull.Title,
		"body":    pull.Body,
		"state":   pull.State,
		"locked":  false,
		"user":    map[string]interface{}{"login": pull.User, "id": Viewer.ID, "type": "User"},
		"head": map[string]interface{}{
			"label": repo.Owner + ":" + pull.HeadRef, "ref": pull.HeadRef, "sha": pull.HeadSha,
			"repo": map[string]interface{}{"name": repo.Name, "full_name": repo.Owner + "/" + repo.Name},
		},
		"base": map[string]interface{}{
			"label": repo.Owner + ":" + pull.BaseRef, "ref": pull.BaseRef, "sha": pull.BaseSha,
			"repo": map[string]interface{}{"name": repo.Name, "full_name": repo.Owner + "/" + repo.Name},
		},
		"merged":           pull.Merged,
		"mergeable":        mergeable,
		"mergeable_state":  mergeableState,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeCommitSha,
		"created_at":       pull.CreatedAt,
		"updated_at":       pull.UpdatedAt,
		"closed_at":        closedAt,
	}
}

func parsePage(query url.Values) (perPage, page int) {
	perPage = 30
	if v := query.Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n != 0 {
			perPage = n
		}
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	page = 1
	if v := query.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n != 0 {
			page = n
		}
	}
	if page < 1 {
		page = 1
	}
	return perPage, page
}

func pageOf[T any](items []T, query url.Values) (page []T, total, perPage, pageNum int) {
	perPage, pageNum = parsePage(query)
	start := (pageNum - 1) * perPage
	if start >= len(items) {
		return []T{}, len(items), perPage, pageNum
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], len(items), perPage, pageNum
}

func linkHeader(host, path string, query url.Values, total, perPage, page int) string {
	lastPage := (total + perPage - 1) / perPage
	if lastPage < 1 {
		lastPage = 1
	}
	if lastPage <= 1 {
		return ""
	}
	urlFor := func(p int) string {
		q := url.Values{}
		for k, vs := range query {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		q.Set("page", strconv.Itoa(p))
		q.Set("per_page", strconv.Itoa(perPage))
		return "http://" + host + path + "?" + q.Encode()
	}
	var parts []string
	if page < lastPage {
		parts = append(parts, "<"+urlFor(page+1)+">; rel=\"next\"")
	}
	if page > 1 {
		parts = append(parts, "<"+urlFor(page-1)+">; rel=\"prev\"")
	}
	parts = append(parts, "<"+urlFor(1)+">; rel=\"first\"")
	parts = append(parts, "<"+urlFor(lastPage)+">; rel=\"last\"")
	return strings.Join(parts, ", ")
}

// Commits reachable from a tip, newest first (first-parent walk is enough for the suite).
func commitsFrom(repo *Repo, tip string) []map[string]interface{} {
	if tip == "" {
		return []map[string]interface{}{}
	}
	out := []map[string]interface{}{}
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		info := parseCommit(obj.Data)
		who := map[string]interface{}{
			"name":  info.AuthorName,
			"email": info.AuthorEmail,
			"date":  time.Unix(info.Timestamp, 0).UTC().Format("2006-01-02T15:04:05") + ".000Z",
		}
		// GitHub's REST API returns the message without the trailing newline the object carries.
		message := info.Message
		if strings.HasSuffix(message, "\n") {
			message = message[:len(message)-1]
		}
		parents := make([]interface{}, 0, len(info.Parents))
		for _, p := range info.Parents {
			parents = append(parents, map[string]interface{}{"sha": p})
		}
		out = append(out, map[string]interface{}{
			"sha": oid,
			"commit": map[string]interface{}{
				"message": message,
				"author":  who,
				"committer": map[string]interface{}{
					"name":  info.CommitterName,
					"email": info.CommitterEmail,
					"date":  time.Unix(info.Timestamp, 0).UTC().Format("2006-01-02T15:04:05") + ".000Z",
				},
				"tree": map[string]interface{}{"sha": info.Tree},
			},
			"author":    map[string]interface{}{"login": Viewer.Login},
			"committer": map[string]interface{}{"login": Viewer.Login},
			"parents":   parents,
		})
		for _, parent := range info.Parents {
			if !seen[parent] {
				queue = append(queue, parent)
			}
		}
		if len(out) >= 1000 {
			break
		}
	}
	return out
}

func bodyMap(body interface{}) map[string]interface{} {
	if m, ok := body.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func bodyStr(body map[string]interface{}, key string) string {
	if v, ok := body[key].(string); ok {
		return v
	}
	return ""
}

func handleRest(store *Store, req *RestRequest) RestResult {
	seg := req.Segments
	host := req.Host
	body := bodyMap(req.Body)

	// GET /api/v3/ (root) and /api/v3 (no slash)
	if len(seg) == 0 {
		if req.Method == "GET" {
			return restJSON(200, map[string]interface{}{}, map[string]string{"X-OAuth-Scopes": "repo, user"})
		}
		return restNotFound("Not Found")
	}

	// GET /api/v3/meta
	if len(seg) == 1 && seg[0] == "meta" {
		if req.Method != "GET" {
			return restNotFound()
		}
		return restJSON(200, map[string]interface{}{"installed_version": "1.0.0", "verifiable_password_authentication": false}, nil)
	}

	// GET /api/v3/user
	if len(seg) == 1 && seg[0] == "user" {
		if req.Method != "GET" {
			return restNotFound()
		}
		return restJSON(200, map[string]interface{}{
			"login":      Viewer.Login,
			"id":         Viewer.ID,
			"node_id":    "U_" + strconv.Itoa(Viewer.ID),
			"name":       Viewer.Name,
			"email":      Viewer.Email,
			"type":       "User",
			"avatar_url": "",
			"html_url":   "http://" + host + "/" + Viewer.Login,
		}, nil)
	}

	// /api/v3/user/repos
	if len(seg) == 2 && seg[0] == "user" && seg[1] == "repos" {
		if req.Method == "GET" {
			repos := store.ListRepos(Viewer.Login)
			page, total, perPage, pageNum := pageOf(repos, req.Query)
			items := []interface{}{}
			for _, r := range page {
				items = append(items, repoJSON(r, host))
			}
			var headers map[string]string
			if link := linkHeader(host, "/api/v3/user/repos", req.Query, total, perPage, pageNum); link != "" {
				headers = map[string]string{"Link": link}
			}
			return restJSON(200, items, headers)
		}
		if req.Method == "POST" {
			name := bodyStr(body, "name")
			if name == "" || !validRepoName(name) {
				return restErr(422, "name is required and must be a valid repository name")
			}
			if existing := store.GetRepo(Viewer.Login, name); existing != nil {
				return restErr(422, "Repository "+Viewer.Login+"/"+name+" already exists")
			}
			priv, _ := body["private"].(bool)
			repo := store.CreateRepo(Viewer.Login, name, CreateRepoOptions{
				Description: bodyStr(body, "description"),
				Private:     priv,
			})
			return restJSON(201, repoJSON(repo, host), nil)
		}
		return restNotFound()
	}

	// /api/v3/repos/{owner}/{repo}[/...]
	if len(seg) >= 3 && seg[0] == "repos" {
		owner := seg[1]
		repoName := seg[2]
		if owner != Viewer.Login {
			return restNotFound("Repository " + owner + "/" + repoName + " not found")
		}
		repo := store.GetRepo(owner, repoName)
		rest := seg[3:]

		// GET/PATCH/DELETE /repos/{owner}/{repo}
		if len(rest) == 0 {
			if req.Method == "GET" {
				if repo == nil {
					return restNotFound("Repository " + owner + "/" + repoName + " not found")
				}
				return restJSON(200, repoJSON(repo, host), nil)
			}
			if req.Method == "PATCH" {
				if repo == nil {
					return restNotFound("Repository " + owner + "/" + repoName + " not found")
				}
				if v, ok := body["description"].(string); ok {
					repo.Description = v
				}
				if v, ok := body["private"].(bool); ok {
					repo.Private = v
				}
				repo.UpdatedAt = store.NowIso()
				return restJSON(200, repoJSON(repo, host), nil)
			}
			if req.Method == "DELETE" {
				if repo == nil {
					return restNotFound("Repository " + owner + "/" + repoName + " not found")
				}
				store.DeleteRepo(owner, repoName)
				return RestResult{Status: 204, Body: nil}
			}
			return restNotFound()
		}
		if repo == nil {
			return restNotFound("Repository " + owner + "/" + repoName + " not found")
		}

		// /branches
		if rest[0] == "branches" {
			if len(rest) == 1 && req.Method == "GET" {
				branches := []interface{}{}
				for _, r := range listRefs(repo) {
					if !strings.HasPrefix(r.Name, "refs/heads/") {
						continue
					}
					branches = append(branches, map[string]interface{}{
						"name":      r.Name[len("refs/heads/"):],
						"commit":    map[string]interface{}{"sha": r.OID},
						"protected": false,
					})
				}
				sort.Slice(branches, func(i, j int) bool {
					return branches[i].(map[string]interface{})["name"].(string) < branches[j].(map[string]interface{})["name"].(string)
				})
				return restJSON(200, branches, nil)
			}
			if len(rest) == 2 && req.Method == "GET" {
				name := rest[1]
				tip, ok := repo.Refs["refs/heads/"+name]
				if !ok {
					return restNotFound("Branch " + name + " not found")
				}
				return restJSON(200, map[string]interface{}{"name": name, "commit": map[string]interface{}{"sha": tip}, "protected": false}, nil)
			}
			return restNotFound()
		}

		// /commits
		if rest[0] == "commits" && len(rest) == 1 && req.Method == "GET" {
			shaParam := req.Query.Get("sha")
			var tip string
			if shaParam != "" {
				if t, ok := repo.Refs["refs/heads/"+shaParam]; ok {
					tip = t
				} else if isOid(shaParam) {
					tip = shaParam
				}
			} else {
				tip = store.HeadTip(repo)
			}
			commits := commitsFrom(repo, tip)
			items := []interface{}{}
			for _, c := range commits {
				items = append(items, c)
			}
			return restJSON(200, items, nil)
		}

		// /git/refs
		if rest[0] == "git" && len(rest) >= 2 && rest[1] == "refs" {
			if req.Method == "POST" && len(rest) == 2 {
				ref := bodyStr(body, "ref")
				sha := bodyStr(body, "sha")
				if !strings.HasPrefix(ref, "refs/") || !isOid(sha) {
					return restErr(422, "ref and sha are required")
				}
				if _, ok := repo.Objects[sha]; !ok {
					return restErr(422, "object "+sha+" does not exist")
				}
				if _, ok := repo.Refs[ref]; ok {
					return restErr(422, "Reference "+ref+" already exists")
				}
				repo.Refs[ref] = sha
				return restJSON(201, map[string]interface{}{"ref": ref, "object": map[string]interface{}{"sha": sha, "type": "commit"}}, nil)
			}
			if req.Method == "GET" {
				wanted := strings.Join(rest[2:], "/")
				if wanted == "" {
					items := []interface{}{}
					for _, r := range listRefs(repo) {
						items = append(items, map[string]interface{}{"ref": r.Name, "object": map[string]interface{}{"sha": r.OID, "type": "commit"}})
					}
					return restJSON(200, items, nil)
				}
				full := "refs/" + wanted
				tip, ok := repo.Refs[full]
				if !ok {
					return restNotFound("Reference " + full + " not found")
				}
				return restJSON(200, map[string]interface{}{"ref": full, "object": map[string]interface{}{"sha": tip, "type": "commit"}}, nil)
			}
			return restNotFound()
		}

		// /contents/{path...}
		if rest[0] == "contents" {
			path := strings.Join(rest[1:], "/")
			return handleContents(store, repo, req, path)
		}

		// /labels
		if rest[0] == "labels" {
			if req.Method == "GET" && len(rest) == 1 {
				items := []interface{}{}
				for _, name := range repo.LabelOrder {
					l := repo.Labels[name]
					items = append(items, map[string]interface{}{"name": l.Name, "color": l.Color, "description": l.Description})
				}
				return restJSON(200, items, nil)
			}
			if req.Method == "POST" && len(rest) == 1 {
				name := bodyStr(body, "name")
				if name == "" {
					return restErr(422, "name is required")
				}
				color := bodyStr(body, "color")
				if color == "" {
					color = "ededed"
				}
				if existing, ok := repo.Labels[name]; ok {
					return restJSON(201, map[string]interface{}{"name": existing.Name, "color": existing.Color, "description": existing.Description}, nil)
				}
				label := &Label{Name: name, Color: color, Description: bodyStr(body, "description")}
				repo.Labels[name] = label
				repo.LabelOrder = append(repo.LabelOrder, name)
				return restJSON(201, map[string]interface{}{"name": label.Name, "color": label.Color, "description": label.Description}, nil)
			}
			return restNotFound()
		}

		// /issues[/...]
		if rest[0] == "issues" {
			return handleIssues(store, repo, req, rest[1:])
		}

		// /pulls[/...]
		if rest[0] == "pulls" {
			return handlePulls(store, repo, req, rest[1:])
		}

		return restNotFound("Not Found")
	}

	return restNotFound("Not Found")
}

func handleContents(store *Store, repo *Repo, req *RestRequest, path string) RestResult {
	body := bodyMap(req.Body)
	author := Viewer.Name + " <" + Viewer.Email + ">"
	branch := bodyStr(body, "branch")
	if branch == "" {
		if q := req.Query.Get("branch"); q != "" {
			branch = q
		} else {
			branch = repo.DefaultBranch
		}
	}
	readRef := branch
	if refParam := req.Query.Get("ref"); refParam != "" {
		readRef = refParam
	}

	if req.Method == "GET" {
		refName := "refs/heads/" + readRef
		tip, ok := repo.Refs[refName]
		if !ok {
			return restNotFound("Branch " + readRef + " not found")
		}
		tree := store.CommitTree(repo, tip)
		if tree == "" {
			return restNotFound("File " + path + " not found")
		}
		if foundOID, _, found := treeLookup(repo.Objects, tree, path); found {
			obj := repo.Objects[foundOID]
			if obj != nil && obj.Type == ObjBlob {
				return restJSON(200, map[string]interface{}{
					"type":     "file",
					"encoding": "base64",
					"size":     len(obj.Data),
					"name":     baseName(path),
					"path":     path,
					"content":  base64.StdEncoding.EncodeToString(obj.Data),
					"sha":      foundOID,
				}, nil)
			}
			// A tree at this path is a directory: fall through to the listing below.
			if obj == nil || obj.Type != ObjTree {
				return restNotFound("File " + path + " not found")
			}
		}
		// Directory listing: immediate children of path ('' = root).
		files := treeFlatten(repo.Objects, tree)
		prefix := ""
		if path != "" {
			prefix = path + "/"
		}
		children := map[string]map[string]interface{}{}
		var order []string
		for _, file := range files {
			if !strings.HasPrefix(file.Path, prefix) {
				continue
			}
			rest := file.Path[len(prefix):]
			if rest == "" {
				continue
			}
			if slash := strings.Index(rest, "/"); slash == -1 {
				if _, ok := children[rest]; !ok {
					order = append(order, rest)
				}
				children[rest] = map[string]interface{}{"type": "file", "name": rest, "path": file.Path, "sha": file.OID}
			} else {
				dirName := rest[:slash]
				if _, ok := children[dirName]; !ok {
					order = append(order, dirName)
					children[dirName] = map[string]interface{}{"type": "dir", "name": dirName, "path": prefix + dirName, "sha": dirTreeSha(repo, tree, prefix+dirName)}
				}
			}
		}
		if len(children) == 0 {
			return restNotFound("File " + path + " not found")
		}
		sort.Strings(order)
		items := make([]interface{}, 0, len(order))
		for _, name := range order {
			items = append(items, children[name])
		}
		return restJSON(200, items, nil)
	}

	if req.Method == "PUT" {
		message := bodyStr(body, "message")
		if message == "" {
			message = "Update file"
		}
		contentB64 := bodyStr(body, "content")
		if contentB64 == "" {
			return restErr(422, "content is required")
		}
		cleaned := strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == '\t' || r == ' ' {
				return -1
			}
			return r
		}, contentB64)
		content, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			content = []byte{}
		}
		sha := bodyStr(body, "sha")
		hasSha := false
		if _, ok := body["sha"]; ok {
			if _, isStr := body["sha"].(string); isStr {
				hasSha = true
			}
		}
		// Branch: create from the default tip when it does not exist yet.
		refName := "refs/heads/" + branch
		tip, hasTip := repo.Refs[refName]
		if !hasTip && branch != repo.DefaultBranch {
			if base := store.HeadTip(repo); base != "" {
				repo.Refs[refName] = base
			}
			tip, hasTip = repo.Refs[refName]
		}
		var existingOID string
		var existingOK bool
		if hasTip {
			_, _, _, existingOK = store.ReadFile(repo, refName, path)
			if existingOK {
				existingOID, _, _, _ = store.ReadFile(repo, refName, path)
			}
		}
		_ = tip
		isCreate := !existingOK
		if !isCreate && hasSha && sha != existingOID {
			return restErr(409, "sha "+sha+" does not match the current blob "+existingOID)
		}
		commit, blob, _, _ := store.CommitFile(repo, branch, path, content, true, message, author)
		status := 200
		if isCreate {
			status = 201
		}
		return restJSON(status, map[string]interface{}{
			"content": map[string]interface{}{"name": baseName(path), "path": path, "sha": blob, "size": len(content), "encoding": "base64"},
			"commit":  map[string]interface{}{"sha": commit, "message": message},
		}, nil)
	}

	if req.Method == "DELETE" {
		message := bodyStr(body, "message")
		if message == "" {
			message = "Delete file"
		}
		sha := bodyStr(body, "sha")
		hasSha := false
		if _, ok := body["sha"]; ok {
			if _, isStr := body["sha"].(string); isStr {
				hasSha = true
			}
		}
		refName := "refs/heads/" + branch
		existingOID, _, _, existingOK := store.ReadFile(repo, refName, path)
		if !existingOK {
			return restNotFound("File " + path + " not found")
		}
		if hasSha && sha != existingOID {
			return restErr(409, "sha "+sha+" does not match the current blob "+existingOID)
		}
		commit, _, _, _ := store.CommitFile(repo, branch, path, nil, false, message, author)
		return restJSON(200, map[string]interface{}{"commit": map[string]interface{}{"sha": commit, "message": message}}, nil)
	}

	return restNotFound()
}

func baseName(path string) string {
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

func dirTreeSha(repo *Repo, rootTree, dirPath string) string {
	// Walk the tree to the directory and return its tree oid (or the root tree when not found).
	parts := strings.Split(dirPath, "/")
	var filtered []string
	for _, p := range parts {
		if p != "" {
			filtered = append(filtered, p)
		}
	}
	current := rootTree
	for _, part := range filtered {
		entries := parseTree(nil)
		if obj := repo.Objects[current]; obj != nil {
			entries = parseTree(obj.Data)
		}
		next := ""
		for _, e := range entries {
			if e.Name == part && e.Mode == "40000" {
				next = e.OID
				break
			}
		}
		if next == "" {
			return current
		}
		current = next
	}
	return current
}

func handleIssues(store *Store, repo *Repo, req *RestRequest, rest []string) RestResult {
	body := bodyMap(req.Body)
	// /issues
	if len(rest) == 0 {
		if req.Method == "POST" {
			title := bodyStr(body, "title")
			issueBody := bodyStr(body, "body")
			var labels []string
			if arr, ok := body["labels"].([]interface{}); ok {
				for _, l := range arr {
					if s, ok := l.(string); ok {
						labels = append(labels, s)
					}
				}
			}
			if title == "" {
				return restErr(422, "title is required")
			}
			for _, label := range labels {
				if _, ok := repo.Labels[label]; !ok {
					return restErr(422, "Label "+label+" does not exist")
				}
			}
			issue := store.CreateIssue(repo, title, issueBody, labels, Viewer.Login)
			return restJSON(201, issueJSON(issue, repo), nil)
		}
		if req.Method == "GET" {
			state := req.Query.Get("state")
			if state == "" {
				state = "open"
			}
			sortField := req.Query.Get("sort")
			if sortField == "" {
				sortField = "created"
			}
			direction := req.Query.Get("direction")
			if direction == "" {
				direction = "desc"
			}
			var issues []*Issue
			for _, i := range repo.Issues {
				issues = append(issues, i)
			}
			if state == "open" {
				filtered := issues[:0]
				for _, i := range issues {
					if i.State == "open" {
						filtered = append(filtered, i)
					}
				}
				issues = filtered
			} else if state == "closed" {
				filtered := issues[:0]
				for _, i := range issues {
					if i.State == "closed" {
						filtered = append(filtered, i)
					}
				}
				issues = filtered
			}
			if sortField == "created" {
				sort.Slice(issues, func(a, b int) bool {
					if issues[a].CreatedAt != issues[b].CreatedAt {
						return issues[a].CreatedAt < issues[b].CreatedAt
					}
					return issues[a].Number < issues[b].Number
				})
				if direction == "desc" {
					for i, j := 0, len(issues)-1; i < j; i, j = i+1, j-1 {
						issues[i], issues[j] = issues[j], issues[i]
					}
				}
			}
			page, total, perPage, pageNum := pageOf(issues, req.Query)
			items := []interface{}{}
			for _, i := range page {
				items = append(items, issueJSON(i, repo))
			}
			var headers map[string]string
			if link := linkHeader(req.Host, "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/issues", req.Query, total, perPage, pageNum); link != "" {
				headers = map[string]string{"Link": link}
			}
			return restJSON(200, items, headers)
		}
		return restNotFound()
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil || number < 1 {
		return restNotFound()
	}
	issue, ok := repo.Issues[number]
	if !ok {
		return restNotFound("Issue #" + strconv.Itoa(number) + " not found")
	}
	// /issues/{n}
	if len(rest) == 1 {
		if req.Method == "GET" {
			return restJSON(200, issueJSON(issue, repo), nil)
		}
		if req.Method == "PATCH" {
			if v, ok := body["state"].(string); ok && (v == "open" || v == "closed") {
				issue.State = v
			}
			if v, ok := body["title"].(string); ok {
				issue.Title = v
			}
			if v, ok := body["body"].(string); ok {
				issue.Body = v
			}
			issue.UpdatedAt = store.NowIso()
			return restJSON(200, issueJSON(issue, repo), nil)
		}
		return restNotFound()
	}
	// /issues/{n}/comments
	if len(rest) == 2 && rest[1] == "comments" {
		if req.Method == "GET" {
			items := []interface{}{}
			for _, c := range issue.Comments {
				items = append(items, map[string]interface{}{"id": c.ID, "body": c.Body, "user": map[string]interface{}{"login": c.User}, "created_at": c.CreatedAt, "updated_at": c.UpdatedAt})
			}
			return restJSON(200, items, nil)
		}
		if req.Method == "POST" {
			commentBody := bodyStr(body, "body")
			if commentBody == "" {
				return restErr(422, "body is required")
			}
			comment := &IssueComment{
				ID:        repo.NextCommentID,
				Body:      commentBody,
				User:      Viewer.Login,
				CreatedAt: store.NowIso(),
				UpdatedAt: store.NowIso(),
			}
			repo.NextCommentID++
			issue.Comments = append(issue.Comments, comment)
			return restJSON(201, map[string]interface{}{"id": comment.ID, "body": comment.Body, "user": map[string]interface{}{"login": comment.User}, "created_at": comment.CreatedAt, "updated_at": comment.UpdatedAt}, nil)
		}
		return restNotFound()
	}
	return restNotFound()
}

func handlePulls(store *Store, repo *Repo, req *RestRequest, rest []string) RestResult {
	body := bodyMap(req.Body)
	// /pulls
	if len(rest) == 0 {
		if req.Method == "POST" {
			title := bodyStr(body, "title")
			pullBody := bodyStr(body, "body")
			head := bodyStr(body, "head")
			base := bodyStr(body, "base")
			if base == "" {
				base = repo.DefaultBranch
			}
			if title == "" || head == "" {
				return restErr(422, "title and head are required")
			}
			if _, ok := repo.Refs["refs/heads/"+head]; !ok {
				return restErr(422, "head branch "+head+" does not exist")
			}
			if _, ok := repo.Refs["refs/heads/"+base]; !ok {
				return restErr(422, "base branch "+base+" does not exist")
			}
			pull := store.CreatePull(repo, CreatePullInput{Title: title, Body: pullBody, Head: head, Base: base, User: Viewer.Login})
			if pull == nil {
				return restErr(422, "could not create pull request")
			}
			return restJSON(201, pullJSON(pull, repo), nil)
		}
		if req.Method == "GET" {
			state := req.Query.Get("state")
			if state == "" {
				state = "open"
			}
			var pulls []*Pull
			for _, p := range repo.Pulls {
				pulls = append(pulls, p)
			}
			if state == "open" {
				filtered := pulls[:0]
				for _, p := range pulls {
					if p.State == "open" {
						filtered = append(filtered, p)
					}
				}
				pulls = filtered
			} else if state == "closed" {
				filtered := pulls[:0]
				for _, p := range pulls {
					if p.State == "closed" {
						filtered = append(filtered, p)
					}
				}
				pulls = filtered
			}
			sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number > pulls[j].Number })
			page, total, perPage, pageNum := pageOf(pulls, req.Query)
			items := []interface{}{}
			for _, p := range page {
				items = append(items, pullJSON(p, repo))
			}
			var headers map[string]string
			if link := linkHeader(req.Host, "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/pulls", req.Query, total, perPage, pageNum); link != "" {
				headers = map[string]string{"Link": link}
			}
			return restJSON(200, items, headers)
		}
		return restNotFound()
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil || number < 1 {
		return restNotFound()
	}
	pull, ok := repo.Pulls[number]
	if !ok {
		return restNotFound("Pull request #" + strconv.Itoa(number) + " not found")
	}
	// /pulls/{n}
	if len(rest) == 1 {
		if req.Method == "GET" {
			store.RefreshPullHead(repo, pull)
			return restJSON(200, pullJSON(pull, repo), nil)
		}
		return restNotFound()
	}
	// /pulls/{n}/files
	if len(rest) == 2 && rest[1] == "files" && req.Method == "GET" {
		store.RefreshPullHead(repo, pull)
		items := []interface{}{}
		for _, f := range pull.Files {
			status := f.Status
			if status == "removed" {
				status = "modified"
			}
			items = append(items, map[string]interface{}{
				"filename":  f.Filename,
				"status":    status,
				"additions": f.Additions,
				"deletions": f.Deletions,
				"changes":   f.Changes,
			})
		}
		return restJSON(200, items, nil)
	}
	// /pulls/{n}/merge
	if len(rest) == 2 && rest[1] == "merge" && req.Method == "PUT" {
		if pull.State == "closed" {
			return restErr(405, "Pull request #"+strconv.Itoa(number)+" is already closed")
		}
		store.RefreshPullHead(repo, pull)
		if pull.Mergeable == "CONFLICTING" {
			return restErr(405, "Pull request #"+strconv.Itoa(number)+" has conflicts")
		}
		author := Viewer.Name + " <" + Viewer.Email + ">"
		message := "Merge pull request #" + strconv.Itoa(number) + " from " + repo.Owner + "/" + pull.HeadRef
		sha := store.SquashMerge(repo, pull, message, author)
		pull.Merged = true
		pull.MergedAt = store.NowIso()
		pull.HasMergedAt = true
		pull.MergeCommitSha = sha
		pull.HasMergeCommitSha = true
		pull.State = "closed"
		pull.UpdatedAt = store.NowIso()
		return restJSON(200, map[string]interface{}{"sha": sha, "merged": true, "message": message}, nil)
	}
	return restNotFound()
}

// /^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/
func validRepoName(name string) bool {
	if len(name) < 1 || len(name) > 100 {
		return false
	}
	c := name[0]
	if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
		return false
	}
	for i := 1; i < len(name); i++ {
		c = name[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
