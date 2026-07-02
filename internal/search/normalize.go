package search

import (
	"regexp/syntax"
	"sort"
	"unicode"

	"github.com/sourcegraph/zoekt/query"
)

const normalizedRegexpFlags = syntax.ClassNL | syntax.PerlX | syntax.UnicodeGroups

// normalizeQuery makes content/file search atoms accent-insensitive. Zoekt
// already handles case folding when the query is parsed with case:no; this pass
// covers examples like "clém" matching "clem" (and vice versa) by expanding
// Latin letters to small accent-equivalent character classes.
func normalizeQuery(q query.Q) query.Q {
	return query.Map(q, func(q query.Q) query.Q {
		switch s := q.(type) {
		case *query.Substring:
			return normalizeSubstring(s)
		case *query.Regexp:
			return normalizeRegexpQuery(s)
		case *query.Symbol:
			return &query.Symbol{Expr: normalizeQuery(s.Expr)}
		default:
			return q
		}
	})
}

func normalizeSubstring(q *query.Substring) query.Q {
	r, changed := normalizeLiteralRunes([]rune(q.Pattern), q.CaseSensitive)
	if !changed {
		return q
	}
	return &query.Regexp{
		Regexp:        query.OptimizeRegexp(r, normalizedRegexpFlags),
		FileName:      q.FileName,
		Content:       q.Content,
		CaseSensitive: q.CaseSensitive,
	}
}

func normalizeRegexpQuery(q *query.Regexp) query.Q {
	r, changed := normalizeRegexp(q.Regexp, q.CaseSensitive)
	if !changed {
		return q
	}
	copy := *q
	copy.Regexp = query.OptimizeRegexp(r, normalizedRegexpFlags)
	return &copy
}

func normalizeRegexp(r *syntax.Regexp, caseSensitive bool) (*syntax.Regexp, bool) {
	if r == nil {
		return r, false
	}
	copy := *r
	switch r.Op {
	case syntax.OpLiteral:
		return normalizeLiteralRunes(r.Rune, caseSensitive)
	case syntax.OpCharClass:
		classes, changed := normalizeCharClass(r.Rune, caseSensitive)
		if !changed {
			return r, false
		}
		copy.Rune = classes
		return &copy, true
	default:
		var changed bool
		if len(r.Sub) > 0 {
			copy.Sub = make([]*syntax.Regexp, len(r.Sub))
			for i, sub := range r.Sub {
				normalized, subChanged := normalizeRegexp(sub, caseSensitive)
				copy.Sub[i] = normalized
				changed = changed || subChanged
			}
		}
		if !changed {
			return r, false
		}
		return &copy, true
	}
}

func normalizeLiteralRunes(runes []rune, caseSensitive bool) (*syntax.Regexp, bool) {
	var sub []*syntax.Regexp
	var literal []rune
	changed := false

	flushLiteral := func() {
		if len(literal) == 0 {
			return
		}
		sub = append(sub, &syntax.Regexp{Op: syntax.OpLiteral, Rune: append([]rune(nil), literal...)})
		literal = literal[:0]
	}

	for _, r := range runes {
		class, ok := accentClassForQueryRune(r, caseSensitive)
		if !ok {
			literal = append(literal, r)
			continue
		}
		changed = true
		flushLiteral()
		sub = append(sub, &syntax.Regexp{Op: syntax.OpCharClass, Rune: runeClass(class)})
	}
	if !changed {
		return &syntax.Regexp{Op: syntax.OpLiteral, Rune: append([]rune(nil), runes...)}, false
	}
	flushLiteral()
	if len(sub) == 1 {
		return sub[0], true
	}
	return &syntax.Regexp{Op: syntax.OpConcat, Sub: sub}, true
}

func normalizeCharClass(ranges []rune, caseSensitive bool) ([]rune, bool) {
	var out []rune
	changed := false
	for i := 0; i+1 < len(ranges); i += 2 {
		lo, hi := ranges[i], ranges[i+1]
		out = append(out, lo, hi)
		// Keep very broad classes such as \pL compact. Accent expansion matters
		// for explicit accented alternatives like [éè], not for huge ranges.
		if hi-lo > 256 {
			continue
		}
		for r := lo; r <= hi; r++ {
			class, ok := accentClassForQueryRune(r, caseSensitive)
			if !ok {
				continue
			}
			changed = true
			for _, c := range class {
				out = append(out, c, c)
			}
		}
	}
	if !changed {
		return ranges, false
	}
	return runeClassFromRanges(out), true
}

func runeClass(runes []rune) []rune {
	pairs := make([]rune, 0, len(runes)*2)
	for _, r := range runes {
		pairs = append(pairs, r, r)
	}
	return runeClassFromRanges(pairs)
}

func runeClassFromRanges(pairs []rune) []rune {
	if len(pairs) == 0 {
		return nil
	}
	type rr struct{ lo, hi rune }
	ranges := make([]rr, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		lo, hi := pairs[i], pairs[i+1]
		if hi < lo {
			lo, hi = hi, lo
		}
		ranges = append(ranges, rr{lo: lo, hi: hi})
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].lo == ranges[j].lo {
			return ranges[i].hi < ranges[j].hi
		}
		return ranges[i].lo < ranges[j].lo
	})
	merged := ranges[:0]
	for _, r := range ranges {
		if len(merged) == 0 || r.lo > merged[len(merged)-1].hi+1 {
			merged = append(merged, r)
			continue
		}
		if r.hi > merged[len(merged)-1].hi {
			merged[len(merged)-1].hi = r.hi
		}
	}
	out := make([]rune, 0, len(merged)*2)
	for _, r := range merged {
		out = append(out, r.lo, r.hi)
	}
	return out
}

func accentClassForQueryRune(r rune, caseSensitive bool) ([]rune, bool) {
	lower := unicode.ToLower(r)
	group, ok := latinAccentGroup(lower)
	if !ok {
		return nil, false
	}
	if caseSensitive && isUpper(r) {
		return upperRunes(group), true
	}
	return group, true
}

func isUpper(r rune) bool {
	return unicode.ToUpper(r) == r && unicode.ToLower(r) != r
}

func upperRunes(in []rune) []rune {
	out := make([]rune, 0, len(in))
	for _, r := range in {
		out = append(out, unicode.ToUpper(r))
	}
	return out
}

func latinAccentGroup(r rune) ([]rune, bool) {
	for _, group := range latinAccentGroups {
		for _, candidate := range group {
			if candidate == r {
				return group, true
			}
		}
	}
	return nil, false
}

var latinAccentGroups = [][]rune{
	[]rune("aàáâãäåāăąǎǟǡǻȁȃȧạảấầẩẫậắằẳẵặæǽǣ"),
	[]rune("cçćĉċč"),
	[]rune("dďđð"),
	[]rune("eèéêëēĕėęěȅȇẹẻẽếềểễệ"),
	[]rune("gĝğġģ"),
	[]rune("hĥħ"),
	[]rune("iìíîïĩīĭįıǐȉȋịỉ"),
	[]rune("jĵ"),
	[]rune("kķĸ"),
	[]rune("lĺļľŀł"),
	[]rune("nñńņňŉŋ"),
	[]rune("oòóôõöøōŏőơǒǫǭȍȏȯọỏốồổỗộớờởỡợœ"),
	[]rune("rŕŗř"),
	[]rune("sśŝşšșß"),
	[]rune("tţťŧț"),
	[]rune("uùúûüũūŭůűųưǔȕȗụủứừửữự"),
	[]rune("wŵẁẃẅ"),
	[]rune("yýÿŷỳỵỷỹ"),
	[]rune("zźżž"),
}
