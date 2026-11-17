// GraphQL v4 at /api/graphql: a small parser + resolver for the documents the suite sends
// (viewer, repository + issues/pulls connections, node, __type introspection, and the five
// mutations). Unknown fields are a GraphQL error (HTTP 200 + errors array); an unknown
// repository is data.repository === null with no errors.
// Faithful port of lib/graphql.ts.
package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// -------------------------------------------------------------------------------------------
// Lexer / parser for the GraphQL document subset the suite uses
// -------------------------------------------------------------------------------------------

// Value is a parsed GraphQL literal: string | number | bool | nil | []Value |
// map[string]Value, plus varRef/enumRef markers.
type varRef struct{ Name string }
type enumRef struct{ Name string }

type FieldNode struct {
	Alias      string
	Name       string
	Args       map[string]interface{}
	Selections []Selection
}

type FragmentNode struct {
	TypeCondition string
	HasCondition  bool
	Selections    []Selection
}

type Selection struct {
	IsFragment bool
	Field      *FieldNode
	Fragment   *FragmentNode
}

type Operation struct {
	Kind       string // 'query' | 'mutation'
	Name       string
	HasName    bool
	Selections []Selection
}

type tokenKind int

const (
	tokName tokenKind = iota
	tokString
	tokInt
	tokPunct
	tokSpread
)

type token struct {
	kind  tokenKind
	value string
	num   float64
}

