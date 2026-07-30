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
	Root      string `json:"root"`
	FilesSeen int    `json:"filesSeen"`
	// FilesIngested counts files whose content was chunked and stored.
	FilesIngested int `json:"filesIngested"`
	// FilesUnchanged counts files skipped as unchanged, split by how it was decided.
	// FilesSkippedByStat never opened the file at all — that is where the
	// incremental win comes from, and separating the two is what proves the fast
	// path is actually being taken rather than silently falling through to hashing.
	FilesUnchanged     int `json:"filesUnchanged"`
	FilesSkippedByStat int `json:"filesSkippedByStat"`
	FilesSkippedByHash int `json:"filesSkippedByHash"`
	// FilesDeleted counts sources removed because the note no longer exists on disk.
	FilesDeleted     int           `json:"filesDeleted"`
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
	BytesRead        int64         `json:"bytesRead"`
	Duration         time.Duration `json:"duration"`

	// ChangedRefs and ChangedSourceIDs name the sources this run wrote or removed.
	// Edge construction needs them to rebuild only what changed: rebuilding the
	// whole graph after a two-file edit would undo the point of incremental ingest.
	ChangedRefs      []string `json:"changedRefs"`
	ChangedSourceIDs []string `json:"changedSourceIds"`
}

// Incremental reports whether this run behaved like an incremental re-scan, which
// is the property the exit criterion is about.
func (r IngestReport) Incremental() bool {
	return r.FilesSeen > 0 && r.FilesIngested < r.FilesSeen
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
		"ingested %d/%d files (%d unchanged: %d by stat, %d by hash; %d deleted, "+
			"%d too large, %d unreadable), %d chunks (+%d deduped), "+
			"links %d/%d resolved (%.1f%% unresolved), %d tags, %d daily notes, "+
			"%.1f MB read, in %s",
		r.FilesIngested, r.FilesSeen, r.FilesUnchanged, r.FilesSkippedByStat,
		r.FilesSkippedByHash, r.FilesDeleted, r.FilesTooLarge, r.FilesUnreadable,
		r.ChunksWritten, r.ChunksDeduped, r.LinksResolved, r.LinksFound,
		100*r.UnresolvedRate(), r.TagsFound, r.DailyNotes,
		float64(r.BytesRead)/(1<<20), r.Duration.Round(time.Millisecond))
}

// PendingLink is a resolved link awaiting edge construction. Parsing and
// resolution happen at ingest, where the vault index is in hand; the edges
// themselves need chunk ids for both ends and so are written by BuildLinkEdges
// once the chunks are persisted.
type PendingLink struct {
	FromSourceRef string
	// ToSourceRef is the resolved target, or "" when the target does not exist in
	// the vault.
	ToSourceRef string
	// Target is the raw link target as written, kept for reporting unresolved links.
	Target string
	// Heading is the "#section" part, used to aim the edge at the chunk carrying
	// that heading rather than the note's first chunk.
	Heading string
	// Raw is the link as written (or, for an inferred link, the evidence quote),
	// used to find the chunk that contains it.
	Raw   string
	Embed bool
	// Kind is EdgeLink for an authored link or EdgeInferredLink for one the LLM pass
	// proposed. Empty means EdgeLink.
	Kind string
}

// Ingester turns Markdown into stored memory.
type Ingester struct {
	store  *store.Store
	tokens TokenCounter
	// Links accumulates resolved links from the most recent run.
	Links []PendingLink

	// chunker selects the strategy. Headings is the default and the baseline the
	// others must beat; all remain available so a measured loss can be reverted
	// without code changes.
	chunker ChunkerKind
	leiden  LeidenChunkerConfig
}

// NewIngester builds an ingester. tc may be nil, in which case an exact BERT
// counter is used if tokenizer.json can be found and a heuristic otherwise —
// tokenization needs only the 0.7 MB tokenizer.json, not the ONNX weights, so
// exact counts are usually available even before the model is provisioned.
func NewIngester(s *store.Store, tc TokenCounter) *Ingester {
	if tc == nil {
		tc = defaultTokenCounter()
	}
	return &Ingester{store: s, tokens: tc, chunker: ChunkerHeadings}
}

// SetChunker selects the chunking strategy. The Leiden variants need a similarity
// function; the semantic one additionally needs an embedder.
//
// Returns an error rather than silently falling back, so a misconfiguration is
// visible instead of quietly producing baseline chunks that look like Leiden's.
func (ing *Ingester) SetChunker(kind ChunkerKind, emb Embedder, cfg LeidenChunkerConfig) error {
	if !kind.Valid() {
		return fmt.Errorf("unknown chunker %q", kind)
	}
	if cfg.Graph.TopK == 0 {
		cfg.Graph = DefaultGraphParams()
	}
	if cfg.Resolution == 0 {
		cfg.Resolution = DefaultResolution
	}
	switch kind {
	case ChunkerLeidenLexical:
		if cfg.Similarity == nil {
			cfg.Similarity = LexicalSimilarity{}
		}
	case ChunkerLeidenSemantic:
		if cfg.Similarity == nil {
			if emb == nil {
				return fmt.Errorf("chunker %q requires an embedder", kind)
			}
			cfg.Similarity = SemanticSimilarity{Embedder: emb}
		}
	}
	ing.chunker = kind
	ing.leiden = cfg
	return nil
}

