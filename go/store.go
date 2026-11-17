// In-memory store for the forge. Everything is deterministic: ids and numbers come
// from counters, timestamps from a fixed epoch plus a per-event counter, so two runs
// of the same trial leave the same state. No wall clock, no randomness.
// Faithful port of lib/store.ts.
//
// Concurrency: the HTTP layer holds the Store's RWMutex for the whole request
// (read lock for read-only routes, write lock for mutating ones), so this file
// needs no locking of its own. Map iteration order is never relied on: every
// enumeration that reaches the wire is sorted or insertion-ordered.
package main

import (
	"encoding/base64"
	"sort"
	"strconv"
	"strings"
	"time"
)

var Viewer = struct {
	ID    int
	Login string
	Name  string
	Email string
}{ID: 1000, Login: "adaiversen", Name: "ada iversen", Email: "adaiversen@example.invalid"}

var Stranger = struct {
	ID    int
	Login string
	Name  string
	Email string
}{ID: 1002, Login: "olgabauer", Name: "olga bauer", Email: "olgabauer@example.invalid"}

const ScenarioEpochSec = 1735689600 // 2025-01-01T00:00:00Z

type Label struct {
	Name        string
	Color       string
	Description string
}

type IssueComment struct {
	ID        int
	Body      string
	User      string
	CreatedAt string
	UpdatedAt string
}

type Issue struct {
	ID         string // opaque global id
	DatabaseID int
	Number     int
	Title      string
	Body       string
	State      string // 'open' | 'closed'
	Labels     []string
	User       string
	CreatedAt  string
	UpdatedAt  string
	Comments   []*IssueComment
}

type PullFile struct {
	Filename  string
	Status    string // 'added' | 'modified' | 'removed'
	Additions int
	Deletions int
	Changes   int
	Patch     string
	HasPatch  bool
}

type Pull struct {
	ID                string
	DatabaseID        int
	Number            int
	Title             string
	Body              string
	State             string // 'open' | 'closed'
	HeadRef           string
	HeadSha           string
	HeadRepo          string
	BaseRef           string
	BaseSha           string
	BaseRepo          string
	Merged            bool
	MergedAt          string
	HasMergedAt       bool
	MergeCommitSha    string
	HasMergeCommitSha bool
	Mergeable         string // 'MERGEABLE' | 'CONFLICTING' | 'UNKNOWN'
	User              string
	CreatedAt         string
	UpdatedAt         string
	Files             []*PullFile
	HeadTree          string
	BaseTree          string
	ChangedPaths      []string
}

type Repo struct {
	ID            int
	Name          string
	Owner         string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string
	Objects       map[string]*GitObject
	Refs          map[string]string
	Issues        map[int]*Issue
	NextIssue     int
	NextIssueID   int
	Labels        map[string]*Label
	LabelOrder    []string
	Pulls         map[int]*Pull
	NextPull      int
	NextPullID    int
	NextCommentID int
}

// All refs of a repo, sorted by name.
type RefEntry struct {
	Name string
	OID  string
}

func listRefs(repo *Repo) []RefEntry {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]RefEntry, 0, len(names))
	for _, name := range names {
		out = append(out, RefEntry{Name: name, OID: repo.Refs[name]})
	}
	return out
}

func globalID(kind string, parts ...interface{}) string {
	var b strings.Builder
	b.WriteString(kind)
	b.WriteString(":")
	for i, p := range parts {
		if i > 0 {
			b.WriteString(":")
		}
		switch v := p.(type) {
		case string:
			b.WriteString(v)
		case int:
			b.WriteString(strconv.Itoa(v))
		}
	}
	return base64.RawURLEncoding.EncodeToString([]byte(b.String()))
}

type DecodedGlobalID struct {
	Kind  string
	Parts []string
}

func decodeGlobalID(id string) *DecodedGlobalID {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(id)
		if err != nil {
			return nil
		}
	}
	text := string(raw)
	idx := strings.Index(text, ":")
	if idx == -1 {
		return nil
	}
	return &DecodedGlobalID{Kind: text[:idx], Parts: strings.Split(text[idx+1:], ":")}
}

type Store struct {
	Repos         map[string]*Repo // key `${owner}/${name}`
	nextRepoID    int
	nextGlobalSeq int
	tick          int // deterministic clock: seconds since the scenario epoch
}

func NewStore() *Store {
	return &Store{Repos: map[string]*Repo{}, nextRepoID: 5000, nextGlobalSeq: 1}
}

// A timestamp string, one second later on every call — deterministic across runs.
func (s *Store) NowIso() string {
	epoch := ScenarioEpochSec + s.tick
	s.tick++
	return time.Unix(int64(epoch), 0).UTC().Format("2006-01-02T15:04:05") + ".000Z"
}