func tokenize(source string) ([]token, error) {
	var tokens []token
	i := 0
	for i < len(source) {
		ch := source[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',':
			i++
		case ch == '#':
			for i < len(source) && source[i] != '\n' {
				i++
			}
		case ch == '.' && i+2 < len(source) && source[i+1] == '.' && source[i+2] == '.':
			tokens = append(tokens, token{kind: tokSpread, value: "..."})
			i += 3
		case strings.ContainsRune("{}()[]:!=$@&|", rune(ch)):
			tokens = append(tokens, token{kind: tokPunct, value: string(ch)})
			i++
		case ch == '"':
			j := i + 1
			var out strings.Builder
			for j < len(source) && source[j] != '"' {
				if source[j] == '\\' && j+1 < len(source) {
					esc := source[j+1]
					switch esc {
					case 'n':
						out.WriteByte('\n')
					case 't':
						out.WriteByte('\t')
					case 'r':
						out.WriteByte('\r')
					default:
						out.WriteByte(esc)
					}
					j += 2
				} else {
					out.WriteByte(source[j])
					j++
				}
			}
			tokens = append(tokens, token{kind: tokString, value: out.String()})
			i = j + 1
		case ch >= '0' && ch <= '9' || ch == '-':
			j := i
			for j < len(source) && (source[j] >= '0' && source[j] <= '9' || source[j] == '.') {
				j++
			}
			num, err := strconv.ParseFloat(source[i:j], 64)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, token{kind: tokInt, value: source[i:j], num: num})
			i = j
		case ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch == '_':
			j := i
			for j < len(source) && (source[j] >= 'A' && source[j] <= 'Z' || source[j] >= 'a' && source[j] <= 'z' || source[j] >= '0' && source[j] <= '9' || source[j] == '_') {
				j++
			}
			tokens = append(tokens, token{kind: tokName, value: source[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q in GraphQL document", ch)
		}
	}
	return tokens, nil
}

type parser struct {
	pos    int
	tokens []token
}

func (p *parser) peek() *token {
	if p.pos < len(p.tokens) {
		return &p.tokens[p.pos]
	}
	return nil
}

func (p *parser) next() (token, error) {
	if p.pos >= len(p.tokens) {
		return token{}, errors.New("unexpected end of GraphQL document")
	}
	tok := p.tokens[p.pos]
	p.pos++
	return tok, nil
}

func (p *parser) expectPunct(value string) error {
	tok, err := p.next()
	if err != nil {
		return err
	}
	if tok.kind != tokPunct || tok.value != value {
		return fmt.Errorf("expected %s, got %v", value, tok)
	}
	return nil
}

func (p *parser) expectName() (string, error) {
	tok, err := p.next()
	if err != nil {
		return "", err
	}
	if tok.kind != tokName {
		return "", fmt.Errorf("expected a name, got %v", tok)
	}
	return tok.value, nil
}

func (p *parser) parseDocument() ([]Operation, error) {
	var operations []Operation
	for p.pos < len(p.tokens) {
		tok := p.peek()
		if tok.kind == tokPunct && tok.value == "{" {
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			operations = append(operations, Operation{Kind: "query", Selections: sels})
			continue
		}
		if tok.kind == tokName && (tok.value == "query" || tok.value == "mutation") {
			kind := tok.value
			p.next()
			op := Operation{Kind: kind}
			if next := p.peek(); next != nil && next.kind == tokName {
				name, _ := p.next()
				op.Name, op.HasName = name.value, true
			}
			// Variable definitions: ($name: Type, ...) — skip, we only need the names at runtime.
			if next := p.peek(); next != nil && next.kind == tokPunct && next.value == "(" {
				depth := 0
				for p.pos < len(p.tokens) {
					t, _ := p.next()
					if t.kind == tokPunct && t.value == "(" {
						depth++
					}
					if t.kind == tokPunct && t.value == ")" {
						depth--
						if depth == 0 {
							break
						}
					}
				}
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			op.Selections = sels
			operations = append(operations, op)
			continue
		}
		return nil, fmt.Errorf("unexpected token %v at top level of GraphQL document", *tok)
	}
	return operations, nil
}

func (p *parser) parseSelectionSet() ([]Selection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var selections []Selection
	for {
		tok := p.peek()
		if tok == nil {
			return nil, errors.New("unterminated selection set")
		}
		if tok.kind == tokPunct && tok.value == "}" {
			p.next()
			return selections, nil
		}
		if tok.kind == tokSpread {
			p.next()
			frag := &FragmentNode{}
			if next := p.peek(); next != nil && next.kind == tokName && next.value == "on" {
				p.next()
				name, err := p.expectName()
				if err != nil {
					return nil, err
				}
				frag.TypeCondition, frag.HasCondition = name, true
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			frag.Selections = sels
			selections = append(selections, Selection{IsFragment: true, Fragment: frag})
			continue
		}
		field, err := p.parseField()
		if err != nil {
			return nil, err
		}
		selections = append(selections, Selection{Field: field})
	}
}

func (p *parser) parseField() (*FieldNode, error) {
	name, err := p.expectName()
	if err != nil {
		return nil, err
	}
	alias := name
	if colon := p.peek(); colon != nil && colon.kind == tokPunct && colon.value == ":" {
		p.next()
		alias = name
		name, err = p.expectName()
		if err != nil {
			return nil, err
		}
	}
	args := map[string]interface{}{}
	if next := p.peek(); next != nil && next.kind == tokPunct && next.value == "(" {
		p.next()
		for {
			tok := p.peek()
			if tok == nil {
				return nil, errors.New("unterminated argument list")
			}
			if tok.kind == tokPunct && tok.value == ")" {
				p.next()
				break
			}
			argName, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			args[argName] = val
		}
	}
	var selections []Selection
	if next := p.peek(); next != nil && next.kind == tokPunct && next.value == "{" {
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		selections = sels
	}
	return &FieldNode{Alias: alias, Name: name, Args: args, Selections: selections}, nil
}

func (p *parser) parseValue() (interface{}, error) {
	tok, err := p.next()
	if err != nil {
		return nil, err
	}
	switch tok.kind {
	case tokString:
		return tok.value, nil
	case tokInt:
		return tok.num, nil
	case tokName:
		switch tok.value {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		}
		return enumRef{Name: tok.value}, nil
	case tokPunct:
		switch tok.value {
		case "$":
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			return varRef{Name: name}, nil
		case "[":
			items := []interface{}{}
			for {
				next := p.peek()
				if next == nil {
					return nil, errors.New("unterminated list")
				}
				if next.kind == tokPunct && next.value == "]" {
					p.next()
					return items, nil
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				items = append(items, v)
			}
		case "{":
			obj := map[string]interface{}{}
			for {
				next := p.peek()
				if next == nil {
					return nil, errors.New("unterminated input object")
				}
				if next.kind == tokPunct && next.value == "}" {
					p.next()
					return obj, nil
				}
				key, err := p.expectName()
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				obj[key] = v
			}
		}
	}
	return nil, fmt.Errorf("unexpected value token %v", tok)
}

func resolveValue(value interface{}, variables map[string]interface{}) interface{} {
	switch v := value.(type) {
	case varRef:
		if variables != nil {
			if val, ok := variables[v.Name]; ok {
				return val
			}
		}
		return nil
	case enumRef:
		return v.Name
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, item := range v {
			out[i] = resolveValue(item, variables)
		}
		return out
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, item := range v {
			out[k] = resolveValue(item, variables)
		}
		return out
	}
	return value
}

func resolveArgs(args map[string]interface{}, variables map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range args {
		out[k] = resolveValue(v, variables)
	}
	return out
}

func asInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	}
	return 0, false
}

func asString(v interface{}) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

var _ = base64.StdEncoding

// -------------------------------------------------------------------------------------------
// Introspection type table
// -------------------------------------------------------------------------------------------

type TypeDef struct {
	Kind   string // 'OBJECT' | 'ENUM'
	Fields []string
	Values []string
}

var typeTable = map[string]TypeDef{
	"Query":                     {Kind: "OBJECT", Fields: []string{"viewer", "repository", "node", "__type", "__schema"}},
	"Mutation":                  {Kind: "OBJECT", Fields: []string{"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"}},
	"User":                      {Kind: "OBJECT", Fields: []string{"id", "login", "name", "email"}},
	"Repository":                {Kind: "OBJECT", Fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequest", "pullRequests", "description", "isPrivate", "createdAt", "updatedAt"}},
	"RepositoryOwner":           {Kind: "OBJECT", Fields: []string{"id", "login"}},
	"Ref":                       {Kind: "OBJECT", Fields: []string{"id", "name"}},
	"Issue":                     {Kind: "OBJECT", Fields: []string{"id", "number", "title", "body", "state", "createdAt", "updatedAt", "author", "labels", "comments", "repository", "url"}},
	"IssueConnection":           {Kind: "OBJECT", Fields: []string{"nodes", "edges", "pageInfo", "totalCount"}},
	"IssueEdge":                 {Kind: "OBJECT", Fields: []string{"node", "cursor"}},
	"PullRequest":               {Kind: "OBJECT", Fields: []string{"id", "number", "title", "body", "state", "headRefName", "baseRefName", "headRef", "baseRef", "mergeable", "merged", "mergeCommit", "files", "author", "createdAt", "updatedAt", "repository"}},
	"PullRequestConnection":     {Kind: "OBJECT", Fields: []string{"nodes", "edges", "pageInfo", "totalCount"}},
	"PullRequestEdge":           {Kind: "OBJECT", Fields: []string{"node", "cursor"}},
	"PullRequestFileConnection": {Kind: "OBJECT", Fields: []string{"nodes", "edges", "pageInfo", "totalCount"}},
	"PullRequestFile":           {Kind: "OBJECT", Fields: []string{"path", "additions", "deletions", "changeType"}},
	"PageInfo":                  {Kind: "OBJECT", Fields: []string{"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"}},
	"Comment":                   {Kind: "OBJECT", Fields: []string{"id", "body", "author", "createdAt", "updatedAt"}},
	"Commit":                    {Kind: "OBJECT", Fields: []string{"oid", "message", "author", "committer"}},
	"CommentEdge":               {Kind: "OBJECT", Fields: []string{"node", "cursor"}},
	"AddCommentPayload":         {Kind: "OBJECT", Fields: []string{"commentEdge", "clientMutationId"}},
	"CreateIssuePayload":        {Kind: "OBJECT", Fields: []string{"issue", "clientMutationId"}},
	"CloseIssuePayload":         {Kind: "OBJECT", Fields: []string{"issue", "clientMutationId"}},
	"CreatePullRequestPayload":  {Kind: "OBJECT", Fields: []string{"pullRequest", "clientMutationId"}},
	"MergePullRequestPayload":   {Kind: "OBJECT", Fields: []string{"pullRequest", "clientMutationId"}},
	"__Type":                    {Kind: "OBJECT", Fields: []string{"name", "kind", "fields", "enumValues", "description"}},
	"__Field":                   {Kind: "OBJECT", Fields: []string{"name", "description", "type"}},
	"__EnumValue":               {Kind: "OBJECT", Fields: []string{"name", "description"}},
	"IssueState":                {Kind: "ENUM", Values: []string{"OPEN", "CLOSED"}},
	"MergeableState":            {Kind: "ENUM", Values: []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}},
	"PullRequestState":          {Kind: "ENUM", Values: []string{"OPEN", "CLOSED", "MERGED"}},
	"PullRequestMergeMethod":    {Kind: "ENUM", Values: []string{"MERGE", "SQUASH", "REBASE"}},
	"RepositoryPermission":      {Kind: "ENUM", Values: []string{"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"}},
	"SearchType":                {Kind: "ENUM", Values: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION"}},
	"IssueOrderField":           {Kind: "ENUM", Values: []string{"CREATED_AT", "UPDATED_AT", "COMMENTS"}},
	"OrderDirection":            {Kind: "ENUM", Values: []string{"ASC", "DESC"}},
}

// -------------------------------------------------------------------------------------------
// Execution
// -------------------------------------------------------------------------------------------

// GqlObject is a resolved node: its __typename plus the typed values its fields read.
type GqlObject struct {
	Typename string
	Repo     *Repo
	Issue    *Issue
	Pull     *Pull
	Nodes    []*GqlObject
	PageInfo map[string]interface{}
	// Scalar / misc payloads.
	Login       string
	RefName     string
	TypeName    string
	TypeDef     *TypeDef
	FieldName   string
	EnumName    string
	CommentID   string
	CommentBody string
	CommentAt   string
	CommitSha   string
	File        *PullFile
	Node        *GqlObject
	Cursor      string
	HasCursor   bool
	HasLogin    bool
	HasRefName  bool
	HasTypeName bool
	HasTypeDef  bool
}

type gqlCtx struct {
	store *Store
}

type gqlError struct{ Message string }

func cursorForIssue(issue *Issue) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(issue.Number)))
}

func issueFromCursor(repo *Repo, cursor string) *Issue {
	if cursor == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return nil
	}
	text := string(raw)
	if !strings.HasPrefix(text, "cursor:") {
		return nil
	}
	number, err := strconv.Atoi(text[len("cursor:"):])
	if err != nil {
		return nil
	}
	return repo.Issues[number]
}

