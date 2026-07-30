package store

import (
	"database/sql"
	"math"
	"math/rand"
	"path/filepath"
	"testing"
)

// Phase 0.5 items 3 and 4 from the plan: prove that vector similarity can be
// done in SQL against a bound FLOAT[384] parameter (which is what lets the
// memory system skip an in-Go vector matrix entirely), and determine whether
// DuckDB 1.4's ART index still raises a spurious constraint violation when a
// primary key is deleted and reinserted inside one transaction.

const testDim = 384

func randUnitVec(r *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	var sum float64
	for i := range v {
		v[i] = float32(r.NormFloat64())
		sum += float64(v[i]) * float64(v[i])
	}
	n := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= n
	}
	return v
}

func cosineSim(a, b []float32) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

// TestArrayCosineDistanceInSQL is the load-bearing check for §3.5's candidate
// generation: store FLOAT[384] and rank with array_cosine_distance against a
// bound parameter, with no vector math in Go.
func TestArrayCosineDistanceInSQL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vec.db")
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var version string
	if err := db.QueryRow("SELECT version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("DuckDB %s", version)

	if _, err := db.Exec(`CREATE TABLE chunks (
		id        TEXT PRIMARY KEY,
		embedding FLOAT[384]
	)`); err != nil {
		t.Fatalf("create table with FLOAT[384] column: %v", err)
	}

	r := rand.New(rand.NewSource(42))
	const n = 200
	vecs := make([][]float32, n)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		vecs[i] = randUnitVec(r, testDim)
		ids[i] = string(rune('a'+i%26)) + "-" + itoa(i)
	}

	// Insert via bound parameter — this is the binding path the plan claims
	// go-duckdb v2 supports (TYPE_ARRAY in statement.go).
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO chunks (id, embedding) VALUES (?, ?::FLOAT[384])`)
	if err != nil {
		t.Fatalf("prepare insert with array cast: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := stmt.Exec(ids[i], toAnySlice(vecs[i])); err != nil {
			stmt.Close()
			t.Fatalf("bind FLOAT[384] parameter (row %d): %v", i, err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Round-trip a vector back out to confirm storage fidelity.
	var out any
	if err := db.QueryRow(`SELECT embedding FROM chunks WHERE id = ?`, ids[0]).Scan(&out); err != nil {
		t.Fatalf("scan FLOAT[384] back: %v", err)
	}
	got, ok := out.([]any)
	if !ok {
		t.Fatalf("embedding scanned as %T, want []any", out)
	}
	if len(got) != testDim {
		t.Fatalf("scanned %d dims, want %d", len(got), testDim)
	}
	first, ok := got[0].(float32)
	if !ok {
		t.Fatalf("element type %T, want float32", got[0])
	}
	if math.Abs(float64(first-vecs[0][0])) > 1e-6 {
		t.Errorf("element 0 round-trip: got %v want %v", first, vecs[0][0])
	}

	// Rank against a query vector deliberately built to be closest to vecs[7].
	query := make([]float32, testDim)
	copy(query, vecs[7])
	for i := 0; i < 20; i++ {
		query[i] += 0.05
	}
	var qsum float64
	for _, x := range query {
		qsum += float64(x) * float64(x)
	}
	qn := float32(math.Sqrt(qsum))
	for i := range query {
		query[i] /= qn
	}

	rows, err := db.Query(`
		SELECT id, array_cosine_distance(embedding, ?::FLOAT[384]) AS d
		FROM chunks
		WHERE embedding IS NOT NULL
		ORDER BY d
		LIMIT 5`, toAnySlice(query))
	if err != nil {
		t.Fatalf("array_cosine_distance query: %v", err)
	}
	defer rows.Close()

	var top []scoredID
	for rows.Next() {
		var x scoredID
		if err := rows.Scan(&x.id, &x.d); err != nil {
			t.Fatal(err)
		}
		top = append(top, x)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(top) != 5 {
		t.Fatalf("got %d results, want 5", len(top))
	}
	if top[0].id != ids[7] {
		t.Errorf("nearest = %s, want %s (the perturbed vector's source)", top[0].id, ids[7])
	}

	// Cross-check SQL's distance against a Go cosine computation: SQL should
	// report 1 - cosine_similarity.
	wantD := 1.0 - cosineSim(query, vecs[7])
	if math.Abs(top[0].d-wantD) > 1e-5 {
		t.Errorf("SQL distance %.8f, Go 1-cos %.8f — definition mismatch", top[0].d, wantD)
	}
	t.Logf("nearest=%s d=%.6f (Go 1-cos=%.6f); ordering ascending: %v",
		top[0].id, top[0].d, wantD, ascending(top))
}

type scoredID struct {
	id string
	d  float64
}

func ascending(top []scoredID) bool {
	for i := 1; i < len(top); i++ {
		if top[i].d < top[i-1].d {
			return false
		}
	}
	return true
}

// TestARTDeleteReinsertSameTx determines whether the constraint-violation
// problem documented on SetPlan still exists in DuckDB 1.4. Re-ingestion of a
// changed source is exactly this pattern, so the answer decides whether §3.4's
// two-transaction dance is still required.
func TestARTDeleteReinsertSameTx(t *testing.T) {
	path := filepath.Join(t.TempDir(), "art.db")
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE items (
		src TEXT NOT NULL,
		ord INTEGER NOT NULL,
		body TEXT NOT NULL,
		PRIMARY KEY (src, ord)
	)`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		if _, err := db.Exec(`INSERT INTO items VALUES (?, ?, ?)`, "s1", i, "v1"); err != nil {
			t.Fatal(err)
		}
	}

	// Delete then reinsert the identical keys inside ONE transaction.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DELETE FROM items WHERE src = ?`, "s1"); err != nil {
		tx.Rollback()
		t.Fatalf("delete: %v", err)
	}
	var reinsertErr error
	for i := 1; i <= 5; i++ {
		if _, err := tx.Exec(`INSERT INTO items VALUES (?, ?, ?)`, "s1", i, "v2"); err != nil {
			reinsertErr = err
			break
		}
	}
	if reinsertErr != nil {
		tx.Rollback()
		t.Logf("STILL BROKEN in this version: delete+reinsert same PK in one tx -> %v", reinsertErr)
		t.Log("=> §3.4 must keep deletes and inserts in separate transactions")
		return
	}
	if err := tx.Commit(); err != nil {
		t.Logf("STILL BROKEN at commit: %v", err)
		t.Log("=> §3.4 must keep deletes and inserts in separate transactions")
		return
	}

	var n int
	var body string
	if err := db.QueryRow(`SELECT COUNT(*), MIN(body) FROM items WHERE src = ?`, "s1").Scan(&n, &body); err != nil {
		t.Fatal(err)
	}
	if n != 5 || body != "v2" {
		t.Errorf("after delete+reinsert: count=%d body=%q, want 5 and v2", n, body)
	}
	t.Log("FIXED in this version: delete+reinsert of the same primary key inside one transaction works")
	t.Log("=> §3.4's re-ingest path may use a single transaction")
}

func toAnySlice(v []float32) []any {
	out := make([]any, len(v))
	for i, x := range v {
		out[i] = x
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
