// git smart HTTP: info/refs advertisement (v2 upload-pack, v0 upload/receive-pack),
// v2 ls-refs + fetch, v0 fetch, and receive-pack with report-status.
// Faithful port of lib/gitsmart.ts. Wire formats verified against sdk/conformance/git-client.ts.
package main

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

const agentName = "forge/1.0"

// The v2 upload-pack advertisement, built once at startup: it never depends on
// the repository (the same six pkt-lines for every request).
var v2UploadAdvertisement = bytes.Join([][]byte{
	pktStr("version 2\n"),
	pktStr("agent=" + agentName + "\n"),
	pktStr("ls-refs=unborn\n"),
	pktStr("fetch=shallow wait-for-done filter\n"),
	pktStr("object-format=sha1\n"),
	flushPkt,
}, nil)

type GitResponse struct {
	Status      int
	ContentType string
	Body        []byte
}

func gitOK(contentType string, body []byte) GitResponse {
	return GitResponse{Status: 200, ContentType: contentType, Body: body}
}

func gitNotFound() GitResponse {
	return GitResponse{Status: 404, ContentType: "text/plain", Body: []byte("repository not found\n")}
}

func gitBadRequest(message string) GitResponse {
	return GitResponse{Status: 400, ContentType: "text/plain", Body: []byte(message + "\n")}
}

// The first ref line (or the capabilities^{} line) carries these capabilities.
const v0UploadCaps = "multi_ack_detailed side-band-64k thin-pack ofs-delta shallow filter agent=forge/1.0 symref=HEAD:refs/heads/main"
const v0ReceiveCaps = "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0 symref=HEAD:refs/heads/main"

// Peeled commit for an annotated tag ref, else "".
func peeledOf(repo *Repo, refName, oid string) string {
	obj := repo.Objects[oid]
	if obj == nil || obj.Type != ObjTag {
		return ""
	}
	tag := parseTag(obj.Data)
	if tag.Type == "commit" {
		return tag.Object
	}
	return ""
}

func headTarget(repo *Repo) string {
	return "refs/heads/" + repo.DefaultBranch
}

// v0 ref lines: first line carries \0caps; annotated tags get a ^{} peeled line after.
func v0RefLines(repo *Repo, caps string) [][]byte {
	refs := listRefs(repo)
	var lines [][]byte
	if len(refs) == 0 {
		lines = append(lines, pktStr(ZeroOID+" capabilities^{}\x00"+caps+"\n"))
		return lines
	}
	for i, ref := range refs {
		first := i == 0
		line := ref.OID + " " + ref.Name
		if first {
			line += "\x00" + caps
		}
		lines = append(lines, pktStr(line+"\n"))
		if peeled := peeledOf(repo, ref.Name, ref.OID); peeled != "" {
			lines = append(lines, pktStr(peeled+" "+ref.Name+"^{}\n"))
		}
	}
	return lines
}

// -------------------------------------------------------------------------------------------
// info/refs
// -------------------------------------------------------------------------------------------

func handleInfoRefs(store *Store, owner, repoName, service, gitProtocol string) GitResponse {
	repo := store.GetRepo(owner, repoName)
	if repo == nil {
		return gitNotFound()
	}
	v2 := gitProtocol == "version=2"
	if service == "git-upload-pack" && v2 {
		return gitOK("application/x-git-upload-pack-advertisement", v2UploadAdvertisement)
	}
	if service == "git-upload-pack" {
		parts := [][]byte{pktStr("# service=git-upload-pack\n"), flushPkt}
		parts = append(parts, v0RefLines(repo, v0UploadCaps)...)
		parts = append(parts, flushPkt)
		return gitOK("application/x-git-upload-pack-advertisement", bytes.Join(parts, nil))
	}
	if service == "git-receive-pack" {
		parts := [][]byte{pktStr("# service=git-receive-pack\n"), flushPkt}
		parts = append(parts, v0RefLines(repo, v0ReceiveCaps)...)
		parts = append(parts, flushPkt)
		return gitOK("application/x-git-receive-pack-advertisement", bytes.Join(parts, nil))
	}
	return gitNotFound()
}

// -------------------------------------------------------------------------------------------
// fetch object computation
// -------------------------------------------------------------------------------------------

type FetchRequest struct {
	Wants          []string
	Haves          []string
	Shallow        []string
	Depth          int
	HasDepth       bool
	DeepenSince    int64
	HasDeepenSince bool
	DeepenNot      []string
}

type FetchPlan struct {
	Objects    []*GitObject
	Shallow    []string
	Unshallow  []string
	WantedRefs []RefEntry
}