func repoObject(repo *Repo) *GqlObject {
	return &GqlObject{Typename: "Repository", Repo: repo}
}

func issueObject(repo *Repo, issue *Issue) *GqlObject {
	return &GqlObject{Typename: "Issue", Repo: repo, Issue: issue}
}

func pullObject(repo *Repo, pull *Pull) *GqlObject {
	return &GqlObject{Typename: "PullRequest", Repo: repo, Pull: pull}
}

func issuesConnection(repo *Repo, args map[string]interface{}) *GqlObject {
	first := 10
	if n, ok := asInt(args["first"]); ok {
		first = n
	}
	after, _ := args["after"].(string)
	var states []string
	if arr, ok := args["states"].([]interface{}); ok {
		for _, s := range arr {
			if str, ok := s.(string); ok {
				states = append(states, str)
			}
		}
	}
	orderBy, _ := args["orderBy"].(map[string]interface{})
	var issues []*Issue
	for _, i := range repo.Issues {
		issues = append(issues, i)
	}
	if states != nil {
		wanted := map[string]bool{}
		for _, s := range states {
			wanted[strings.ToLower(s)] = true
		}
		filtered := issues[:0]
		for _, i := range issues {
			if wanted[strings.ToLower(i.State)] {
				filtered = append(filtered, i)
			}
		}
		issues = filtered
	}
	direction := "DESC"
	if orderBy != nil {
		if d, ok := orderBy["direction"].(string); ok {
			direction = strings.ToUpper(d)
		}
	}
	sortSlice(issues, func(a, b *Issue) bool { return a.Number < b.Number })
	if direction == "DESC" {
		reverseIssues(issues)
	}
	if after != "" {
		if anchor := issueFromCursor(repo, after); anchor != nil {
			idx := -1
			for i, iss := range issues {
				if iss.Number == anchor.Number {
					idx = i
					break
				}
			}
			if idx >= 0 {
				issues = issues[idx+1:]
			}
		}
	}
	page := issues
	if len(page) > first {
		page = page[:first]
	}
	hasNextPage := len(issues) > first
	var endCursor interface{}
	var startCursor interface{}
	if len(page) > 0 {
		endCursor = cursorForIssue(page[len(page)-1])
		startCursor = cursorForIssue(page[0])
	}
	nodes := make([]*GqlObject, 0, len(page))
	for _, i := range page {
		nodes = append(nodes, issueObject(repo, i))
	}
	return &GqlObject{
		Typename: "IssueConnection",
		Nodes:    nodes,
		PageInfo: map[string]interface{}{
			"hasNextPage": hasNextPage, "hasPreviousPage": false,
			"startCursor": startCursor, "endCursor": endCursor,
		},
	}
}

