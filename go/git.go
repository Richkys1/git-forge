// Git object model: SHA-1 content addressing, zlib storage, tree/commit/tag
// build + parse, and packfile read/write (v2, full objects + REF_DELTA/OFS_DELTA).
// Standard git formats only; no git binary anywhere.
// Faithful port of lib/git.ts — same formats, same edge cases.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"sort"
	"strings"
	"sync"
)

type ObjType string

const (
	ObjBlob   ObjType = "blob"
	ObjTree   ObjType = "tree"
	ObjCommit ObjType = "commit"
	ObjTag    ObjType = "tag"
)

type GitObject struct {
	Type ObjType
	Data []byte
	OID  string
	// Memoized pack fragment: object header + zlib-deflated data, exactly as
	// writePack emits it. Objects are immutable, so the fragment never changes.
	frag   []byte
	fragMu sync.Mutex
}

// Largest object (bytes) whose pack fragment writePack memoizes.
const packMemoMaxBytes = 256 * 1024

const ZeroOID = "0000000000000000000000000000000000000000"

func isOid(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// The git object id: sha1("<type> <len>\0<data>").
func hashObject(typ ObjType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func storeObject(objects map[string]*GitObject, typ ObjType, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := objects[oid]; !ok {
		objects[oid] = &GitObject{Type: typ, Data: data, OID: oid}
	}
	return oid
}

// ---------------------------------------------------------------------------------------------
// trees
// ---------------------------------------------------------------------------------------------

type TreeEntry struct {
	Mode string // '100644' | '100755' | '120000' | '40000'
	Name string
	OID  string
}

// Tree bytes: entries sorted by name with directories sorted as if named "name/".
func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		an := sorted[i].Name
		if sorted[i].Mode == "40000" {
			an += "/"
		}
		bn := sorted[j].Name
		if sorted[j].Mode == "40000" {
			bn += "/"
		}
		return an < bn
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		fmt.Fprintf(&buf, "%s %s\x00", e.Mode, e.Name)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

func parseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	i := 0
	for i < len(data) {
		sp := bytes.IndexByte(data[i:], ' ')
		if sp < 0 {
			break
		}
		sp += i
		nul := bytes.IndexByte(data[sp:], 0)
		if nul < 0 {
			break
		}
		nul += sp
		mode := string(data[i:sp])
		name := string(data[sp+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		i = nul + 21
	}
	return entries
}

// Look a path up in a tree, walking subtrees. Returns the blob oid + mode, or nil.
func treeLookup(objects map[string]*GitObject, treeOID string, path string) (oid, mode string, ok bool) {
	segments := splitPath(path)
	cur := treeOID
	for i, seg := range segments {
		obj := objects[cur]
		if obj == nil || obj.Type != ObjTree {
			return "", "", false
		}
		var entry *TreeEntry
		for _, e := range parseTree(obj.Data) {
			if e.Name == seg {
				ee := e
				entry = &ee
				break
			}
		}
		if entry == nil {
			return "", "", false
		}
		if i == len(segments)-1 {
			return entry.OID, entry.Mode, true
		}
		if entry.Mode != "40000" {
			return "", "", false
		}
		cur = entry.OID
	}
	return "", "", false
}

func splitPath(path string) []string {
	parts := strings.Split(path, "/")
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

type FlatFile struct {
	Path string
	OID  string
	Mode string
}

// Every entry of a tree, flattened to full paths (files only; empty dirs dropped).
func treeFlatten(objects map[string]*GitObject, treeOID string) []FlatFile {
	return treeFlattenPrefix(objects, treeOID, "")
}

func treeFlattenPrefix(objects map[string]*GitObject, treeOID, prefix string) []FlatFile {
	obj := objects[treeOID]
	if obj == nil || obj.Type != ObjTree {
		return nil
	}
	var out []FlatFile
	for _, e := range parseTree(obj.Data) {
		p := e.Name
		if prefix != "" {
			p = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			out = append(out, treeFlattenPrefix(objects, e.OID, p)...)
		} else {
			out = append(out, FlatFile{Path: p, OID: e.OID, Mode: e.Mode})
		}
	}
	return out
}

// A new tree equal to `base` with `path` set to (oid, mode) — or removed when oid is "".
func treeSet(objects map[string]*GitObject, baseTreeOID string, hasBase bool, path, oid, mode string) string {
	segments := splitPath(path)
	var build func(treeOID string, hasTree bool, depth int) (string, bool)
	build = func(treeOID string, hasTree bool, depth int) (string, bool) {
		var entries []TreeEntry
		if hasTree {
			if obj := objects[treeOID]; obj != nil {
				entries = parseTree(obj.Data)
			}
		}
		name := segments[depth]
		last := depth == len(segments)-1
		idx := -1
		for i, e := range entries {
			if e.Name == name {
				idx = i
				break
			}
		}
		if last {
			if oid == "" {
				if idx != -1 {
					entries = append(entries[:idx], entries[idx+1:]...)
				}
			} else if idx != -1 {
				entries[idx] = TreeEntry{Mode: mode, Name: name, OID: oid}
			} else {
				entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
			}
		} else {
			var child string
			var hasChild bool
			if idx != -1 && entries[idx].Mode == "40000" {
				child, hasChild = entries[idx].OID, true
			}
			next, ok := build(child, hasChild, depth+1)
			if !ok {
				if idx != -1 {
					entries = append(entries[:idx], entries[idx+1:]...)
				}
			} else if idx != -1 {
				entries[idx] = TreeEntry{Mode: "40000", Name: name, OID: next}
			} else {
				entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: next})
			}
		}
		if len(entries) == 0 {
			return "", false
		}
		return storeObject(objects, ObjTree, buildTree(entries)), true
	}
	result, ok := build(baseTreeOID, hasBase, 0)
	if !ok {
		return storeObject(objects, ObjTree, buildTree(nil))
	}
	return result
}

// ---------------------------------------------------------------------------------------------
// commits and tags
// ---------------------------------------------------------------------------------------------

type CommitInfo struct {
	Tree           string
	Parents        []string
	Author         string
	Committer      string
	Message        string
	AuthorTs       int64
	CommitterTs    int64
	AuthorTz       string
	CommitterTz    string
	Timestamp      int64 // the author timestamp (alias of AuthorTs)
	AuthorName     string
	AuthorEmail    string
	CommitterName  string
	CommitterEmail string
}

func splitIdentity(who string) (name, email string) {
	// "Name <email>"
	lt := strings.LastIndex(who, "<")
	gt := strings.LastIndex(who, ">")
	if lt >= 0 && gt > lt {
		return strings.TrimSpace(who[:lt]), who[lt+1 : gt]
	}
	return strings.TrimSpace(who), ""
}

type CommitInput struct {
	Tree      string
	Parents   []string
	Message   string
	Author    string
	Committer string
	Timestamp int64
	TZ        string
}

func buildCommit(c CommitInput) []byte {
	tz := c.TZ
	if tz == "" {
		tz = "+0000"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", c.Tree)
	for _, p := range c.Parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author %s %d %s\n", c.Author, c.Timestamp, tz)
	fmt.Fprintf(&b, "committer %s %d %s\n", c.Committer, c.Timestamp, tz)
	b.WriteString("\n")
	msg := c.Message
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	b.WriteString(msg)
	return []byte(b.String())
}

func parseCommit(data []byte) CommitInfo {
	text := string(data)
	headEnd := strings.Index(text, "\n\n")
	head, message := text, ""
	if headEnd != -1 {
		head = text[:headEnd]
		message = text[headEnd+2:]
	}
	var info CommitInfo
	info.AuthorTz = "+0000"
	info.CommitterTz = "+0000"
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			info.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			info.Parents = append(info.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			rest := line[7:]
			if name, ts, tz, ok := parseIdentLine(rest); ok {
				info.Author, info.AuthorTs, info.AuthorTz = name, ts, tz
			} else {
				info.Author = rest
			}
		case strings.HasPrefix(line, "committer "):
			rest := line[10:]
			if name, ts, tz, ok := parseIdentLine(rest); ok {
				info.Committer, info.CommitterTs, info.CommitterTz = name, ts, tz
			} else {
				info.Committer = rest
			}
		}
	}
	info.Message = message
	info.Timestamp = info.AuthorTs
	info.AuthorName, info.AuthorEmail = splitIdentity(info.Author)
	info.CommitterName, info.CommitterEmail = splitIdentity(info.Committer)
	return info
}

// Parse "Name <email> 1234567890 +0000" at the end of an author/committer line.
func parseIdentLine(rest string) (name string, ts int64, tz string, ok bool) {
	// Find the last two space-separated tokens: timestamp and tz.
	lastSp := strings.LastIndex(rest, " ")
	if lastSp < 0 {
		return "", 0, "", false
	}
	tz = rest[lastSp+1:]
	if len(tz) != 5 || (tz[0] != '+' && tz[0] != '-') {
		return "", 0, "", false
	}
	for _, c := range tz[1:] {
		if c < '0' || c > '9' {
			return "", 0, "", false
		}
	}
	before := rest[:lastSp]
	sp2 := strings.LastIndex(before, " ")
	if sp2 < 0 {
		return "", 0, "", false
	}
	tsStr := before[sp2+1:]
	var t int64
	for _, c := range tsStr {
		if c < '0' || c > '9' {
			return "", 0, "", false
		}
		t = t*10 + int64(c-'0')
	}
	return before[:sp2], t, tz, true
}

type TagInput struct {
	Object    string
	Type      ObjType
	Tag       string
	Message   string
	Tagger    string
	Timestamp int64
	TZ        string
}

func buildTag(t TagInput) []byte {
	tz := t.TZ
	if tz == "" {
		tz = "+0000"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "object %s\n", t.Object)
	fmt.Fprintf(&b, "type %s\n", t.Type)
	fmt.Fprintf(&b, "tag %s\n", t.Tag)
	fmt.Fprintf(&b, "tagger %s %d %s\n", t.Tagger, t.Timestamp, tz)
	b.WriteString("\n")
	msg := t.Message
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	b.WriteString(msg)
	return []byte(b.String())
}

type TagInfo struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

func parseTag(data []byte) TagInfo {
	text := string(data)
	headEnd := strings.Index(text, "\n\n")
	head, message := text, ""
	if headEnd != -1 {
		head = text[:headEnd]
		message = text[headEnd+2:]
	}
	var info TagInfo
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			info.Object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			info.Type = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "tag "):
			info.Tag = strings.TrimSpace(line[4:])
		case strings.HasPrefix(line, "tagger "):
			info.Tagger = strings.TrimSpace(line[7:])
		}
	}
	info.Message = message
	return info
}