// Walk from the wants, honouring depth / deepen-since / deepen-not, skipping anything
// the client already holds. The held set is reachable from the haves WITHOUT crossing a
// shallow boundary: a shallow commit's parents are not held, so a deepen can deliver them.
func planFetch(repo *Repo, req *FetchRequest) *FetchPlan {
	shallowSet := map[string]bool{}
	for _, o := range req.Shallow {
		shallowSet[o] = true
	}
	heldSet := map[string]bool{}
	addTreeToSet := func(set map[string]bool, treeOID string) {
		stack := []string{treeOID}
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if set[oid] {
				continue
			}
			obj := repo.Objects[oid]
			if obj == nil {
				continue
			}
			set[oid] = true
			if obj.Type == ObjTree {
				for _, e := range parseTree(obj.Data) {
					stack = append(stack, e.OID)
				}
			}
		}
	}
	// Held = reachable from the haves, stopping at shallow boundaries.
	{
		queue := append([]string(nil), req.Haves...)
		seen := map[string]bool{}
		for len(queue) > 0 {
			oid := queue[0]
			queue = queue[1:]
			if seen[oid] {
				continue
			}
			seen[oid] = true
			obj := repo.Objects[oid]
			if obj == nil {
				continue
			}
			if obj.Type == ObjTag {
				heldSet[oid] = true
				tag := parseTag(obj.Data)
				if !seen[tag.Object] {
					queue = append(queue, tag.Object)
				}
				continue
			}
			if obj.Type != ObjCommit {
				heldSet[oid] = true
				continue
			}
			heldSet[oid] = true
			info := parseCommit(obj.Data)
			addTreeToSet(heldSet, info.Tree)
			if shallowSet[oid] {
				continue // boundary: its parents are not held
			}
			for _, parent := range info.Parents {
				if !seen[parent] {
					queue = append(queue, parent)
				}
			}
		}
	}
	excluded := map[string]bool{}
	for _, ref := range req.DeepenNot {
		if tip, ok := repo.Refs[ref]; ok {
			for oid := range collectReachable([]string{tip}, func(o string) *GitObject { return repo.Objects[o] }) {
				excluded[oid] = true
			}
		}
	}
	sentCommits := map[string]bool{}
	sentOrder := []string{}
	objects := map[string]*GitObject{}
	// Add a commit's tree closure only (tree + blobs + subtrees) — never its ancestors,
	// and never anything the client already holds.
	addTreeClosure := func(treeOID string) {
		stack := []string{treeOID}
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if objects[oid] != nil || heldSet[oid] || excluded[oid] {
				continue
			}
			obj := repo.Objects[oid]
			if obj == nil {
				continue
			}
			objects[oid] = obj
			if obj.Type == ObjTree {
				for _, e := range parseTree(obj.Data) {
					stack = append(stack, e.OID)
				}
			}
		}
	}
	// BFS over commits, level = distance from the nearest want.
	type queueItem struct {
		oid   string
		level int
	}
	queue := make([]queueItem, 0, len(req.Wants))
	for _, oid := range req.Wants {
		queue = append(queue, queueItem{oid, 0})
	}
	seen := map[string]bool{}
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		oid, level := item.oid, item.level
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := repo.Objects[oid]
		if obj == nil {
			continue
		}
		if obj.Type == ObjTag {
			if !heldSet[oid] && !excluded[oid] {
				objects[oid] = obj
			}
			tag := parseTag(obj.Data)
			if !seen[tag.Object] {
				queue = append(queue, queueItem{tag.Object, level})
			}
			continue
		}
		if obj.Type != ObjCommit {
			if !heldSet[oid] && !excluded[oid] {
				objects[oid] = obj
			}
			continue
		}
		if excluded[oid] {
			continue
		}
		info := parseCommit(obj.Data)
		// deepen-since: an older commit is a boundary — neither it nor its ancestors are sent.
		if req.HasDeepenSince && info.Timestamp < req.DeepenSince {
			continue
		}
		withinDepth := !req.HasDepth || level < req.Depth
		if withinDepth && !heldSet[oid] {
			sentCommits[oid] = true
			sentOrder = append(sentOrder, oid)
			objects[oid] = obj
			addTreeClosure(info.Tree)
		}
		// Parents are worth visiting only while they could still be within the depth.
		if !req.HasDepth || level+1 < req.Depth {
			for _, parent := range info.Parents {
				if !seen[parent] {
					queue = append(queue, queueItem{parent, level + 1})
				}
			}
		}
	}
	// Boundary: a sent commit whose parent exists but is neither sent nor held.
	// Order matches the Node reference: the order commits were sent (BFS order).
	var shallow []string
	for _, oid := range sentOrder {
		obj := repo.Objects[oid]
		if obj == nil {
			continue
		}
		info := parseCommit(obj.Data)
		for _, parent := range info.Parents {
			if !sentCommits[parent] && !heldSet[parent] {
				if _, exists := repo.Objects[parent]; exists {
					shallow = append(shallow, oid)
					break
				}
			}
		}
	}
	// Unshallow: a requested boundary that is sent and no longer a boundary.
	shallowBoundary := map[string]bool{}
	for _, oid := range shallow {
		shallowBoundary[oid] = true
	}
	var unshallow []string
	for _, oid := range req.Shallow {
		if sentCommits[oid] && !shallowBoundary[oid] {
			unshallow = append(unshallow, oid)
		}
	}
	wantSet := map[string]bool{}
	for _, oid := range req.Wants {
		wantSet[oid] = true
	}
	var wantedRefs []RefEntry
	for _, ref := range listRefs(repo) {
		if wantSet[ref.OID] {
			wantedRefs = append(wantedRefs, ref)
		}
	}
	// Deterministic pack order: sort by oid.
	sorted := make([]*GitObject, 0, len(objects))
	for _, obj := range objects {
		sorted = append(sorted, obj)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].OID < sorted[j].OID })
	return &FetchPlan{Objects: sorted, Shallow: shallow, Unshallow: unshallow, WantedRefs: wantedRefs}
}

