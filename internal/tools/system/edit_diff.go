package system

import (
	"fmt"
	"strings"
)

// Compact unified-style diff used in edit_file results, so the agent can
// verify what changed without spending a read_file round-trip.

const (
	diffContext  = 3
	diffMaxD     = 500  // Myers edit-distance cap; beyond it we show a block replace
	diffMaxBytes = 6000 // cap on rendered diff size
	diffLineMax  = 200
)

type diffOp struct {
	kind byte // ' ' equal, '-' delete, '+' insert
	a, b int  // positions in old / new line slices
}

// diffLineOps returns ops covering every change plus up to diffContext equal
// lines at the edges. Common prefix/suffix are trimmed first so cost scales
// with the size of the change, not the file.
func diffLineOps(a, b []string) []diffOp {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	ma, mb := a[p:len(a)-s], b[p:len(b)-s]

	var ops []diffOp
	for i := max(0, p-diffContext); i < p; i++ {
		ops = append(ops, diffOp{' ', i, i})
	}
	for _, op := range myersOps(ma, mb) {
		op.a += p
		op.b += p
		ops = append(ops, op)
	}
	for i := 0; i < s && i < diffContext; i++ {
		ops = append(ops, diffOp{' ', len(a) - s + i, len(b) - s + i})
	}
	return ops
}

func blockOps(n, m int) []diffOp {
	ops := make([]diffOp, 0, n+m)
	for i := 0; i < n; i++ {
		ops = append(ops, diffOp{'-', i, 0})
	}
	for j := 0; j < m; j++ {
		ops = append(ops, diffOp{'+', n, j})
	}
	return ops
}

// myersOps is the classic O(ND) Myers diff with backtracking.
func myersOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		return blockOps(n, m)
	}
	maxD := n + m
	if maxD > diffMaxD {
		maxD = diffMaxD
	}
	off := maxD + 1
	v := make([]int, 2*maxD+3)
	var trace [][]int
	found := false
	for d := 0; d <= maxD && !found; d++ {
		snap := make([]int, len(v))
		copy(snap, v)
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				found = true
				break
			}
		}
	}
	if !found {
		return blockOps(n, m)
	}

	var rev []diffOp
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		vv := trace[d]
		k := x - y
		var pk int
		if k == -d || (k != d && vv[off+k-1] < vv[off+k+1]) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := vv[off+pk]
		py := px - pk
		for x > px && y > py {
			rev = append(rev, diffOp{' ', x - 1, y - 1})
			x--
			y--
		}
		if d > 0 {
			if x == px {
				rev = append(rev, diffOp{'+', x, y - 1})
			} else {
				rev = append(rev, diffOp{'-', x - 1, y})
			}
		}
		x, y = px, py
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// renderDiff returns a unified-style diff (with +/- counts) of two texts.
func renderDiff(oldText, newText string) (diff string, added, removed int) {
	a := strings.Split(oldText, "\n")
	b := strings.Split(newText, "\n")
	if len(a) > 1 && len(b) > 1 && a[len(a)-1] == "" && b[len(b)-1] == "" {
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	ops := diffLineOps(a, b)
	for _, op := range ops {
		switch op.kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	if added == 0 && removed == 0 {
		return "", 0, 0
	}

	type span struct{ lo, hi int }
	var spans []span
	for i, op := range ops {
		if op.kind == ' ' {
			continue
		}
		lo, hi := max(0, i-diffContext), min(len(ops)-1, i+diffContext)
		if n := len(spans); n > 0 && lo <= spans[n-1].hi+1 {
			if hi > spans[n-1].hi {
				spans[n-1].hi = hi
			}
		} else {
			spans = append(spans, span{lo, hi})
		}
	}

	var out strings.Builder
	for si, sp := range spans {
		oldN, newN := 0, 0
		for _, op := range ops[sp.lo : sp.hi+1] {
			if op.kind != '+' {
				oldN++
			}
			if op.kind != '-' {
				newN++
			}
		}
		var h strings.Builder
		fmt.Fprintf(&h, "@@ -%d,%d +%d,%d @@\n", ops[sp.lo].a+1, oldN, ops[sp.lo].b+1, newN)
		for _, op := range ops[sp.lo : sp.hi+1] {
			switch op.kind {
			case ' ':
				h.WriteString(" " + clipLine(a[op.a], diffLineMax) + "\n")
			case '-':
				h.WriteString("-" + clipLine(a[op.a], diffLineMax) + "\n")
			case '+':
				h.WriteString("+" + clipLine(b[op.b], diffLineMax) + "\n")
			}
		}
		if out.Len()+h.Len() > diffMaxBytes {
			fmt.Fprintf(&out, "… diff truncated (%d more hunk(s))\n", len(spans)-si)
			break
		}
		out.WriteString(h.String())
	}
	return strings.TrimRight(out.String(), "\n"), added, removed
}
