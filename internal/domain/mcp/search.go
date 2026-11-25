package mcp

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// Search limits (PRD §6.2, Appendix B mcp.search_limit_max).
const (
	DefaultSearchLimit = 8
	MaxSearchLimit     = 25
)

// BM25 parameters and field weights.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

var fieldWeights = [...]float64{3, 2, 1, 1} // name, title, description, params

// Index is an in-memory BM25 index over a team's primitives. Build it once per refresh; it is safe
// for concurrent readers once built.
type Index struct {
	docs   []indexDoc
	df     map[string]int
	avgLen float64
}

type indexDoc struct {
	p   Primitive
	tf  map[string]float64 // weighted term frequency
	len float64
}

// NewIndex builds an index. Primitives are sorted by qualified name so results are deterministic.
func NewIndex(prims []Primitive) *Index {
	ps := append([]Primitive(nil), prims...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].QualifiedName() < ps[j].QualifiedName() })
	ix := &Index{df: map[string]int{}}
	total := 0.0
	for _, p := range ps {
		d := indexDoc{p: p, tf: map[string]float64{}}
		fields := [...]string{p.Name, p.Title, p.Description, strings.Join(p.Params, " ")}
		for i, f := range fields {
			for _, t := range Tokenize(f) {
				d.tf[t] += fieldWeights[i]
				d.len += fieldWeights[i]
			}
		}
		for t := range d.tf {
			ix.df[t]++
		}
		total += d.len
		ix.docs = append(ix.docs, d)
	}
	if len(ix.docs) > 0 {
		ix.avgLen = total / float64(len(ix.docs))
	}
	return ix
}

// Len is the number of indexed primitives.
func (ix *Index) Len() int { return len(ix.docs) }

// Query is an afs_search request after the gateway has resolved the agent's allowed set.
type Query struct {
	Text    string
	Servers []string             // empty means all
	Kinds   []Kind               // empty means all; "resource" also matches resource templates
	Limit   int                  // 0 means DefaultSearchLimit; capped at MaxSearchLimit
	Allowed func(Primitive) bool // the agent's effective policy; nil allows all
	Boost   map[string]float64   // per-server multiplier from the agent's recent use, >= 1
}

// Hit is one search result. It carries no schema; afs_describe returns those.
type Hit struct {
	Primitive Primitive
	Score     float64
}

// Search ranks primitives for q. An empty query returns no hits. Ties break by qualified name.
func (ix *Index) Search(q Query) []Hit {
	terms := Tokenize(q.Text)
	if len(terms) == 0 {
		return nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}
	servers := toSet(q.Servers)
	n := float64(len(ix.docs))
	var hits []Hit
	for _, d := range ix.docs {
		if len(servers) > 0 && !servers[d.p.Server] || !kindIn(d.p.Kind, q.Kinds) ||
			q.Allowed != nil && !q.Allowed(d.p) {
			continue
		}
		score := 0.0
		for _, t := range terms {
			tf := d.tf[t]
			if tf == 0 {
				continue
			}
			df := float64(ix.df[t])
			idf := math.Log(1 + (n-df+0.5)/(df+0.5))
			score += idf * tf * (bm25K1 + 1) / (tf + bm25K1*(1-bm25B+bm25B*d.len/ix.avgLen))
		}
		if score == 0 {
			continue
		}
		if b := q.Boost[d.p.Server]; b > 1 {
			score *= b
		}
		hits = append(hits, Hit{Primitive: d.p, Score: score})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func kindIn(k Kind, ks []Kind) bool {
	if len(ks) == 0 {
		return true
	}
	for _, x := range ks {
		if x == k || x == KindResource && k == KindResourceTemplate {
			return true
		}
	}
	return false
}

// Tokenize lower-cases s and splits it into terms on non-alphanumerics, snake_case, kebab-case and
// camelCase boundaries. Single characters and a few stop words are dropped; a trailing "s" is
// stripped from words longer than three characters so "issues" matches "issue".
func Tokenize(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 1 {
			w := string(cur)
			if !stopWords[w] {
				if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
					w = w[:len(w)-1]
				}
				out = append(out, w)
			}
		}
		cur = cur[:0]
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case unicode.IsUpper(r):
			// "createPR" -> create, pr; "HTTPServer" -> http, server
			if len(cur) > 0 && (unicode.IsLower(rs[i-1]) || i+1 < len(rs) && unicode.IsLower(rs[i+1]) && unicode.IsUpper(rs[i-1])) {
				flush()
			}
			cur = append(cur, unicode.ToLower(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

var stopWords = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "to": true, "in": true, "on": true, "for": true,
	"and": true, "or": true, "is": true, "it": true, "by": true, "with": true, "from": true, "this": true,
	"that": true, "be": true, "as": true, "at": true,
}