// ChunkerName reports the active chunker, so a run's chunk boundaries can be
// attributed to a strategy.
func (ing *Ingester) ChunkerName() string { return string(ing.chunker) }

// chunkDocument dispatches to the configured chunker.
func (ing *Ingester) chunkDocument(ctx context.Context, blocks []Block) ([]Chunk, error) {
	switch ing.chunker {
	case ChunkerLeidenLexical, ChunkerLeidenSemantic:
		return ChunkLeiden(ctx, blocks, ing.tokens, ing.leiden)
	default:
		return ChunkHeadings(blocks, ing.tokens), nil
	}
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

	// Pass 1: decide what changed, from stat alone.
	//
	// Done as its own pass so the expensive passes can be skipped entirely when
	// nothing changed. Without it, an unchanged re-scan still reads the first 8 KB of
	// every note for aliases — on a 3,000-note vault that is thousands of file opens
	// to establish that there is no work to do.
	known, err := ing.knownSources()
	if err != nil {
		return rep, err
	}
	candidates := make([]noteFile, 0, len(files))
	for _, f := range files {
		if src, ok := known[f.relPath]; ok && src.FileSize == f.size &&
			src.MTime != "" && sameTimestamp(src.MTime, f.modTime) {
			rep.FilesUnchanged++
			rep.FilesSkippedByStat++
			continue
		}
		candidates = append(candidates, f)
	}

	// Pass 2: build the link resolver. Paths come free from the walk; aliases need
	// the frontmatter, so they are only read when there is something to resolve links
	// for. Every note must be registered even so — a changed note may link to an
	// unchanged one.
	resolver := NewLinkResolver()
	readAliasesNow := len(candidates) > 0
	for _, f := range files {
		var aliases []string
		if readAliasesNow {
			aliases = readAliases(f.absPath)
		}
		resolver.AddNote(f.relPath, aliases)
	}

	// Pass 3: ingest the candidates.
	changedRefs := map[string]bool{}
	changedIDs := map[string]bool{}
	for i, f := range candidates {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if onProgress != nil {
			onProgress(i, len(candidates))
		}

		res, err := ing.ingestFile(ctx, f, resolver)
		rep.BytesRead += res.bytesRead
		if err != nil {
			rep.FilesUnreadable++
			slog.Warn("skipping unreadable note", "path", f.relPath, "error", err)
			continue
		}
		if res.unchanged {
			rep.FilesUnchanged++
			rep.FilesSkippedByHash++
			continue
		}
		rep.FilesIngested++
		rep.ChunksWritten += res.chunksWritten
		rep.ChunksDeduped += res.chunksDeduped
		rep.TagsFound += res.tags
		if res.dailyNote {
			rep.DailyNotes++
		}
		changedRefs[f.relPath] = true
		if res.sourceID != "" {
			changedIDs[res.sourceID] = true
		}
		for _, l := range res.links {
			rep.LinksFound++
			if l.ToSourceRef != "" {
				rep.LinksResolved++
				ing.Links = append(ing.Links, l)
			} else {
				rep.LinksUnresolved++
				if len(rep.UnresolvedSample) < 10 {
					rep.UnresolvedSample = append(rep.UnresolvedSample, l.Target)
				}
			}
		}
	}
	if onProgress != nil {
		onProgress(len(candidates), len(candidates))
	}

	// Pass 4: sweep notes that no longer exist.
	//
	// Without this a re-scan is only half incremental: edits are picked up but
	// deletions are not, so memory accumulates notes the user removed and search
	// returns text that is no longer in their vault. Worse than stale — misleading,
	// with nothing to signal it.
	present := make(map[string]bool, len(files))
	for _, f := range files {
		present[f.relPath] = true
	}
	for ref, src := range known {
		if present[ref] || !underRoot(src.Path, root) {
			continue
		}
		if err := ing.store.DeleteSource(src.ID); err != nil {
			return rep, err
		}
		if err := ing.store.DeleteLinksFrom(ref); err != nil {
			return rep, err
		}
		rep.FilesDeleted++
		changedRefs[ref] = true
		slog.Info("removed memory for a note that no longer exists", "ref", ref)
	}

	rep.ChangedRefs = sortedSetKeys(changedRefs)
	rep.ChangedSourceIDs = sortedSetKeys(changedIDs)

	if err := ing.store.MarkStatsDirty(); err != nil {
		return rep, err
	}
	rep.Duration = time.Since(started)
	return rep, nil
}

// knownSources indexes the directory sources already stored, by source_ref.
func (ing *Ingester) knownSources() (map[string]store.MemorySource, error) {
	all, err := ing.store.ListSources()
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.MemorySource, len(all))
	for _, s := range all {
		if s.SourceType == store.SourceDirectory {
			out[s.SourceRef] = s
		}
	}
	return out, nil
}