func pullsConnection(repo *Repo, args map[string]interface{}) *GqlObject {
	first := 10
	if n, ok := asInt(args["first"]); ok {
		first = n
	}
	var pulls []*Pull
	for _, p := range repo.Pulls {
		pulls = append(pulls, p)
	}
	sortSlice(pulls, func(a, b *Pull) bool { return a.Number > b.Number })
	if len(pulls) > first {
		pulls = pulls[:first]
	}
	nodes := make([]*GqlObject, 0, len(pulls))
	for _, p := range pulls {
		nodes = append(nodes, pullObject(repo, p))
	}
	return &GqlObject{
		Typename: "PullRequestConnection",
		Nodes:    nodes,
		PageInfo: map[string]interface{}{
			"hasNextPage": false, "hasPreviousPage": false,
			"startCursor": nil, "endCursor": nil,
		},
	}
}

func sortSlice[T any](s []T, less func(a, b T) bool) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && less(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func reverseIssues(issues []*Issue) {
	for i, j := 0, len(issues)-1; i < j; i, j = i+1, j-1 {
		issues[i], issues[j] = issues[j], issues[i]
	}
}

func findByGlobalID(store *Store, id string) *GqlObject {
	decoded := decodeGlobalID(id)
	if decoded == nil {
		return nil
	}
	switch decoded.Kind {
	case "Repository":
		if len(decoded.Parts) < 1 {
			return nil
		}
		repoID, err := strconv.Atoi(decoded.Parts[0])
		if err != nil {
			return nil
		}
		for _, repo := range store.Repos {
			if repo.ID == repoID {
				return repoObject(repo)
			}
		}
		return nil
	case "Issue":
		if len(decoded.Parts) < 2 {
			return nil
		}
		repoID, err1 := strconv.Atoi(decoded.Parts[0])
		number, err2 := strconv.Atoi(decoded.Parts[1])
		if err1 != nil || err2 != nil {
			return nil
		}
		for _, repo := range store.Repos {
			if repo.ID == repoID {
				if issue := repo.Issues[number]; issue != nil {
					return issueObject(repo, issue)
				}
				return nil
			}
		}
		return nil
	case "PullRequest":
		if len(decoded.Parts) < 2 {
			return nil
		}
		repoID, err1 := strconv.Atoi(decoded.Parts[0])
		number, err2 := strconv.Atoi(decoded.Parts[1])
		if err1 != nil || err2 != nil {
			return nil
		}
		for _, repo := range store.Repos {
			if repo.ID == repoID {
				if pull := repo.Pulls[number]; pull != nil {
					return pullObject(repo, pull)
				}
				return nil
			}
		}
		return nil
	}
	return nil
}

func fieldValue(obj *GqlObject, field string, args map[string]interface{}, ctx *gqlCtx) (interface{}, bool) {
	store := ctx.store
	switch obj.Typename {
	case "Query":
		switch field {
		case "viewer":
			return &GqlObject{Typename: "User"}, true
		case "repository":
			owner, _ := args["owner"].(string)
			name, _ := args["name"].(string)
			if owner != Viewer.Login {
				return nil, true
			}
			repo := store.GetRepo(owner, name)
			if repo == nil {
				return nil, true
			}
			return repoObject(repo), true
		case "node":
			id, _ := args["id"].(string)
			return findByGlobalID(store, id), true
		case "__type":
			name, _ := args["name"].(string)
			def, ok := typeTable[name]
			if !ok {
				return nil, true
			}
			d := def
			return &GqlObject{Typename: "__Type", TypeName: name, HasTypeName: true, TypeDef: &d, HasTypeDef: true}, true
		}
		return nil, false
	case "Mutation":
		return mutationField(obj, field, args, ctx)
	case "User":
		switch field {
		case "login":
			return Viewer.Login, true
		case "id":
			return globalID("User", Viewer.ID), true
		case "name":
			return Viewer.Name, true
		case "email":
			return Viewer.Email, true
		}
		return nil, false
	case "Repository":
		repo := obj.Repo
		switch field {
		case "id":
			return globalID("Repository", repo.ID), true
		case "databaseId":
			return repo.ID, true
		case "name":
			return repo.Name, true
		case "nameWithOwner":
			return repo.Owner + "/" + repo.Name, true
		case "owner":
			return &GqlObject{Typename: "RepositoryOwner", Login: repo.Owner, HasLogin: true}, true
		case "defaultBranchRef":
			return &GqlObject{Typename: "Ref", RefName: repo.DefaultBranch, HasRefName: true}, true
		case "viewerPermission":
			return "ADMIN", true
		case "hasIssuesEnabled":
			return true, true
		case "description":
			return repo.Description, true
		case "isPrivate":
			return repo.Private, true
		case "createdAt":
			return repo.CreatedAt, true
		case "updatedAt":
			return repo.UpdatedAt, true
		case "issue":
			n, _ := asInt(args["number"])
			if issue := repo.Issues[n]; issue != nil {
				return issueObject(repo, issue), true
			}
			return nil, true
		case "issues":
			return issuesConnection(repo, args), true
		case "pullRequest":
			n, _ := asInt(args["number"])
			if pull := repo.Pulls[n]; pull != nil {
				return pullObject(repo, pull), true
			}
			return nil, true
		case "pullRequests":
			return pullsConnection(repo, args), true
		}
		return nil, false
	case "RepositoryOwner":
		switch field {
		case "login":
			return obj.Login, true
		case "id":
			return globalID("User", Viewer.ID), true
		}
		return nil, false
	case "Ref":
		switch field {
		case "name":
			return obj.RefName, true
		case "id":
			return globalID("Ref", obj.RefName), true
		}
		return nil, false
	case "Issue":
		issue := obj.Issue
		repo := obj.Repo
		switch field {
		case "id":
			return issue.ID, true
		case "number":
			return issue.Number, true
		case "title":
			return issue.Title, true
		case "body":
			return issue.Body, true
		case "state":
			return strings.ToUpper(issue.State), true
		case "createdAt":
			return issue.CreatedAt, true
		case "updatedAt":
			return issue.UpdatedAt, true
		case "repository":
			return repoObject(repo), true
		case "author":
			return &GqlObject{Typename: "User"}, true
		}
		return nil, false
	case "IssueConnection", "PullRequestConnection", "PullRequestFileConnection":
		switch field {
		case "nodes":
			return obj.Nodes, true
		case "edges":
			edges := make([]*GqlObject, 0, len(obj.Nodes))
			for _, n := range obj.Nodes {
				edges = append(edges, &GqlObject{Typename: "IssueEdge", Node: n, Cursor: "", HasCursor: true})
			}
			return edges, true
		case "pageInfo":
			return &GqlObject{Typename: "PageInfo", PageInfo: obj.PageInfo}, true
		case "totalCount":
			return len(obj.Nodes), true
		}
		return nil, false
	case "IssueEdge", "PullRequestEdge", "CommentEdge":
		switch field {
		case "node":
			return obj.Node, true
		case "cursor":
			return obj.Cursor, true
		}
		return nil, false
	case "PullRequest":
		pull := obj.Pull
		repo := obj.Repo
		switch field {
		case "id":
			return pull.ID, true
		case "number":
			return pull.Number, true
		case "title":
			return pull.Title, true
		case "body":
			return pull.Body, true
		case "state":
			if pull.Merged {
				return "MERGED", true
			}
			return strings.ToUpper(pull.State), true
		case "headRefName":
			return pull.HeadRef, true
		case "baseRefName":
			return pull.BaseRef, true
		case "headRef":
			return &GqlObject{Typename: "Ref", RefName: pull.HeadRef, HasRefName: true}, true
		case "baseRef":
			return &GqlObject{Typename: "Ref", RefName: pull.BaseRef, HasRefName: true}, true
		case "mergeable":
			return pull.Mergeable, true
		case "merged":
			return pull.Merged, true
		case "mergeCommit":
			if pull.HasMergeCommitSha {
				return &GqlObject{Typename: "Commit", CommitSha: pull.MergeCommitSha}, true
			}
			return nil, true
		case "createdAt":
			return pull.CreatedAt, true
		case "updatedAt":
			return pull.UpdatedAt, true
		case "repository":
			return repoObject(repo), true
		case "author":
			return &GqlObject{Typename: "User"}, true
		case "files":
			first := 10
			if n, ok := asInt(args["first"]); ok {
				first = n
			}
			files := pull.Files
			if len(files) > first {
				files = files[:first]
			}
			nodes := make([]*GqlObject, 0, len(files))
			for _, f := range files {
				nodes = append(nodes, &GqlObject{Typename: "PullRequestFile", File: f})
			}
			return &GqlObject{
				Typename: "PullRequestFileConnection",
				Nodes:    nodes,
				PageInfo: map[string]interface{}{"hasNextPage": false, "hasPreviousPage": false, "startCursor": nil, "endCursor": nil},
			}, true
		}
		return nil, false
	case "PullRequestFile":
		file := obj.File
		switch field {
		case "path":
			return file.Filename, true
		case "additions":
			return file.Additions, true
		case "deletions":
			return file.Deletions, true
		case "changeType":
			return strings.ToUpper(file.Status), true
		}
		return nil, false
	case "PageInfo":
		switch field {
		case "hasNextPage":
			return obj.PageInfo["hasNextPage"], true
		case "hasPreviousPage":
			return obj.PageInfo["hasPreviousPage"], true
		case "startCursor":
			return obj.PageInfo["startCursor"], true
		case "endCursor":
			return obj.PageInfo["endCursor"], true
		}
		return nil, false
	case "Comment":
		switch field {
		case "id":
			return obj.CommentID, true
		case "body":
			return obj.CommentBody, true
		case "author":
			return &GqlObject{Typename: "User"}, true
		case "createdAt":
			return obj.CommentAt, true
		case "updatedAt":
			return obj.CommentAt, true
		}
		return nil, false
	case "__Type":
		def := obj.TypeDef
		switch field {
		case "name":
			return obj.TypeName, true
		case "kind":
			return def.Kind, true
		case "description":
			return nil, true
		case "fields":
			if def.Kind != "OBJECT" {
				return nil, true
			}
			var out []*GqlObject
			for _, name := range def.Fields {
				out = append(out, &GqlObject{Typename: "__Field", FieldName: name})
			}
			return out, true
		case "enumValues":
			if def.Kind != "ENUM" {
				return nil, true
			}
			var out []*GqlObject
			for _, name := range def.Values {
				out = append(out, &GqlObject{Typename: "__EnumValue", EnumName: name})
			}
			return out, true
		}
		return nil, false
	case "__Field":
		switch field {
		case "name":
			return obj.FieldName, true
		case "description":
			return nil, true
		case "type":
			empty := TypeDef{Kind: "OBJECT", Fields: []string{}}
			return &GqlObject{Typename: "__Type", TypeName: "String", HasTypeName: true, TypeDef: &empty, HasTypeDef: true}, true
		}
		return nil, false
	case "__EnumValue":
		switch field {
		case "name":
			return obj.EnumName, true
		case "description":
			return nil, true
		}
		return nil, false
	case "CreateIssuePayload", "CloseIssuePayload":
		if field == "issue" {
			return obj.Node, true
		}
		return nil, false
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if field == "pullRequest" {
			return obj.Node, true
		}
		return nil, false
	case "AddCommentPayload":
		if field == "commentEdge" {
			return obj.Node, true
		}
		return nil, false
	}
	return nil, false
}

func mutationField(obj *GqlObject, field string, args map[string]interface{}, ctx *gqlCtx) (interface{}, bool) {
	store := ctx.store
	input, _ := args["input"].(map[string]interface{})
	if input == nil {
		input = map[string]interface{}{}
	}
	strArg := func(key string) string {
		s, _ := input[key].(string)
		return s
	}
	switch field {
	case "createIssue":
		target := findByGlobalID(store, strArg("repositoryId"))
		if target == nil || target.Typename != "Repository" {
			return &GqlObject{Typename: "CreateIssuePayload", Node: nil}, true
		}
		repo := target.Repo
		issue := store.CreateIssue(repo, strArg("title"), strArg("body"), nil, Viewer.Login)
		return &GqlObject{Typename: "CreateIssuePayload", Node: issueObject(repo, issue)}, true
	case "closeIssue":
		target := findByGlobalID(store, strArg("issueId"))
		if target == nil || target.Typename != "Issue" {
			return &GqlObject{Typename: "CloseIssuePayload", Node: nil}, true
		}
		issue := target.Issue
		issue.State = "closed"
		issue.UpdatedAt = store.NowIso()
		return &GqlObject{Typename: "CloseIssuePayload", Node: target}, true
	case "createPullRequest":
		target := findByGlobalID(store, strArg("repositoryId"))
		if target == nil || target.Typename != "Repository" {
			return &GqlObject{Typename: "CreatePullRequestPayload", Node: nil}, true
		}
		repo := target.Repo
		base := strArg("baseRefName")
		if base == "" {
			base = repo.DefaultBranch
		}
		pull := store.CreatePull(repo, CreatePullInput{
			Title: strArg("title"), Body: strArg("body"),
			Head: strArg("headRefName"), Base: base, User: Viewer.Login,
		})
		var node *GqlObject
		if pull != nil {
			node = pullObject(repo, pull)
		}
		return &GqlObject{Typename: "CreatePullRequestPayload", Node: node}, true
	case "mergePullRequest":
		target := findByGlobalID(store, strArg("pullRequestId"))
		if target == nil || target.Typename != "PullRequest" {
			return &GqlObject{Typename: "MergePullRequestPayload", Node: nil}, true
		}
		repo := target.Repo
		pull := target.Pull
		if pull.State == "open" {
			store.RefreshPullHead(repo, pull)
			if pull.Mergeable != "CONFLICTING" {
				author := Viewer.Name + " <" + Viewer.Email + ">"
				sha := store.SquashMerge(repo, pull, "Merge pull request #"+strconv.Itoa(pull.Number)+" from "+repo.Owner+"/"+pull.HeadRef, author)
				pull.Merged = true
				pull.MergedAt = store.NowIso()
				pull.HasMergedAt = true
				pull.MergeCommitSha = sha
				pull.HasMergeCommitSha = true
				pull.State = "closed"
				pull.UpdatedAt = store.NowIso()
			}
		}
		return &GqlObject{Typename: "MergePullRequestPayload", Node: target}, true
	case "addComment":
		target := findByGlobalID(store, strArg("subjectId"))
		if target == nil || target.Typename != "Issue" {
			return &GqlObject{Typename: "AddCommentPayload", Node: nil}, true
		}
		issue := target.Issue
		repo := target.Repo
		comment := &IssueComment{
			ID:        repo.NextCommentID,
			Body:      strArg("body"),
			User:      Viewer.Login,
			CreatedAt: store.NowIso(),
			UpdatedAt: store.NowIso(),
		}
		repo.NextCommentID++
		issue.Comments = append(issue.Comments, comment)
		node := &GqlObject{
			Typename:    "Comment",
			CommentID:   globalID("IssueComment", repo.ID, issue.Number, comment.ID),
			CommentBody: comment.Body,
			CommentAt:   comment.CreatedAt,
		}
		return &GqlObject{Typename: "AddCommentPayload", Node: &GqlObject{Typename: "CommentEdge", Node: node, Cursor: "", HasCursor: true}}, true
	}
	return nil, false
}

func resolveSelections(obj *GqlObject, selections []Selection, variables map[string]interface{}, ctx *gqlCtx, errs *[]gqlError) map[string]interface{} {
	out := map[string]interface{}{}
	for _, sel := range selections {
		if sel.IsFragment {
			if sel.Fragment.HasCondition && sel.Fragment.TypeCondition != obj.Typename {
				continue
			}
			for k, v := range resolveSelections(obj, sel.Fragment.Selections, variables, ctx, errs) {
				out[k] = v
			}
			continue
		}
		field := sel.Field
		if def, ok := typeTable[obj.Typename]; ok && def.Kind == "OBJECT" && field.Name != "__typename" {
			found := false
			for _, f := range def.Fields {
				if f == field.Name {
					found = true
					break
				}
			}
			if !found {
				*errs = append(*errs, gqlError{Message: "Cannot query field \"" + field.Name + "\" on type \"" + obj.Typename + "\"."})
				continue
			}
		}
		if field.Name == "__typename" {
			out[field.Alias] = obj.Typename
			continue
		}
		args := resolveArgs(field.Args, variables)
		value, ok := fieldValue(obj, field.Name, args, ctx)
		if !ok {
			*errs = append(*errs, gqlError{Message: "Cannot query field \"" + field.Name + "\" on type \"" + obj.Typename + "\"."})
			continue
		}
		out[field.Alias] = resolveValueNode(value, field.Selections, variables, ctx, errs)
	}
	return out
}

func resolveValueNode(value interface{}, selections []Selection, variables map[string]interface{}, ctx *gqlCtx, errs *[]gqlError) interface{} {
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case []*GqlObject:
		out := make([]interface{}, len(v))
		for i, item := range v {
			out[i] = resolveValueNode(item, selections, variables, ctx, errs)
		}
		return out
	case *GqlObject:
		if len(selections) == 0 {
			return map[string]interface{}{}
		}
		return resolveSelections(v, selections, variables, ctx, errs)
	}
	return value
}