// ---------------------------------------------------------------------------------------------
// reachability
// ---------------------------------------------------------------------------------------------

// All object ids reachable from `tips` (commits walk parents + trees + tag targets).
func collectReachable(tips []string, lookup func(oid string) *GitObject) map[string]bool {
	seen := map[string]bool{}
	stack := append([]string(nil), tips...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			panic("missing object " + oid)
		}
		seen[oid] = true
		switch obj.Type {
		case ObjCommit:
			c := parseCommit(obj.Data)
			stack = append(stack, c.Tree)
			stack = append(stack, c.Parents...)
		case ObjTag:
			stack = append(stack, parseTag(obj.Data).Object)
		case ObjTree:
			for _, e := range parseTree(obj.Data) {
				stack = append(stack, e.OID)
			}
		}
	}
	return seen
}

// True when `ancestor` is reachable from `tip` (or equal): the fast-forward test.
func isAncestor(objects map[string]*GitObject, ancestor, tip string) bool {
	if ancestor == tip {
		return true
	}
	seen := map[string]bool{tip: true}
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		obj := objects[oid]
		if obj == nil {
			continue
		}
		switch obj.Type {
		case ObjCommit:
			for _, p := range parseCommit(obj.Data).Parents {
				if p == ancestor {
					return true
				}
				if !seen[p] {
					seen[p] = true
					stack = append(stack, p)
				}
			}
		case ObjTag:
			t := parseTag(obj.Data).Object
			if t == ancestor {
				return true
			}
			if !seen[t] {
				seen[t] = true
				stack = append(stack, t)
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------------------------
// packfiles
// ---------------------------------------------------------------------------------------------

var packTypes = map[int]ObjType{1: ObjCommit, 2: ObjTree, 3: ObjBlob, 4: ObjTag}
var typeCodes = map[ObjType]int{ObjCommit: 1, ObjTree: 2, ObjBlob: 3, ObjTag: 4}

// Read a pack v2/v3 stream. Returns objects in pack order, deltas fully resolved.
// `lookup` resolves REF_DELTA bases that live in the repository (not the pack);
// OFS_DELTA bases are always earlier entries in the same pack, so they resolve inline.
func readPack(buf []byte, lookup func(oid string) *GitObject) ([]*GitObject, error) {
	if len(buf) < 12 || string(buf[0:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	version := binary.BigEndian.Uint32(buf[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := binary.BigEndian.Uint32(buf[8:12])
	// Verify the trailing checksum over everything before it.
	want := sha1.Sum(buf[:len(buf)-20])
	got := buf[len(buf)-20:]
	if !bytes.Equal(want[:], got) {
		return nil, errors.New("pack checksum mismatch")
	}
	off := 12
	objects := make([]*GitObject, 0, count)
	byOffset := map[int]*GitObject{}
	byOID := map[string]*GitObject{}
	for n := uint32(0); n < count; n++ {
		start := off
		b := buf[off]
		off++
		typeCode := (b >> 4) & 0x7
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			b = buf[off]
			off++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		var obj *GitObject
		switch {
		case typeCode == 6:
			// OFS_DELTA: negative offset to the base object (always an earlier entry).
			bb := buf[off]
			off++
			baseOff := int(bb & 0x7f)
			for bb&0x80 != 0 {
				bb = buf[off]
				off++
				baseOff = ((baseOff + 1) << 7) | int(bb&0x7f)
			}
			base := byOffset[start-baseOff]
			if base == nil {
				return nil, errors.New("delta base not in pack")
			}
			length, err := deflateLen(buf, off)
			if err != nil {
				return nil, err
			}
			delta, err := inflate(buf[off : off+length])
			if err != nil {
				return nil, err
			}
			off += length
			data, err := applyDelta(base.Data, delta)
			if err != nil {
				return nil, err
			}
			obj = &GitObject{Type: base.Type, Data: data, OID: hashObject(base.Type, data)}
		case typeCode == 7:
			// REF_DELTA: base by object id — an earlier pack entry, or in the repository.
			baseOID := hex.EncodeToString(buf[off : off+20])
			off += 20
			length, err := deflateLen(buf, off)
			if err != nil {
				return nil, err
			}
			delta, err := inflate(buf[off : off+length])
			if err != nil {
				return nil, err
			}
			off += length
			base := byOID[baseOID]
			if base == nil && lookup != nil {
				base = lookup(baseOID)
			}
			if base == nil {
				return nil, fmt.Errorf("delta base %s not found", baseOID)
			}
			data, err := applyDelta(base.Data, delta)
			if err != nil {
				return nil, err
			}
			obj = &GitObject{Type: base.Type, Data: data, OID: hashObject(base.Type, data)}
		default:
			typ, ok := packTypes[int(typeCode)]
			if !ok {
				return nil, fmt.Errorf("unknown pack object type %d", typeCode)
			}
			length, err := deflateLen(buf, off)
			if err != nil {
				return nil, err
			}
			raw, err := inflate(buf[off : off+length])
			if err != nil {
				return nil, err
			}
			off += length
			if len(raw) != size {
				return nil, errors.New("pack object size mismatch")
			}
			obj = &GitObject{Type: typ, Data: raw, OID: hashObject(typ, raw)}
		}
		objects = append(objects, obj)
		byOffset[start] = obj
		byOID[obj.OID] = obj
	}
	return objects, nil
}

func inflate(data []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func deflate(data []byte) []byte {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// Bytes consumed by the zlib stream starting at `off`. Inflate once to get the data;
// the stream's Adler-32 trailer then locates its end, and a verification inflate on
// the candidate slice rules out a coincidental match inside the deflate data.
func deflateLen(buf []byte, off int) (int, error) {
	data, err := inflate(buf[off:])
	if err != nil {
		return 0, err
	}
	want := adler32.Checksum(data)
	wantBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(wantBytes, want)
	pos := off + 2
	max := len(buf) - 4
	for pos <= max {
		if bytes.Equal(buf[pos:pos+4], wantBytes) {
			end := pos + 4
			if round, err := inflate(buf[off:end]); err == nil && bytes.Equal(round, data) {
				return end - off, nil
			}
		}
		pos++
	}
	return 0, errors.New("could not find zlib stream end")
}

// Apply a git delta stream to a base buffer.
func applyDelta(base, delta []byte) ([]byte, error) {
	p := 0
	readVarint := func() (int, error) {
		v := 0
		shift := uint(0)
		for {
			if p >= len(delta) {
				return 0, errors.New("delta truncated")
			}
			b := delta[p]
			p++
			v |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return v, nil
			}
		}
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	var out bytes.Buffer
	total := 0
	for p < len(delta) {
		op := delta[p]
		p++
		if op&0x80 != 0 {
			off, size := 0, 0
			if op&0x01 != 0 {
				off = int(delta[p])
				p++
			}
			if op&0x02 != 0 {
				off |= int(delta[p]) << 8
				p++
			}
			if op&0x04 != 0 {
				off |= int(delta[p]) << 16
				p++
			}
			if op&0x08 != 0 {
				off |= int(delta[p]) << 24
				p++
			}
			if op&0x10 != 0 {
				size = int(delta[p])
				p++
			}
			if op&0x20 != 0 {
				size |= int(delta[p]) << 8
				p++
			}
			if op&0x40 != 0 {
				size |= int(delta[p]) << 16
				p++
			}
			if size == 0 {
				size = 0x10000
			}
			out.Write(base[off : off+size])
			total += size
		} else if op != 0 {
			out.Write(delta[p : p+int(op)])
			p += int(op)
			total += int(op)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if total != dstSize {
		return nil, errors.New("delta result size mismatch")
	}
	return out.Bytes(), nil
}

// One object's pack fragment: header bytes (type + size varint) + deflated data.
func packFragmentOf(obj *GitObject) []byte {
	obj.fragMu.Lock()
	defer obj.fragMu.Unlock()
	if obj.frag != nil {
		return obj.frag
	}
	code := typeCodes[obj.Type]
	size := len(obj.Data)
	var header []byte
	s := size
	header = append(header, byte((code<<4)|(s&0x0f)))
	s >>= 4
	for s > 0 {
		header[len(header)-1] |= 0x80
		header = append(header, byte(s&0x7f))
		s >>= 7
	}
	fragment := append(append([]byte(nil), header...), deflate(obj.Data)...)
	// Memoize only small objects: the fragment is at most the deflated size, and a
	// repository's objects are already held in memory, so this adds no unbounded state.
	if size <= packMemoMaxBytes {
		obj.frag = fragment
	}
	return fragment
}

// Write a pack v2 of full (undeltified) objects, with trailing checksum.
func writePack(objects []*GitObject) []byte {
	head := make([]byte, 12)
	copy(head[0:4], "PACK")
	binary.BigEndian.PutUint32(head[4:8], 2)
	binary.BigEndian.PutUint32(head[8:12], uint32(len(objects)))
	parts := make([][]byte, 0, len(objects)+2)
	parts = append(parts, head)
	for _, obj := range objects {
		parts = append(parts, packFragmentOf(obj))
	}
	body := bytes.Join(parts, nil)
	sum := sha1.Sum(body)
	return append(body, sum[:]...)
}
