package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"simple-cot-chat/store"
)

// Ingestion limits. These are guards against pathological input, not UX targets;
// every skip is counted and reported rather than silently dropped.
const (
	// MaxFileBytes skips files larger than this. A 2 MB Markdown file is not a
	// note; it is usually generated output or a pasted dump.
	MaxFileBytes = 2 << 20
	// MaxFilesPerRun bounds a single ingest run.
	MaxFilesPerRun = 20000
	// dedupCandidates is how many existing chunks to compare against for
	// near-duplicate rejection.
	dedupCandidates = 40
)

// skipDirs are never walked. `.obsidian` is workspace config, plugins and themes
// — pure noise. `.trash` is deleted notes the user chose to discard.
var skipDirs = map[string]bool{
	".git": true, ".obsidian": true, ".trash": true, ".stfolder": true,
	"node_modules": true, "dist": true, "build": true, "vendor": true,
	".venv": true, "__pycache__": true, ".cache": true,
}

var markdownExts = map[string]bool{".md": true, ".markdown": true}

// IngestReport summarizes a run. Everything skipped is counted: a silent skip
// reads as "covered everything" when it did not.
type IngestReport struct {
	Root             string        `json:"root"`
	FilesSeen        int           `json:"filesSeen"`
	FilesIngested    int           `json:"filesIngested"`
	FilesUnchanged   int           `json:"filesUnchanged"`
	FilesTooLarge    int           `json:"filesTooLarge"`
	FilesUnreadable  int           `json:"filesUnreadable"`
	FilesOverCap     int           `json:"filesOverCap"`
	ChunksWritten    int           `json:"chunksWritten"`
	ChunksDeduped    int           `json:"chunksDeduped"`
	LinksFound       int           `json:"linksFound"`
	LinksResolved    int           `json:"linksResolved"`
	LinksUnresolved  int           `json:"linksUnresolved"`
	UnresolvedSample []string      `json:"unresolvedSample"`
	TagsFound        int           `json:"tagsFound"`
	DailyNotes       int           `json:"dailyNotes"`
	Duration         time.Duration `json:"duration"`
}

// UnresolvedRate returns the share of links that could not be resolved, which is
// the health metric for wikilink handling on a real vault.
func (r IngestReport) UnresolvedRate() float64 {
	if r.LinksFound == 0 {
		return 0
	}
	return float64(r.LinksUnresolved) / float64(r.LinksFound)
}

func (r IngestReport) String() string {
	return fmt.Sprintf(
		"ingested %d/%d files (%d unchanged, %d too large, %d unreadable), "+
			"%d chunks (+%d deduped), links %d/%d resolved (%.1f%% unresolved), "+
			"%d tags, %d daily notes, in %s",
		r.FilesIngested, r.FilesSeen, r.FilesUnchanged, r.FilesTooLarge, r.FilesUnreadable,
		r.ChunksWritten, r.ChunksDeduped, r.LinksResolved, r.LinksFound,
		100*r.UnresolvedRate(), r.TagsFound, r.DailyNotes, r.Duration.Round(time.Millisecond))
}

// PendingLink is a resolved link awaiting edge construction in Phase 6. Parsing
// and resolution happen at ingest, where the vault index is in hand; the edges
// themselves need chunk ids for both ends and so are written later.
type PendingLink struct {
	FromSourceRef string
	ToSourceRef   string
	Heading       string
	Embed         bool
}

// Ingester turns Markdown into stored memory.
type Ingester struct {
	store  *store.Store
	tokens TokenCounter
	// Links accumulates resolved links from the most recent run.
	Links []PendingLink
}

// NewIngester builds an ingester. tc may be nil, in which case an exact BERT
// counter is used if tokenizer.json can be found and a heuristic otherwise —
// tokenization needs only the 0.7 MB tokenizer.json, not the ONNX weights, so
// exact counts are usually available even before the model is provisioned.
func NewIngester(s *store.Store, tc TokenCounter) *Ingester {
	if tc == nil {
		tc = defaultTokenCounter()
	}
	return &Ingester{store: s, tokens: tc}
}

// TokenCounterName reports which counter is in use, so a run's chunk boundaries
// can be attributed.
func (ing *Ingester) TokenCounterName() string { return ing.tokens.Name() }

func defaultTokenCounter() TokenCounter {
	if p := TokenizerPath(); p != "" {
		if tc, err := NewBERTCounter(p); err == nil {
			return tc
		} else {
			slog.Warn("falling back to heuristic token counting", "error", err)
		}
	} else {
		slog.Info("tokenizer.json not found; using heuristic token counting")
	}
	return NewHeuristicCounter()
}