// underRoot reports whether a stored source's absolute path lies under root.
//
// The deletion sweep must never touch sources belonging to a *different* ingested
// directory: a vault scan that quietly deleted another vault's memory because those
// notes were absent from this walk would be a data-loss bug, not a stale-data one.
func underRoot(path, root string) bool {
	if path == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sortedSetKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
	skippedByStat bool
	bytesRead     int64
	sourceID      string
	chunksWritten int
	chunksDeduped int
	tags          int
	dailyNote     bool
	links         []PendingLink
}

func (ing *Ingester) ingestFile(ctx context.Context, f noteFile, resolver *LinkResolver) (fileResult, error) {
	var res fileResult
	sourceRef := f.relPath

	existing, found, err := ing.store.FindSource(store.SourceDirectory, sourceRef)
	if err != nil {
		return res, err
	}

	// Fast incremental skip: matching mtime *and* size means the file has not been
	// touched, so it is never opened. This is where the incremental win actually
	// comes from — on a vault-scale corpus the cost of a re-scan is dominated by
	// reading and hashing every file, not by the database.
	//
	// mtime alone would be unsafe (some editors and sync tools preserve it) and size
	// alone obviously so; together they are the same check every build tool relies
	// on. The content hash below remains the authority whenever either differs, so a
	// false "changed" costs a read and a false "unchanged" requires both stamps to
	// collide — which is why the pair is used rather than mtime on its own.
	if found && existing.FileSize == f.size && existing.MTime != "" &&
		sameTimestamp(existing.MTime, f.modTime) {
		res.unchanged = true
		res.skippedByStat = true
		res.sourceID = existing.ID
		return res, nil
	}

	raw, err := os.ReadFile(f.absPath)
	if err != nil {
		return res, err
	}
	res.bytesRead = int64(len(raw))
	content := string(raw)
	hash := hashContent(content)

	// The file's stamps moved but its bytes did not — a touch, a sync, or a
	// save-with-no-edit. Refresh the stamps so the next scan takes the fast path,
	// but do not re-chunk: re-chunking would churn chunk ids and therefore every
	// edge touching them, for no change in content.
	if found && existing.ContentHash == hash {
		res.unchanged = true
		res.sourceID = existing.ID
		if err := ing.store.RefreshSourceStamps(existing.ID, f.modTime.Format(time.RFC3339), f.size); err != nil {
			return res, err
		}
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
	chunks, err := ing.chunkDocument(ctx, blocks)
	if err != nil {
		return res, err
	}

	// Links are resolved here, where the vault index is in hand. Edges are
	// written in Phase 6 once both endpoints have chunk ids.
	for _, l := range ExtractLinks(body) {
		pl := PendingLink{
			FromSourceRef: sourceRef, Target: l.Target,
			Heading: l.Heading, Raw: l.Raw, Embed: l.Embed,
		}
		if target, ok := resolver.Resolve(l.Target); ok {
			pl.ToSourceRef = target
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

	sourceID, err := ing.store.ReplaceSource(store.MemorySource{
		SourceType:  store.SourceDirectory,
		SourceRef:   sourceRef,
		Title:       title,
		Path:        f.absPath,
		ContentHash: hash,
		MTime:       f.modTime.Format(time.RFC3339),
		FileSize:    f.size,
	}, storeChunks)
	if err != nil {
		return res, err
	}
	res.sourceID = sourceID
	res.chunksWritten = len(storeChunks)

	// Persist the resolved links. They are needed later to rebuild this note's edges
	// *and* the edges of notes linking to it, neither of which can be recovered from
	// the chunks alone (see store.StoredLink).
	stored := make([]store.StoredLink, 0, len(res.links))
	for _, l := range res.links {
		if l.ToSourceRef == "" {
			continue
		}
		stored = append(stored, store.StoredLink{
			ToRef: l.ToSourceRef, Heading: l.Heading, Raw: l.Raw, Embed: l.Embed,
		})
	}
	if err := ing.store.ReplaceLinksFrom(sourceRef, EdgeLink, stored); err != nil {
		return res, err
	}
	return res, nil
}

// sameTimestamp compares a stored RFC3339 mtime against a file's, to the second.
//
// Truncating to the second is deliberate: the stored value is RFC3339 with no
// sub-second component, so comparing at higher resolution would report every file
// as changed and silently disable the fast path — the exact failure this function
// exists to avoid.
func sameTimestamp(stored string, actual time.Time) bool {
	t, err := time.Parse(time.RFC3339, stored)
	if err != nil {
		return false
	}
	return t.UTC().Truncate(time.Second).Equal(actual.UTC().Truncate(time.Second))
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
// enough to be a duplicate at the ingest threshold.
func isDuplicate(set *NgramSet, others []*NgramSet) bool {
	return isDuplicateAt(set, others, DedupThreshold)
}

// isDuplicateAt is the same test at an explicit threshold. Retrieval uses a looser
// one than ingest: ingest stops outright copies entering the corpus, retrieval stops
// merely-similar passages both being returned.
func isDuplicateAt(set *NgramSet, others []*NgramSet, threshold float64) bool {
	for _, o := range others {
		if set.Dice(o, threshold) >= threshold {
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