// A commit timestamp (seconds), one second later on every call.
func (s *Store) NowSec() int64 {
	epoch := ScenarioEpochSec + s.tick
	s.tick++
	return int64(epoch)
}

// The trial's fixed now when the harness sets it, else the scenario epoch.
func TrialEpochSec() int64 {
	raw := getEnv("MAKEFUN_TRIAL_NOW")
	if raw != "" {
		if ms, err := strconv.ParseFloat(raw, 64); err == nil && ms > 0 && ms == ms {
			return int64(ms) / 1000
		}
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t.Unix()
		}
	}
	return ScenarioEpochSec
}

func (s *Store) Key(owner, name string) string { return owner + "/" + name }

func (s *Store) GetRepo(owner, name string) *Repo {
	return s.Repos[s.Key(owner, name)]
}

func (s *Store) ListRepos(owner string) []*Repo {
	var out []*Repo
	for _, repo := range s.Repos {
		if repo.Owner == owner {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

type CreateRepoOptions struct {
	Description   string
	Private       bool
	DefaultBranch string
}

func (s *Store) CreateRepo(owner, name string, options CreateRepoOptions) *Repo {
	if _, exists := s.Repos[s.Key(owner, name)]; exists {
		return nil
	}
	now := s.NowIso()
	defBranch := options.DefaultBranch
	if defBranch == "" {
		defBranch = "main"
	}
	repo := &Repo{
		ID:            s.nextRepoID,
		Name:          name,
		Owner:         owner,
		Description:   options.Description,
		Private:       options.Private,
		DefaultBranch: defBranch,
		CreatedAt:     now,
		UpdatedAt:     now,
		Objects:       map[string]*GitObject{},
		Refs:          map[string]string{},
		Issues:        map[int]*Issue{},
		NextIssue:     1,
		NextIssueID:   1,
		Labels:        map[string]*Label{},
		Pulls:         map[int]*Pull{},
		NextPull:      1,
		NextPullID:    1,
		NextCommentID: 1,
	}
	s.nextRepoID++
	s.Repos[s.Key(owner, name)] = repo
	return repo
}

func (s *Store) DeleteRepo(owner, name string) bool {
	key := s.Key(owner, name)
	if _, ok := s.Repos[key]; !ok {
		return false
	}
	delete(s.Repos, key)
	return true
}

// -------------------------------------------------------------------------------------------
// git helpers on a repo
// -------------------------------------------------------------------------------------------

func (s *Store) RefTip(repo *Repo, ref string) (string, bool) {
	oid, ok := repo.Refs[ref]
	return oid, ok
}

func (s *Store) SetRef(repo *Repo, ref, oid string) { repo.Refs[ref] = oid }

func (s *Store) DeleteRef(repo *Repo, ref string) bool {
	if _, ok := repo.Refs[ref]; !ok {
		return false
	}
	delete(repo.Refs, ref)
	return true
}

// The default branch's tip, or "" when the repo is unborn.
func (s *Store) HeadTip(repo *Repo) string {
	return repo.Refs["refs/heads/"+repo.DefaultBranch]
}

// The tree oid of a commit, or "".
func (s *Store) CommitTree(repo *Repo, commitOID string) string {
	obj := repo.Objects[commitOID]
	if obj == nil || obj.Type != ObjCommit {
		return ""
	}
	return parseCommit(obj.Data).Tree
}

func (s *Store) ListRefs(repo *Repo) []RefEntry { return listRefs(repo) }

// Objects reachable from the given tips that the caller does not already have.
func (s *Store) MissingObjects(repo *Repo, tips []string, have map[string]bool) []*GitObject {
	reachable := collectReachable(tips, func(oid string) *GitObject { return repo.Objects[oid] })
	oids := make([]string, 0, len(reachable))
	for oid := range reachable {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	var out []*GitObject
	for _, oid := range oids {
		if have[oid] {
			continue
		}
		if obj := repo.Objects[oid]; obj != nil {
			out = append(out, obj)
		}
	}
	return out
}

// Create a commit on a branch from a tree mutation: set `path` to `content` (or delete
// when content is nil), parented on the branch tip (or none when unborn).
func (s *Store) CommitFile(repo *Repo, branch, path string, content []byte, hasContent bool, message, author string) (commit string, blob string, hasBlob bool, tree string) {
	ref := "refs/heads/" + branch
	tip, hasTip := repo.Refs[ref]
	var baseTree string
	var hasBase bool
	if hasTip {
		baseTree = s.CommitTree(repo, tip)
		hasBase = baseTree != ""
	}
	var blobOID string
	var newTree string
	if !hasContent {
		newTree = treeSet(repo.Objects, baseTree, hasBase, path, "", "100644")
	} else {
		blobOID = storeObject(repo.Objects, ObjBlob, content)
		newTree = treeSet(repo.Objects, baseTree, hasBase, path, blobOID, "100644")
	}
	ts := s.NowSec()
	var parents []string
	if hasTip {
		parents = []string{tip}
	}
	commitData := buildCommit(CommitInput{
		Tree:      newTree,
		Parents:   parents,
		Message:   message,
		Author:    author,
		Committer: author,
		Timestamp: ts,
	})
	commitOID := storeObject(repo.Objects, ObjCommit, commitData)
	repo.Refs[ref] = commitOID
	return commitOID, blobOID, hasContent, newTree
}

// The file at `path` on a ref's tip: {blob oid, content} or nil.
func (s *Store) ReadFile(repo *Repo, ref, path string) (oid, mode string, data []byte, ok bool) {
	tip, exists := repo.Refs[ref]
	if !exists {
		return "", "", nil, false
	}
	tree := s.CommitTree(repo, tip)
	if tree == "" {
		return "", "", nil, false
	}
	foundOID, foundMode, found := treeLookup(repo.Objects, tree, path)
	if !found {
		return "", "", nil, false
	}
	obj := repo.Objects[foundOID]
	if obj == nil || obj.Type != ObjBlob {
		return "", "", nil, false
	}
	return foundOID, foundMode, obj.Data, true
}

// The flat file list of a ref's tip tree.
func (s *Store) ListFiles(repo *Repo, ref string) []FlatFile {
	tip, exists := repo.Refs[ref]
	if !exists {
		return nil
	}
	tree := s.CommitTree(repo, tip)
	if tree == "" {
		return nil
	}
	return treeFlatten(repo.Objects, tree)
}

// Diff two trees: paths added/modified/removed with per-file line counts.
func (s *Store) DiffTrees(repo *Repo, baseTree, headTree string) []*PullFile {
	baseList := treeFlatten(repo.Objects, baseTree)
	headList := treeFlatten(repo.Objects, headTree)
	base := map[string]FlatFile{}
	for _, f := range baseList {
		base[f.Path] = f
	}
	head := map[string]FlatFile{}
	for _, f := range headList {
		head[f.Path] = f
	}
	var files []*PullFile
	for _, h := range headList {
		b, exists := base[h.Path]
		if !exists {
			text := ""
			if obj := repo.Objects[h.OID]; obj != nil {
				text = string(obj.Data)
			}
			n := countLines(text)
			files = append(files, &PullFile{Filename: h.Path, Status: "added", Additions: n, Deletions: 0, Changes: n})
		} else if b.OID != h.OID {
			oldText := ""
			if obj := repo.Objects[b.OID]; obj != nil {
				oldText = string(obj.Data)
			}
			newText := ""
			if obj := repo.Objects[h.OID]; obj != nil {
				newText = string(obj.Data)
			}
			add := countLines(newText)
			del := countLines(oldText)
			files = append(files, &PullFile{Filename: h.Path, Status: "modified", Additions: add, Deletions: del, Changes: add + del})
		}
	}
	for _, b := range baseList {
		if _, exists := head[b.Path]; !exists {
			text := ""
			if obj := repo.Objects[b.OID]; obj != nil {
				text = string(obj.Data)
			}
			n := countLines(text)
			files = append(files, &PullFile{Filename: b.Path, Status: "removed", Additions: 0, Deletions: n, Changes: n})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Filename < files[j].Filename })
	return files
}

// True when the head branch can merge cleanly onto base (no same-file conflict).
func (s *Store) Mergeable(repo *Repo, pull *Pull) bool {
	baseFiles := map[string]string{}
	for _, f := range treeFlatten(repo.Objects, pull.BaseTree) {
		baseFiles[f.Path] = f.OID
	}
	headFiles := map[string]string{}
	for _, f := range treeFlatten(repo.Objects, pull.HeadTree) {
		headFiles[f.Path] = f.OID
	}
	baseTip, ok := repo.Refs["refs/heads/"+pull.BaseRef]
	if !ok {
		return false
	}
	baseTreeNow := s.CommitTree(repo, baseTip)
	if baseTreeNow == "" {
		return false
	}
	nowFiles := map[string]string{}
	for _, f := range treeFlatten(repo.Objects, baseTreeNow) {
		nowFiles[f.Path] = f.OID
	}
	for path, headOID := range headFiles {
		baseOID, exists := baseFiles[path]
		if !exists {
			// Added on head: conflicts only if base also added it differently.
			if nowOID, ok := nowFiles[path]; ok && nowOID != headOID {
				return false
			}
		} else if headOID != baseOID {
			if nowOID, ok := nowFiles[path]; ok && nowOID != baseOID {
				return false
			}
		}
	}
	for path := range baseFiles {
		if _, ok := headFiles[path]; !ok {
			if nowOID, ok := nowFiles[path]; ok && nowOID != baseFiles[path] {
				return false
			}
		}
	}
	return true
}

// Squash-merge a pull: one commit on the base branch whose tree is the head tree
// merged over the base tree (head wins per path), parented on the base tip.
func (s *Store) SquashMerge(repo *Repo, pull *Pull, message, author string) string {
	baseTip := repo.Refs["refs/heads/"+pull.BaseRef]
	baseTree := s.CommitTree(repo, baseTip)
	files := s.DiffTrees(repo, pull.BaseTree, pull.HeadTree)
	tree := baseTree
	for _, f := range files {
		if f.Status == "removed" {
			tree = treeSet(repo.Objects, tree, true, f.Filename, "", "100644")
		} else {
			if headFileOID, headFileMode, ok := treeLookup(repo.Objects, pull.HeadTree, f.Filename); ok {
				tree = treeSet(repo.Objects, tree, true, f.Filename, headFileOID, headFileMode)
			}
		}
	}
	ts := s.NowSec()
	commit := storeObject(repo.Objects, ObjCommit, buildCommit(CommitInput{
		Tree:      tree,
		Parents:   []string{baseTip},
		Message:   message,
		Author:    author,
		Committer: author,
		Timestamp: ts,
	}))
	repo.Refs["refs/heads/"+pull.BaseRef] = commit
	return commit
}

// Re-derive a pull's head sha/tree after its branch moved.
func (s *Store) RefreshPullHead(repo *Repo, pull *Pull) {
	tip, ok := repo.Refs["refs/heads/"+pull.HeadRef]
	if !ok {
		return
	}
	pull.HeadSha = tip
	if tree := s.CommitTree(repo, tip); tree != "" {
		pull.HeadTree = tree
		pull.Files = s.DiffTrees(repo, pull.BaseTree, tree)
		pull.ChangedPaths = nil
		for _, f := range pull.Files {
			pull.ChangedPaths = append(pull.ChangedPaths, f.Filename)
		}
	}
	if s.Mergeable(repo, pull) {
		pull.Mergeable = "MERGEABLE"
	} else {
		pull.Mergeable = "CONFLICTING"
	}
}

// -------------------------------------------------------------------------------------------
// issues / pulls
// -------------------------------------------------------------------------------------------

func (s *Store) CreateIssue(repo *Repo, title, body string, labels []string, user string) *Issue {
	number := repo.NextIssue
	repo.NextIssue++
	db := repo.NextIssueID
	repo.NextIssueID++
	now := s.NowIso()
	issue := &Issue{
		ID:         globalID("Issue", repo.ID, number),
		DatabaseID: db,
		Number:     number,
		Title:      title,
		Body:       body,
		State:      "open",
		Labels:     append([]string(nil), labels...),
		User:       user,
		CreatedAt:  now,
		UpdatedAt:  now,
		Comments:   nil,
	}
	repo.Issues[number] = issue
	return issue
}

type CreatePullInput struct {
	Title string
	Body  string
	Head  string
	Base  string
	User  string
}

func (s *Store) CreatePull(repo *Repo, input CreatePullInput) *Pull {
	headTip, ok1 := repo.Refs["refs/heads/"+input.Head]
	baseTip, ok2 := repo.Refs["refs/heads/"+input.Base]
	if !ok1 || !ok2 {
		return nil
	}
	headTree := s.CommitTree(repo, headTip)
	baseTree := s.CommitTree(repo, baseTip)
	files := s.DiffTrees(repo, baseTree, headTree)
	number := repo.NextPull
	repo.NextPull++
	db := repo.NextPullID
	repo.NextPullID++
	now := s.NowIso()
	pull := &Pull{
		ID:         globalID("PullRequest", repo.ID, number),
		DatabaseID: db,
		Number:     number,
		Title:      input.Title,
		Body:       input.Body,
		State:      "open",
		HeadRef:    input.Head,
		HeadSha:    headTip,
		HeadRepo:   repo.Name,
		BaseRef:    input.Base,
		BaseSha:    baseTip,
		BaseRepo:   repo.Name,
		Merged:     false,
		Mergeable:  "UNKNOWN",
		User:       input.User,
		CreatedAt:  now,
		UpdatedAt:  now,
		Files:      files,
		HeadTree:   headTree,
		BaseTree:   baseTree,
	}
	for _, f := range files {
		pull.ChangedPaths = append(pull.ChangedPaths, f.Filename)
	}
	if s.Mergeable(repo, pull) {
		pull.Mergeable = "MERGEABLE"
	} else {
		pull.Mergeable = "CONFLICTING"
	}
	repo.Pulls[number] = pull
	return pull
}

func countLines(text string) int {
	if text == "" {
		return 0
	}
	n := strings.Count(text, "\n")
	if strings.HasSuffix(text, "\n") {
		return n
	}
	return n + 1
}