// TokenizerPath locates tokenizer.json using the same resolution order the
// embedder will use for the model itself.
func TokenizerPath() string {
	if p := os.Getenv("LOCALCHAT_TOKENIZER"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".cache", "localchat", "models", "bge-small-en-v1.5", "tokenizer.json"))
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "models", "bge-small-en-v1.5", "tokenizer.json"),
			filepath.Join(dir, "tokenizer.json"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// noteFile is one discovered Markdown file.
type noteFile struct {
	absPath string
	relPath string
	size    int64
	modTime time.Time
}

// IngestDirectory walks root and ingests every Markdown file, replacing any
// previous content for files whose hash changed and skipping those unchanged.
//
// Directories are the primary source (§3.3), so this is the path that matters
// most: it must be incremental over thousands of files, Obsidian-aware, and
// honest about what it skipped.
func (ing *Ingester) IngestDirectory(ctx context.Context, root string, onProgress func(done, total int)) (IngestReport, error) {
	started := time.Now()
	rep := IngestReport{Root: root}
	ing.Links = nil

	info, err := os.Stat(root)
	if err != nil {
		return rep, fmt.Errorf("ingest root %s: %w", root, err)
	}
	if !info.IsDir() {
		return rep, fmt.Errorf("ingest root %s is not a directory", root)
	}

	files, rep2, err := discoverNotes(root, rep)
	if err != nil {
		return rep2, err
	}
	rep = rep2

	// Pass 1: build the link resolver from every note's path and aliases, so a
	// link to a note later in the walk still resolves. Aliases require reading
	// frontmatter, which is cheap relative to full ingestion.
	resolver := NewLinkResolver()
	for _, f := range files {
		aliases := readAliases(f.absPath)
		resolver.AddNote(f.relPath, aliases)
	}

	// Pass 2: ingest.
	for i, f := range files {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if onProgress != nil {
			onProgress(i, len(files))
		}

		res, err := ing.ingestFile(f, resolver)
		if err != nil {
			rep.FilesUnreadable++
			slog.Warn("skipping unreadable note", "path", f.relPath, "error", err)
			continue
		}
		if res.unchanged {
			rep.FilesUnchanged++
			continue
		}
		rep.FilesIngested++
		rep.ChunksWritten += res.chunksWritten
		rep.ChunksDeduped += res.chunksDeduped
		rep.TagsFound += res.tags
		if res.dailyNote {
			rep.DailyNotes++
		}
		for _, l := range res.links {
			rep.LinksFound++
			if l.ToSourceRef != "" {
				rep.LinksResolved++
				ing.Links = append(ing.Links, l)
			} else {
				rep.LinksUnresolved++
				if len(rep.UnresolvedSample) < 10 {
					rep.UnresolvedSample = append(rep.UnresolvedSample, l.Heading)
				}
			}
		}
	}
	if onProgress != nil {
		onProgress(len(files), len(files))
	}

	if err := ing.store.MarkStatsDirty(); err != nil {
		return rep, err
	}
	rep.Duration = time.Since(started)
	return rep, nil
}

func discoverNotes(root string, rep IngestReport) ([]noteFile, IngestReport, error) {
	var files []noteFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory should not abort the whole run.
			slog.Warn("skipping unreadable path", "path", path, "error", err)
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (skipDirs[name] || strings.HasPrefix(name, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if !markdownExts[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}
		rep.FilesSeen++
		if len(files) >= MaxFilesPerRun {
			rep.FilesOverCap++
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			rep.FilesUnreadable++
			return nil
		}
		if fi.Size() > MaxFileBytes {
			rep.FilesTooLarge++
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		files = append(files, noteFile{
			absPath: path,
			relPath: filepath.ToSlash(rel),
			size:    fi.Size(),
			modTime: fi.ModTime().UTC(),
		})
		return nil
	})
	if err != nil {
		return nil, rep, fmt.Errorf("walk %s: %w", root, err)
	}
	// Deterministic order keeps runs reproducible and makes ambiguous-basename
	// resolution stable.
	sort.Slice(files, func(i, j int) bool { return files[i].relPath < files[j].relPath })
	return files, rep, nil
}

// readAliases extracts frontmatter aliases without a full ingest, for pass 1.
func readAliases(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	// Frontmatter lives at the top; 8 KB is far more than enough.
	buf := make([]byte, 8<<10)
	n, _ := f.Read(buf)
	fmRaw, _ := SplitFrontmatter(string(buf[:n]))
	if fmRaw == "" {
		return nil
	}
	fm := ParseFrontmatter(fmRaw)
	out := append([]string{}, fm.List("aliases")...)
	return append(out, fm.List("alias")...)
}

type fileResult struct {
	unchanged     bool
	chunksWritten int
	chunksDeduped int
	tags          int
	dailyNote     bool
	links         []PendingLink
}

func (ing *Ingester) ingestFile(f noteFile, resolver *LinkResolver) (fileResult, error) {
	var res fileResult

	raw, err := os.ReadFile(f.absPath)
	if err != nil {
		return res, err
	}
	content := string(raw)
	hash := hashContent(content)
	sourceRef := f.relPath

	// Incremental skip: unchanged content is a no-op. This is what keeps a
	// re-scan of a thousands-of-notes vault cheap.
	if existing, found, err := ing.store.FindSource(store.SourceDirectory, sourceRef); err != nil {
		return res, err
	} else if found && existing.ContentHash == hash {
		res.unchanged = true
		return res, nil
	}

	fmRaw, body := SplitFrontmatter(content)
	fm := ParseFrontmatter(fmRaw)

	// Vault-native signals.
	tags := ExtractTags(body)
	for _, t := range fm.List("tags") {
		tags = append(tags, strings.ToLower(strings.TrimPrefix(strings.TrimSpace(t), "#")))
	}
	tags = dedupeStrings(tags)
	res.tags = len(tags)

	var dates []string
	if d, ok := DailyNoteDate(f.relPath); ok {
		dates = append(dates, d)
		res.dailyNote = true
	}
	for _, key := range []string{"date", "created", "updated"} {
		for _, v := range fm.List(key) {
			if len(v) >= 10 && isoDateRe.MatchString(v[:10]) {
				dates = append(dates, v[:10])
			}
		}
	}
	dates = dedupeStrings(dates)

	title := fm.Get("title")
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(f.relPath), filepath.Ext(f.relPath))
	}

	blocks := ParseBlocks(body)
	chunks := ChunkHeadings(blocks, ing.tokens)

	// Links are resolved here, where the vault index is in hand. Edges are
	// written in Phase 6 once both endpoints have chunk ids.
	for _, l := range ExtractLinks(body) {
		pl := PendingLink{FromSourceRef: sourceRef, Heading: l.Heading, Embed: l.Embed}
		if target, ok := resolver.Resolve(l.Target); ok {
			pl.ToSourceRef = target
		} else {
			pl.Heading = l.Target // carry the unresolved target for reporting
		}
		res.links = append(res.links, pl)
	}

	// Terms are needed for both storage and dedup probing, so compute once.
	chunkTerms := make([]map[string]int, len(chunks))
	for i, c := range chunks {
		chunkTerms[i] = ExtractTerms(c.Text)
	}

	// Fetch dedup candidates once per file rather than once per chunk. One query
	// per chunk made ingestion dominated by query overhead; the candidate set for
	// a whole file is barely larger than for a single chunk, because chunks of one
	// note share most of their rare terms.
	candidates, err := ing.dedupCandidates(chunkTerms, sourceRef)
	if err != nil {
		return res, err
	}

	// Precompute the candidates' n-gram sets once for the whole file.
	candidateSets := make([]*NgramSet, len(candidates))
	for i, c := range candidates {
		candidateSets[i] = NewNgramSet(c.Text)
	}

	storeChunks := make([]store.MemoryChunk, 0, len(chunks))
	acceptedSets := make([]*NgramSet, 0, len(chunks))
	for i, c := range chunks {
		// Compare against the corpus and against chunks already accepted from
		// this same file — a note that repeats a passage internally should not
		// store it twice either.
		set := NewNgramSet(c.Text)
		if isDuplicate(set, candidateSets) || isDuplicate(set, acceptedSets) {
			res.chunksDeduped++
			continue
		}
		acceptedSets = append(acceptedSets, set)
		storeChunks = append(storeChunks, store.MemoryChunk{
			Text:        c.Text,
			HeadingPath: c.HeadingPath,
			TokenCount:  c.TokenCount,
			CharLen:     len(c.Text),
			Terms:       chunkTerms[i],
			Entities:    ExtractEntities(c.Text, tags, dates),
		})
	}

	if _, err := ing.store.ReplaceSource(store.MemorySource{
		SourceType:  store.SourceDirectory,
		SourceRef:   sourceRef,
		Title:       title,
		Path:        f.absPath,
		ContentHash: hash,
		MTime:       f.modTime.Format(time.RFC3339),
	}, storeChunks); err != nil {
		return res, err
	}
	res.chunksWritten = len(storeChunks)
	return res, nil
}