type gqlExecResult struct {
	Data   interface{}         `json:"data"`
	Errors []map[string]string `json:"errors,omitempty"`
}

func executeGraphQL(store *Store, body interface{}) (int, interface{}) {
	ctx := &gqlCtx{store: store}
	var requests []interface{}
	if arr, ok := body.([]interface{}); ok {
		requests = arr
	} else {
		requests = []interface{}{body}
	}
	results := make([]interface{}, 0, len(requests))
	for _, req := range requests {
		results = append(results, executeOne(ctx, req))
	}
	if _, isArr := body.([]interface{}); isArr {
		return 200, results
	}
	return 200, results[0]
}

func executeOne(ctx *gqlCtx, req interface{}) gqlExecResult {
	input, _ := req.(map[string]interface{})
	if input == nil {
		input = map[string]interface{}{}
	}
	query, _ := input["query"].(string)
	if query == "" {
		// typeof input.query !== 'string' — but a missing query is also not a string.
		if _, ok := input["query"]; !ok {
			return gqlExecResult{Data: nil, Errors: []map[string]string{{"message": "GraphQL query must be a string."}}}
		}
		return gqlExecResult{Data: nil, Errors: []map[string]string{{"message": "GraphQL query must be a string."}}}
	}
	tokens, err := tokenize(query)
	if err != nil {
		return gqlExecResult{Data: nil, Errors: []map[string]string{{"message": err.Error()}}}
	}
	p := &parser{tokens: tokens}
	operations, err := p.parseDocument()
	if err != nil {
		return gqlExecResult{Data: nil, Errors: []map[string]string{{"message": err.Error()}}}
	}
	variables, _ := input["variables"].(map[string]interface{})
	if variables == nil {
		variables = map[string]interface{}{}
	}
	var op *Operation
	if name, ok := input["operationName"].(string); ok {
		for i := range operations {
			if operations[i].HasName && operations[i].Name == name {
				op = &operations[i]
				break
			}
		}
	} else if len(operations) >= 1 {
		op = &operations[0]
	}
	if op == nil {
		return gqlExecResult{Data: nil, Errors: []map[string]string{{"message": "No operation to execute."}}}
	}
	rootType := "Query"
	if op.Kind == "mutation" {
		rootType = "Mutation"
	}
	root := &GqlObject{Typename: rootType}
	var errs []gqlError
	data := resolveSelections(root, op.Selections, variables, ctx, &errs)
	if len(errs) > 0 {
		errMaps := make([]map[string]string, 0, len(errs))
		for _, e := range errs {
			errMaps = append(errMaps, map[string]string{"message": e.Message})
		}
		return gqlExecResult{Data: nil, Errors: errMaps}
	}
	return gqlExecResult{Data: data}
}
