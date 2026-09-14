package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// A battery is a file of lines, one item per line:
//
//	# comment
//	set <key>=<value> ...          defaults for every item after it
//
// ${VAR} and ${VAR:-default} are taken from the environment.
//
// A value with spaces is written in double quotes: reason="without KPTI".
//
//	run    <name> <key>=<value> ...  boots, checked against expectations
//	matrix <name> <key>=<value> ...  runs, reduced to medians per metric
//	ab     <name> <key>=<value> ...  two variants (a.* / b.*), alternating
//	kernel <name> git=<dir> rev=<rev> [host=timing|stats]
//	                               a host+guest image set built from that
//	                               revision (scripts/build-ref.sh), which
//	                               items select with kernel=<name>
//
// Keys (a value runs to the next space; lists are comma separated):
//
//	suite       smoke|default|full|security|perf|hosttests|profile|failclosed
//	cases       exact case names, one boot for all of them
//	vendor      pvm|intel, or a list
//	cpus        guest vCPU counts, one boot each
//	reps        repetitions
//	host        stats|timing: the host build the item needs
//	kernel      an image set declared with "kernel"; default: out/images
//	l1          kvm|tcg|tcg-la57
//	pti         on: boot L1 with pti=on
//	guest       extra guest kernel arguments
//	mod         kvm module arguments
//	l1append    extra L1 kernel arguments
//	stats       on: bracket every case with counter snapshots
//	profile     case to profile (suite profile)
//	allow-fail  case names expected to fail in this configuration
//	expect      pass (default) | refuse-load
//	reason      (refuse-load) regexp the log must match: why it refused
//	timeout     seconds for the whole boot
//	baseline    (matrix) auto (baselines/<machine>.tsv), a path, or none
//	threshold   (matrix) percent worse than the baseline that fails (15)
//	a.<key>, b.<key>   (ab only) what the two variants set
type Item struct {
	Kind string // run, matrix, ab
	Name string
	Line int
	Keys map[string]string
}

type Battery struct {
	Path    string
	Text    string
	Items   []Item
	Kernels []Kernel
}

type Kernel struct {
	Name, Git, Rev, Host string
	Line                 int
}

var knownKeys = map[string]bool{
	"suite": true, "cases": true, "vendor": true, "cpus": true, "reps": true,
	"host": true, "l1": true, "pti": true, "guest": true, "mod": true,
	"l1append": true, "stats": true, "profile": true, "allow-fail": true,
	"expect": true, "timeout": true, "baseline": true, "threshold": true, "reason": true, "kernel": true,
}

func ParseBattery(path string) (*Battery, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b := &Battery{Path: path, Text: string(raw)}
	defaults := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	n := 0
	for sc.Scan() {
		n++
		f, err := fields(expandEnv(sc.Text()))
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if len(f) == 0 {
			continue
		}
		kind := f[0]
		switch kind {
		case "set":
			kv, err := parseKeys(f[1:], true)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, n, err)
			}
			for k, v := range kv {
				defaults[k] = v
			}
		case "kernel":
			if len(f) < 2 {
				return nil, fmt.Errorf("%s:%d: kernel needs a name", path, n)
			}
			k := Kernel{Name: f[1], Host: "timing", Line: n}
			for _, kv := range f[2:] {
				key, val, ok := strings.Cut(kv, "=")
				switch {
				case !ok:
					return nil, fmt.Errorf("%s:%d: %q is not key=value", path, n, kv)
				case key == "git":
					k.Git = val
				case key == "rev":
					k.Rev = val
				case key == "host":
					k.Host = val
				default:
					return nil, fmt.Errorf("%s:%d: kernel: unknown key %q", path, n, key)
				}
			}
			if k.Git == "" || k.Rev == "" {
				return nil, fmt.Errorf("%s:%d: kernel needs git= and rev= (is the variable they name set?)", path, n)
			}
			b.Kernels = append(b.Kernels, k)
		case "run", "matrix", "ab":
			if len(f) < 2 {
				return nil, fmt.Errorf("%s:%d: %s needs a name", path, n, kind)
			}
			kv, err := parseKeys(f[2:], kind == "ab")
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, n, err)
			}
			keys := map[string]string{}
			for k, v := range defaults {
				keys[k] = v
			}
			for k, v := range kv {
				keys[k] = v
			}
			if kind == "ab" && (!hasPrefix(keys, "a.") || !hasPrefix(keys, "b.")) {
				return nil, fmt.Errorf("%s:%d: ab needs a.<key> and b.<key>", path, n)
			}
			b.Items = append(b.Items, Item{Kind: kind, Name: f[1], Line: n, Keys: keys})
		default:
			return nil, fmt.Errorf("%s:%d: unknown directive %q", path, n, kind)
		}
	}
	return b, sc.Err()
}

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// expandEnv replaces ${VAR} and ${VAR:-default}: the testbed has no kernel
// of its own, so where the trees are comes from the environment.
func expandEnv(line string) string {
	return envRe.ReplaceAllStringFunc(line, func(m string) string {
		g := envRe.FindStringSubmatch(m)
		if v := os.Getenv(g[1]); v != "" {
			return v
		}
		return g[3]
	})
}

// fields splits a line on spaces, keeping "double quoted" runs together and
// dropping the quotes, and stops at a # outside quotes.
func fields(line string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inQuote, have := false, false
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
			have = true
		case r == '#' && !inQuote:
			if have {
				out = append(out, cur.String())
			}
			return out, nil
		case (r == ' ' || r == '\t') && !inQuote:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quote")
	}
	if have {
		out = append(out, cur.String())
	}
	return out, nil
}

func parseKeys(fields []string, allowVariants bool) (map[string]string, error) {
	kv := map[string]string{}
	for _, f := range fields {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			return nil, fmt.Errorf("%q is not key=value", f)
		}
		base := k
		if allowVariants && (strings.HasPrefix(k, "a.") || strings.HasPrefix(k, "b.")) {
			base = k[2:]
		}
		if !knownKeys[base] {
			return nil, fmt.Errorf("unknown key %q", k)
		}
		kv[k] = v
	}
	return kv, nil
}

func hasPrefix(m map[string]string, p string) bool {
	for k := range m {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

func (it Item) Get(k, def string) string {
	if v, ok := it.Keys[k]; ok && v != "" {
		return v
	}
	return def
}

func (it Item) List(k string) []string {
	v := it.Get(k, "")
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

func (it Item) Ints(k string, def []int) ([]int, error) {
	l := it.List(k)
	if l == nil {
		return def, nil
	}
	out := make([]int, 0, len(l))
	for _, s := range l {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a number", k, s)
		}
		out = append(out, n)
	}
	return out, nil
}

// Variant is an ab item with one side's a./b. keys folded over the rest.
func (it Item) Variant(side string) Item {
	v := Item{Kind: "run", Name: it.Name + "-" + side, Line: it.Line, Keys: map[string]string{}}
	for k, val := range it.Keys {
		if strings.HasPrefix(k, "a.") || strings.HasPrefix(k, "b.") {
			continue
		}
		v.Keys[k] = val
	}
	for k, val := range it.Keys {
		if strings.HasPrefix(k, side+".") {
			v.Keys[k[2:]] = val
		}
	}
	return v
}

func (it Item) String() string {
	ks := make([]string, 0, len(it.Keys))
	for k := range it.Keys {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s", it.Kind, it.Name)
	for _, k := range ks {
		fmt.Fprintf(&sb, " %s=%s", k, it.Keys[k])
	}
	return sb.String()
}