// dedupCandidates fetches chunks that might duplicate any of this file's chunks,
// preselected by the rarest terms across the whole file.
//
// Preselecting by rare terms is what keeps dedup sub-quadratic: a rare term is
// highly selective, so the candidate set stays small even as the corpus grows,
// and comparing every new chunk against every stored chunk is avoided.
func (ing *Ingester) dedupCandidates(chunkTerms []map[string]int, excludeSourceRef string) ([]store.MemoryChunk, error) {
	probeSet := map[string]bool{}
	for _, terms := range chunkTerms {
		for _, t := range rarestTerms(terms, 6) {
			probeSet[t] = true
		}
	}
	if len(probeSet) == 0 {
		return nil, nil
	}
	probe := make([]string, 0, len(probeSet))
	for t := range probeSet {
		probe = append(probe, t)
	}
	sort.Strings(probe) // deterministic query shape
	limit := dedupCandidates * (1 + len(chunkTerms)/4)
	return ing.store.CandidateChunksByTerms(probe, limit, excludeSourceRef)
}

// isDuplicate reports whether set matches any of the precomputed sets closely
// enough to be a duplicate.
func isDuplicate(set *NgramSet, others []*NgramSet) bool {
	for _, o := range others {
		if set.Dice(o, DedupThreshold) >= DedupThreshold {
			return true
		}
	}
	return false
}

func hashContent(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