func ackLines(repo *Repo, req *FetchRequest) []string {
	var lines []string
	any := false
	for _, have := range req.Haves {
		if _, ok := repo.Objects[have]; ok {
			lines = append(lines, "ACK "+have+"\n")
			any = true
		}
	}
	if any {
		lines = append(lines, "ready\n")
	} else {
		lines = append(lines, "NAK\n")
	}
	return lines
}

// -------------------------------------------------------------------------------------------
// upload-pack (ls-refs / fetch, v2 and v0)
// -------------------------------------------------------------------------------------------

func handleUploadPack(store *Store, owner, repoName string, body []byte, gitProtocol string) GitResponse {
	repo := store.GetRepo(owner, repoName)
	if repo == nil {
		return gitNotFound()
	}
	frames, _ := splitFramesAndRest(body)
	// Data lines plus delim markers (delim frames are not data frames).
	var items []interface{} // string | 'delim'
	for _, f := range frames {
		if f.Type == FrameData {
			items = append(items, string(f.Data))
		} else if f.Type == FrameDelim {
			items = append(items, "delim")
		}
	}
	var lines []string
	for _, it := range items {
		if s, ok := it.(string); ok {
			lines = append(lines, s)
		}
	}
	command := ""
	if len(lines) > 0 {
		command = strings.TrimSuffix(lines[0], "\n")
	}
	if strings.HasPrefix(command, "command=ls-refs") {
		return handleLsRefs(repo, items)
	}
	if strings.HasPrefix(command, "command=fetch") {
		return handleFetchV2(repo, items)
	}
	// No command= line: a v0 fetch (want/have/done) or garbage.
	for _, l := range lines {
		if strings.HasPrefix(l, "want ") {
			return handleFetchV0(repo, frames, body)
		}
	}
	return gitBadRequest("unknown upload-pack request")
}

