package memory

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
	"simple-cot-chat/store"
)

// openTestStore builds a store against a temp database. It uses the exported
// Open-equivalent path rather than reaching into store internals.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory.db")
	s, err := store.OpenAt(path)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// writeVault materializes a synthetic Obsidian vault covering the shapes §3.3
// calls out: wikilinks (with headings, aliases, duplicate basenames), tags,
// frontmatter lists, a daily note, an embed, and a .obsidian directory that must
// be skipped.
func writeVault(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	files := map[string]string{
		"Architecture.md": `---
title: Architecture
tags: [design, core]
aliases:
  - Arch
---

# Architecture

The system stores sessions in an embedded DuckDB database. See [[Data Model]] for
the schema and [[Retrieval#Fusion]] for scoring.

## Notes

Tagged #design and #core/storage here.

` + "```go\nfunc Open() (*Store, error) {\n\t// # not a heading\n\treturn nil, nil\n}\n```" + `

| Table | Purpose |
|-------|---------|
| chunks | retrievable spans |
| edges  | graph structure |
`,

		"Data Model.md": `# Data Model

Chunks carry a heading path and an optional embedding. Linked from
[[Architecture]] and referenced by [[Arch]] using its alias.

An embed follows: ![[Retrieval]]
`,

		"Retrieval.md": `# Retrieval

## Fusion

Four signals are combined: BM25, vector cosine, entity overlap, and character
n-grams.

## Graph walk

Expansion goes beyond what direct scoring finds.
`,

		"daily/2026-07-28.md": `# 2026-07-28

Worked on the memory system today. Linked to [[Architecture]].
`,

		"a/Duplicate.md": "# Duplicate A\n\nFirst copy lives in folder a.\n",
		"b/Duplicate.md": "# Duplicate B\n\nSecond copy lives in folder b.\n",

		"Broken Links.md": `# Broken Links

This references [[No Such Note]] and the ambiguous [[Duplicate]].
`,

		// Must be skipped entirely.
		".obsidian/workspace.json":   `{"main":{"id":"x"}}`,
		".obsidian/plugins/notes.md": "# Plugin note that must not be ingested\n\nNoise.\n",
		".trash/Deleted.md":          "# Deleted\n\nShould not be ingested.\n",
		"attachments/image.png":      "\x89PNG\r\n\x1a\n binary",
	}

	for rel, content := range files {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestIngestVault(t *testing.T) {
	s := openTestStore(t)
	root := writeVault(t)
	ing := NewIngester(s, NewHeuristicCounter())

	rep, err := ing.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatalf("IngestDirectory: %v", err)
	}
	t.Log(rep.String())

	// 7 markdown files outside skipped directories: Architecture, Data Model,
	// Retrieval, Broken Links, daily/2026-07-28, a/Duplicate, b/Duplicate.
	// The .obsidian and .trash notes, the JSON, and the PNG must all be absent.
	const wantFiles = 7
	if rep.FilesSeen != wantFiles {
		t.Errorf("FilesSeen = %d, want %d (skipped dirs must not be walked)", rep.FilesSeen, wantFiles)
	}
	if rep.FilesIngested != wantFiles {
		t.Errorf("FilesIngested = %d, want %d", rep.FilesIngested, wantFiles)
	}
	if rep.ChunksWritten == 0 {
		t.Fatal("no chunks written")
	}
	if rep.DailyNotes != 1 {
		t.Errorf("DailyNotes = %d, want 1", rep.DailyNotes)
	}

	// .obsidian and .trash content must be absent.
	for _, ref := range []string{
		".obsidian/plugins/notes.md", ".trash/Deleted.md", "attachments/image.png",
	} {
		if _, found, _ := s.FindSource(store.SourceDirectory, ref); found {
			t.Errorf("%s was ingested but should have been skipped", ref)
		}
	}

	// Heading paths made it through to storage.
	src, found, err := s.FindSource(store.SourceDirectory, "Retrieval.md")
	if err != nil || !found {
		t.Fatalf("Retrieval.md not ingested: found=%v err=%v", found, err)
	}
	chunks, err := s.GetChunksBySource(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, c := range chunks {
		paths[c.HeadingPath] = true
	}
	if !paths["Retrieval › Fusion"] {
		t.Errorf("expected heading path 'Retrieval › Fusion', got %v", keys(paths))
	}

	// Code fence survived ingestion intact.
	archSrc, _, _ := s.FindSource(store.SourceDirectory, "Architecture.md")
	archChunks, _ := s.GetChunksBySource(archSrc.ID)
	var sawFence, sawTable bool
	for _, c := range archChunks {
		if strings.Contains(c.Text, "func Open()") {
			sawFence = true
			if strings.Count(c.Text, "```") != 2 {
				t.Errorf("code fence not intact in stored chunk: %q", c.Text)
			}
			if !strings.Contains(c.Text, "# not a heading") {
				t.Error("code comment lost")
			}
		}
		if strings.Contains(c.Text, "| chunks | retrievable spans |") {
			sawTable = true
			if !strings.Contains(c.Text, "| edges  | graph structure |") {
				t.Error("table rows split across chunks")
			}
		}
	}
	if !sawFence {
		t.Error("code block missing from stored chunks")
	}
	if !sawTable {
		t.Error("table missing from stored chunks")
	}

	// Title comes from frontmatter.
	if archSrc.Title != "Architecture" {
		t.Errorf("title = %q, want Architecture", archSrc.Title)
	}

	// Tags became entities.
	var tagCount int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM memory_entities WHERE kind = 'tag'`).Scan(&tagCount); err != nil {
		t.Fatal(err)
	}
	if tagCount == 0 {
		t.Error("no tag entities created")
	}

	// Link resolution: aliases and heading targets resolve, the ambiguous and
	// missing ones do not — and unresolved links are counted, not dropped.
	if rep.LinksFound == 0 {
		t.Fatal("no links found")
	}
	if rep.LinksResolved == 0 {
		t.Error("no links resolved")
	}
	if rep.LinksUnresolved < 2 {
		t.Errorf("LinksUnresolved = %d, want >= 2 (No Such Note + ambiguous Duplicate)", rep.LinksUnresolved)
	}
	if len(rep.UnresolvedSample) == 0 {
		t.Error("unresolved links were not sampled for reporting")
	}
	if rate := rep.UnresolvedRate(); rate > 0.5 {
		t.Errorf("unresolved rate = %.2f, unexpectedly high", rate)
	}
	// The alias link resolved to the same note as the direct link.
	var toArch int
	for _, l := range ing.Links {
		if l.ToSourceRef == "Architecture.md" {
			toArch++
		}
	}
	if toArch < 2 {
		t.Errorf("expected >= 2 resolved links to Architecture.md (direct + alias), got %d", toArch)
	}
}

// TestIngestIncremental is the property that makes vault-scale re-scanning viable.
func TestIngestIncremental(t *testing.T) {
	s := openTestStore(t)
	root := writeVault(t)
	ing := NewIngester(s, NewHeuristicCounter())

	first, err := ing.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.FilesIngested == 0 {
		t.Fatal("first run ingested nothing")
	}

	second, err := ing.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.FilesIngested != 0 {
		t.Errorf("re-scan ingested %d files, want 0 (unchanged content must be a no-op)", second.FilesIngested)
	}
	if second.FilesUnchanged != first.FilesIngested {
		t.Errorf("FilesUnchanged = %d, want %d", second.FilesUnchanged, first.FilesIngested)
	}

	// Edit one note; only that one re-ingests.
	target := filepath.Join(root, "Retrieval.md")
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, append(body, []byte("\n## New Section\n\nAdded content here.\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	third, err := ing.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if third.FilesIngested != 1 {
		t.Errorf("after editing one file, ingested %d, want 1", third.FilesIngested)
	}
	if third.FilesUnchanged != first.FilesIngested-1 {
		t.Errorf("FilesUnchanged = %d, want %d", third.FilesUnchanged, first.FilesIngested-1)
	}

	// Total sources stays constant: re-ingest replaces, never duplicates.
	st, err := s.MemoryStats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Sources != first.FilesIngested {
		t.Errorf("sources = %d, want %d — re-ingest duplicated a source", st.Sources, first.FilesIngested)
	}
}

// TestIngestDedupesRepeatedPassage is a Phase 2 exit criterion: the same passage
// ingested twice yields one chunk, so corpus statistics stay honest.
func TestIngestDedupesRepeatedPassage(t *testing.T) {
	s := openTestStore(t)
	root := t.TempDir()
	ing := NewIngester(s, NewHeuristicCounter())

	passage := "The ingestion pipeline rejects near-duplicate chunks at write time so that " +
		"document frequency counts stay honest and per-source diversity is not defeated by clones. " +
		"This paragraph is deliberately long enough to be a chunk on its own merits."

	// Two different files containing the identical passage.
	if err := os.WriteFile(filepath.Join(root, "one.md"), []byte("# One\n\n"+passage+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "two.md"), []byte("# Two\n\n"+passage+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := ing.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.String())

	if rep.ChunksDeduped < 1 {
		t.Errorf("ChunksDeduped = %d, want >= 1 (the repeated passage)", rep.ChunksDeduped)
	}

	// Count chunks whose text contains the passage: exactly one survives.
	var n int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM memory_chunks WHERE text LIKE '%rejects near-duplicate chunks%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d chunks contain the repeated passage, want 1", n)
	}
}

// TestIngestRepoDocs runs the pipeline over the repository's own Markdown, which
// is real prose with real code fences and tables — a harder fixture than anything
// synthetic.
func TestIngestRepoDocs(t *testing.T) {
	s := openTestStore(t)
	ing := NewIngester(s, NewHeuristicCounter())

	root := ".."
	if _, err := os.Stat(filepath.Join(root, "ARCHITECTURE.md")); err != nil {
		t.Skip("repo docs not found")
	}

	rep, err := ing.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatalf("IngestDirectory: %v", err)
	}
	t.Log(rep.String())

	if rep.FilesIngested < 3 {
		t.Errorf("ingested %d files, expected at least ARCHITECTURE/README/UI", rep.FilesIngested)
	}
	if rep.ChunksWritten < 10 {
		t.Errorf("chunks = %d, expected more from the repo docs", rep.ChunksWritten)
	}

	// Chunking must not INTRODUCE an unclosed fence. Comparing against the source
	// documents matters: a hand-written document can itself contain an unclosed
	// fence (a prose line that happens to begin with the fence sequence is one per
	// CommonMark), and that is a defect in the document, not in the chunker. The
	// invariant is that chunking adds none of its own.
	rows, err := s.DB().Query(`SELECT id, text FROM memory_chunks`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	chunkOpen := 0
	total := 0
	var samples []string
	for rows.Next() {
		var id, text string
		if err := rows.Scan(&id, &text); err != nil {
			t.Fatal(err)
		}
		total++
		if n := unclosedFences(text); n != 0 {
			chunkOpen += n
			if len(samples) < 3 {
				samples = append(samples, id+": "+truncStr(text, 200))
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// How many unclosed fences the source documents themselves contain.
	sourceOpen := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !markdownExts[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		_, body := SplitFrontmatter(string(b))
		sourceOpen += unclosedFences(body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("checked %d chunks: %d unclosed fences in chunks, %d in the source documents",
		total, chunkOpen, sourceOpen)
	if chunkOpen > sourceOpen {
		t.Errorf("chunking introduced %d unclosed fence(s) beyond the %d already in the "+
			"source documents:\n%s", chunkOpen-sourceOpen, sourceOpen, strings.Join(samples, "\n"))
	}

	// Sanity: heading paths are populated for most chunks.
	var withPath int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM memory_chunks WHERE heading_path <> ''`).Scan(&withPath); err != nil {
		t.Fatal(err)
	}
	if withPath == 0 {
		t.Error("no chunk carries a heading path")
	}
	t.Logf("%d/%d chunks carry a heading path", withPath, total)
}

func TestIngestContextCancellation(t *testing.T) {
	s := openTestStore(t)
	root := writeVault(t)
	ing := NewIngester(s, NewHeuristicCounter())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the run starts

	_, err := ing.IngestDirectory(ctx, root, nil)
	if err == nil {
		t.Error("expected cancellation error")
	}
}

func TestIngestProgressCallback(t *testing.T) {
	s := openTestStore(t)
	root := writeVault(t)
	ing := NewIngester(s, NewHeuristicCounter())

	var calls int
	var lastDone, lastTotal int
	_, err := ing.IngestDirectory(context.Background(), root, func(done, total int) {
		calls++
		lastDone, lastTotal = done, total
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Error("progress callback never invoked")
	}
	if lastDone != lastTotal || lastTotal == 0 {
		t.Errorf("final progress = %d/%d, want done == total > 0", lastDone, lastTotal)
	}
}

func TestIngestSkipsOversizedFile(t *testing.T) {
	s := openTestStore(t)
	root := t.TempDir()
	big := strings.Repeat("word ", (MaxFileBytes/5)+100)
	if err := os.WriteFile(filepath.Join(root, "huge.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "small.md"), []byte("# Small\n\nFine.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ing := NewIngester(s, NewHeuristicCounter())
	rep, err := ing.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.FilesTooLarge != 1 {
		t.Errorf("FilesTooLarge = %d, want 1 — skips must be counted, not silent", rep.FilesTooLarge)
	}
	if rep.FilesIngested != 1 {
		t.Errorf("FilesIngested = %d, want 1", rep.FilesIngested)
	}
}

// TestIngestWithExactTokenizer runs the pipeline with the real BERT counter when
// tokenizer.json is available, since chunk boundaries depend on it.
func TestIngestWithExactTokenizer(t *testing.T) {
	p := TokenizerPath()
	if p == "" {
		t.Skip("tokenizer.json not available")
	}
	tc, err := NewBERTCounter(p)
	if err != nil {
		t.Fatalf("NewBERTCounter: %v", err)
	}
	if tc.Name() != "bert-wordpiece" {
		t.Errorf("counter name = %q", tc.Name())
	}

	// The exact counter must agree with the heuristic to within a sane factor;
	// a wild divergence means one of them is broken.
	h := NewHeuristicCounter()
	for _, s := range []string{
		"The memory system ingests conversations and artifacts.",
		"antidisestablishmentarianism unbelievability",
		"Café naïve résumé",
	} {
		exact, approx := tc.Count(s), h.Count(s)
		if exact == 0 {
			t.Errorf("exact count of %q is 0", s)
		}
		ratio := float64(approx) / float64(exact)
		if ratio < 0.4 || ratio > 3.0 {
			t.Errorf("counters diverge wildly for %q: exact=%d heuristic=%d", s, exact, approx)
		}
	}

	s := openTestStore(t)
	root := writeVault(t)
	ing := NewIngester(s, tc)
	rep, err := ing.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("with exact tokenizer: %s", rep.String())
	if rep.ChunksWritten == 0 {
		t.Error("no chunks written with exact tokenizer")
	}
}
