package main

// The text tools of the bash pipelines -- grep, sed, tail, head, sort -rn,
// uniq -c, tr, cut -- as far as the agent uses them, with their C-locale
// byte semantics: L1's unit runs with no LANG.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
)

// eachLine calls fn for every line of r, without its newline; eol says
// whether the line had one (only the last line can lack it).  fn returns
// false to stop.  The slice is only valid during the call.
func eachLine(r io.Reader, fn func(line []byte, eol bool) bool) error {
	br := bufio.NewReaderSize(r, 1<<16)
	var acc []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			acc = append(acc, chunk...)
			continue
		}
		line := chunk
		if acc != nil {
			line = append(acc, chunk...)
			acc = nil
		}
		if len(line) > 0 {
			eol := line[len(line)-1] == '\n'
			if eol {
				line = line[:len(line)-1]
			}
			if !fn(line, eol) {
				return nil
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// prefixStream is sed 's/^/prefix/' (after head -max when max >= 0).  GNU
// sed keeps a missing newline missing, so a stream that ends mid-line runs
// into whatever is printed next, exactly as it did from the bash.
func prefixStream(w io.Writer, prefix string, r io.Reader, max int) {
	if max == 0 {
		return
	}
	n := 0
	eachLine(r, func(l []byte, eol bool) bool {
		b := make([]byte, 0, len(prefix)+len(l)+1)
		b = append(append(b, prefix...), l...)
		if eol {
			b = append(b, '\n')
		}
		w.Write(b)
		n++
		return max < 0 || n < max
	})
}

// writePrefixed prints grep output -- always newline terminated -- through
// sed 's/^/prefix/'.
func writePrefixed(w io.Writer, prefix string, lines []string) {
	for _, l := range lines {
		io.WriteString(w, prefix+l+"\n")
	}
}

// splitLines is what a line-oriented tool sees in b.
func splitLines(b []byte) []string {
	var out []string
	eachLine(bytes.NewReader(b), func(l []byte, _ bool) bool {
		out = append(out, string(l))
		return true
	})
	return out
}

// grepLines keeps the lines keep() accepts.
func grepLines(b []byte, keep func(string) bool) []string {
	var out []string
	for _, l := range splitLines(b) {
		if keep(l) {
			out = append(out, l)
		}
	}
	return out
}

// containsFold is grep -i for a fixed string: ASCII case folding only, as
// in the C locale.
func containsFold(line, lowerNeedle string) bool {
	return strings.Contains(asciiLower(line), lowerNeedle)
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func head(lines []string, n int) []string {
	if len(lines) > n {
		return lines[:n]
	}
	return lines
}

func tail(lines []string, n int) []string {
	if len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}

// tailBytes is tail -n on raw input, which leaves a missing final newline
// missing.
func tailBytes(b []byte, n int) []byte {
	if n <= 0 {
		return nil
	}
	end := len(b)
	if end > 0 && b[end-1] == '\n' {
		end--
	}
	cnt := 0
	for i := end - 1; i >= 0; i-- {
		if b[i] == '\n' {
			cnt++
			if cnt == n {
				return b[i+1:]
			}
		}
	}
	return b
}

// sedRangePlus is sed -n '/re/,+n p': a line that matches starts a range of
// itself and the n lines after it; a match inside a range does not extend
// it.
func sedRangePlus(lines []string, match func(string) bool, n int) []string {
	var out []string
	left := -1
	for _, l := range lines {
		if left >= 0 {
			out = append(out, l)
			left--
			continue
		}
		if match(l) {
			out = append(out, l)
			left = n - 1
		}
	}
	return out
}

// stripNewlines is what $(...) does to a command's output.
func stripNewlines(s string) string { return strings.TrimRight(s, "\n") }

// splitIFS is word splitting of an unquoted expansion with the default IFS.
// Pathname expansion of the words is not done.
func splitIFS(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' })
}

// awkFields is awk's default field splitting ($1..$NF).
func awkFields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' })
}

// field is $i of an awk record: "" past NF.
func field(f []string, i int) string {
	if i >= 1 && i <= len(f) {
		return f[i-1]
	}
	return ""
}

// padRight is printf "%-ns" in mawk, which counts bytes.
func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

// uniqCount is "sort | uniq -c": each distinct line once, behind its count.
func uniqCount(counts map[string]int) []string {
	out := make([]string, 0, len(counts))
	for k, v := range counts {
		out = append(out, fmt.Sprintf("%7d %s", v, k))
	}
	return out
}

// sortRN is sort -rn in the C locale: by the leading number, then -- the
// last-resort comparison, which -r reverses as well -- by the bytes of the
// whole line.  Which order awk's "for (k in n)" produced does not matter.
func sortRN(lines []string) {
	sort.SliceStable(lines, func(i, j int) bool {
		c := numCompare(lines[i], lines[j])
		if c == 0 {
			c = strings.Compare(lines[i], lines[j])
		}
		return c > 0
	})
}

// numKey is the numeric key sort -n reads: leading blanks, an optional
// minus, digits and an optional fraction.  Leading zeros of the integer
// part and trailing zeros of the fraction are dropped.
func numKey(s string) (neg bool, intp, frac string) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i < len(s) && s[i] == '-' {
		neg = true
		i++
	}
	st := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	intp = strings.TrimLeft(s[st:i], "0")
	if i < len(s) && s[i] == '.' {
		i++
		st = i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		frac = strings.TrimRight(s[st:i], "0")
	}
	if intp == "" && frac == "" {
		neg = false
	}
	return
}

func numCompare(a, b string) int {
	an, ai, af := numKey(a)
	bn, bi, bf := numKey(b)
	if an != bn {
		if an {
			return -1
		}
		return 1
	}
	c := 0
	switch {
	case len(ai) != len(bi):
		c = len(ai) - len(bi)
	case ai != bi:
		c = strings.Compare(ai, bi)
	default:
		c = strings.Compare(af, bf)
	}
	if an {
		c = -c
	}
	return c
}

// echoTr is $(echo "$s" | tr , ' ').  echo takes an argument made only of
// -n, -e and -E letters as options, and then prints nothing.
func echoTr(s string) string {
	if len(s) > 1 && s[0] == '-' && strings.Trim(s[1:], "neE") == "" {
		return ""
	}
	return strings.ReplaceAll(s, ",", " ")
}

// cutF2 is cut -d: -f2-: a line without the delimiter passes whole.
func cutF2(line string) string {
	if _, after, ok := strings.Cut(line, ":"); ok {
		return after
	}
	return line
}