func handleLsRefs(repo *Repo, items []interface{}) GitResponse {
	// After the delim: peel / symrefs / unborn / ref-prefix <p> arguments.
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	pastDelim := false
	for _, item := range items {
		if s, ok := item.(string); ok && s == "delim" {
			pastDelim = true
			continue
		}
		text, ok := item.(string)
		if !ok {
			continue
		}
		text = strings.TrimSuffix(text, "\n")
		if text == "command=ls-refs" || strings.HasPrefix(text, "agent=") || strings.HasPrefix(text, "object-format=") {
			continue
		}
		if !pastDelim {
			continue
		}
		switch {
		case text == "peel":
			peel = true
		case text == "symrefs":
			symrefs = true
		case text == "unborn":
			unborn = true
		case strings.HasPrefix(text, "ref-prefix "):
			prefixes = append(prefixes, text[len("ref-prefix "):])
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var out [][]byte
	headRef := headTarget(repo)
	headTip, hasHead := repo.Refs[headRef]
	if symrefs && keep("HEAD") {
		if hasHead {
			out = append(out, pktStr(headTip+" HEAD symref-target:"+headRef+"\n"))
		} else if unborn {
			out = append(out, pktStr("unborn HEAD symref-target:"+headRef+"\n"))
		}
	}
	for _, ref := range listRefs(repo) {
		if !keep(ref.Name) {
			continue
		}
		line := ref.OID + " " + ref.Name
		if peel {
			if peeled := peeledOf(repo, ref.Name, ref.OID); peeled != "" {
				line += " peeled:" + peeled
			}
		}
		out = append(out, pktStr(line+"\n"))
	}
	out = append(out, flushPkt)
	return gitOK("application/x-git-upload-pack-result", bytes.Join(out, nil))
}

func parseFetchArgs(items []interface{}) *FetchRequest {
	req := &FetchRequest{}
	pastDelim := false
	for _, item := range items {
		if s, ok := item.(string); ok && s == "delim" {
			pastDelim = true
			continue
		}
		text, ok := item.(string)
		if !ok {
			continue
		}
		text = strings.TrimSuffix(text, "\n")
		if text == "command=fetch" || strings.HasPrefix(text, "agent=") || strings.HasPrefix(text, "object-format=") {
			continue
		}
		if !pastDelim {
			continue
		}
		switch {
		case text == "thin-pack" || text == "no-progress" || text == "ofs-delta" || text == "done":
		case strings.HasPrefix(text, "want "):
			req.Wants = append(req.Wants, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "have "):
			req.Haves = append(req.Haves, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "shallow "):
			req.Shallow = append(req.Shallow, strings.TrimSpace(text[8:]))
		case strings.HasPrefix(text, "deepen "):
			if n, err := strconv.Atoi(text[7:]); err == nil {
				req.Depth, req.HasDepth = n, true
			}
		case strings.HasPrefix(text, "deepen-since "):
			if n, err := strconv.ParseInt(text[13:], 10, 64); err == nil {
				req.DeepenSince, req.HasDeepenSince = n, true
			}
		case strings.HasPrefix(text, "deepen-not "):
			req.DeepenNot = append(req.DeepenNot, strings.TrimSpace(text[11:]))
		}
	}
	return req
}

func handleFetchV2(repo *Repo, items []interface{}) GitResponse {
	req := parseFetchArgs(items)
	plan := planFetch(repo, req)
	pack := writePack(plan.Objects)
	var parts [][]byte
	parts = append(parts, pktStr("acknowledgments\n"))
	for _, line := range ackLines(repo, req) {
		parts = append(parts, pktStr(line))
	}
	parts = append(parts, flushPkt)
	parts = append(parts, pktStr("shallow-info\n"))
	for _, oid := range plan.Shallow {
		parts = append(parts, pktStr("shallow "+oid+"\n"))
	}
	for _, oid := range plan.Unshallow {
		parts = append(parts, pktStr("unshallow "+oid+"\n"))
	}
	parts = append(parts, flushPkt)
	parts = append(parts, pktStr("wanted-refs\n"))
	for _, ref := range plan.WantedRefs {
		parts = append(parts, pktStr(ref.OID+" "+ref.Name+"\n"))
	}
	parts = append(parts, flushPkt)
	parts = append(parts, pktStr("packfile\n"))
	parts = append(parts, sideband(1, pack)...)
	parts = append(parts, flushPkt)
	return gitOK("application/x-git-upload-pack-result", bytes.Join(parts, nil))
}

func handleFetchV0(repo *Repo, frames []PktFrame, body []byte) GitResponse {
	// v0: want lines (first carries caps after a space), shallow/deepen lines, flush,
	// then have/done lines, flush. Parse all data frames in order.
	req := &FetchRequest{}
	for _, frame := range frames {
		if frame.Type != FrameData {
			continue
		}
		text := strings.TrimSuffix(string(frame.Data), "\n")
		switch {
		case strings.HasPrefix(text, "want "):
			rest := text[5:]
			oid := strings.Split(rest, " ")[0]
			if isOid(oid) {
				req.Wants = append(req.Wants, oid)
			}
		case strings.HasPrefix(text, "have "):
			req.Haves = append(req.Haves, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "shallow "):
			req.Shallow = append(req.Shallow, strings.TrimSpace(text[8:]))
		case strings.HasPrefix(text, "deepen "):
			if n, err := strconv.Atoi(text[7:]); err == nil {
				req.Depth, req.HasDepth = n, true
			}
		case strings.HasPrefix(text, "deepen-since "):
			if n, err := strconv.ParseInt(text[13:], 10, 64); err == nil {
				req.DeepenSince, req.HasDeepenSince = n, true
			}
		case strings.HasPrefix(text, "deepen-not "):
			req.DeepenNot = append(req.DeepenNot, strings.TrimSpace(text[11:]))
		}
	}
	plan := planFetch(repo, req)
	pack := writePack(plan.Objects)
	var parts [][]byte
	for _, line := range ackLines(repo, req) {
		parts = append(parts, pktStr(line))
	}
	for _, oid := range plan.Shallow {
		parts = append(parts, pktStr("shallow "+oid+"\n"))
	}
	for _, oid := range plan.Unshallow {
		parts = append(parts, pktStr("unshallow "+oid+"\n"))
	}
	parts = append(parts, sideband(1, pack)...)
	parts = append(parts, flushPkt)
	return gitOK("application/x-git-upload-pack-result", bytes.Join(parts, nil))
}

// -------------------------------------------------------------------------------------------
// receive-pack
// -------------------------------------------------------------------------------------------

type PushCommand struct {
	OldOID string
	NewOID string
	Ref    string
}

func handleReceivePack(store *Store, owner, repoName string, body []byte) GitResponse {
	repo := store.GetRepo(owner, repoName)
	if repo == nil {
		return gitNotFound()
	}
	frames, restOffset := splitFramesAndRest(body)
	var commands []PushCommand
	for _, frame := range frames {
		if frame.Type != FrameData {
			continue
		}
		text := strings.TrimSuffix(string(frame.Data), "\n")
		nul := strings.Index(text, "\x00")
		head := text
		if nul >= 0 {
			head = text[:nul]
		}
		parts := strings.Split(head, " ")
		if len(parts) < 3 || !isOid(parts[0]) || !isOid(parts[1]) {
			continue
		}
		commands = append(commands, PushCommand{OldOID: parts[0], NewOID: parts[1], Ref: parts[2]})
	}
	if len(commands) == 0 {
		return gitBadRequest("no push commands")
	}
	hasUpdate := false
	for _, c := range commands {
		if c.NewOID != ZeroOID {
			hasUpdate = true
			break
		}
	}
	unpackOK := true
	unpackDetail := "ok"
	if hasUpdate {
		packBytes := body[restOffset:]
		if len(packBytes) > 0 {
			objects, err := readPack(packBytes, nil)
			if err != nil {
				unpackOK = false
				unpackDetail = "unpack error: " + err.Error()
			} else {
				for _, obj := range objects {
					repo.Objects[obj.OID] = obj
				}
			}
		}
	}
	var report strings.Builder
	if unpackOK {
		report.WriteString("unpack ok\n")
	} else {
		report.WriteString("unpack " + unpackDetail + "\n")
	}
	for _, cmd := range commands {
		ok, reason := true, ""
		if !unpackOK {
			ok, reason = false, "unpack failed"
		} else {
			ok, reason = applyPushCommand(repo, cmd)
		}
		if ok {
			report.WriteString("ok " + cmd.Ref + "\n")
		} else {
			report.WriteString("ng " + cmd.Ref + " " + reason + "\n")
		}
	}
	var band [][]byte
	for _, line := range strings.SplitAfter(report.String(), "\n") {
		if line == "" {
			continue
		}
		band = append(band, pktStr(line))
	}
	parts := sideband(1, bytes.Join(band, nil))
	parts = append(parts, flushPkt)
	return gitOK("application/x-git-receive-pack-result", bytes.Join(parts, nil))
}

func applyPushCommand(repo *Repo, cmd PushCommand) (bool, string) {
	current, exists := repo.Refs[cmd.Ref]
	if cmd.NewOID == ZeroOID {
		// Delete.
		if !exists {
			return false, "non-fast-forward"
		}
		if cmd.OldOID != ZeroOID && cmd.OldOID != current {
			return false, "non-fast-forward"
		}
		delete(repo.Refs, cmd.Ref)
		return true, ""
	}
	if cmd.OldOID == ZeroOID {
		// Create.
		if exists {
			return false, "non-fast-forward"
		}
		if _, ok := repo.Objects[cmd.NewOID]; !ok {
			return false, "non-fast-forward"
		}
		repo.Refs[cmd.Ref] = cmd.NewOID
		return true, ""
	}
	// Update: old must match the tip and the new tip must descend from it.
	if !exists {
		return false, "non-fast-forward"
	}
	if current != cmd.OldOID {
		return false, "non-fast-forward"
	}
	if cmd.NewOID == cmd.OldOID {
		return true, ""
	}
	if _, ok := repo.Objects[cmd.NewOID]; !ok {
		return false, "non-fast-forward"
	}
	if !isAncestor(repo.Objects, cmd.OldOID, cmd.NewOID) {
		return false, "non-fast-forward"
	}
	repo.Refs[cmd.Ref] = cmd.NewOID
	return true, ""
}
