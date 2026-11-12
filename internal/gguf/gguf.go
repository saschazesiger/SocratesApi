// Package gguf reads the header of GGUF files: the metadata and where each
// tensor lies in the file. It never reads tensor data, so even a 300 GB
// model is inspected in a moment.
package gguf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Tensor is one tensor of a file.
type Tensor struct {
	Name   string
	Type   uint32
	Dims   []uint64
	Offset int64 // absolute offset in the file
	Size   int64 // bytes, from the distance to the next tensor
	File   int   // index of the split file
}

// Layer returns the block index of the tensor ("blk.12.*" is 12), or -1.
func (t Tensor) Layer() int {
	if !strings.HasPrefix(t.Name, "blk.") {
		return -1
	}
	rest := t.Name[4:]
	i := strings.IndexByte(rest, '.')
	if i < 0 {
		return -1
	}
	n, err := strconv.Atoi(rest[:i])
	if err != nil {
		return -1
	}
	return n
}

// IsExpert reports whether the tensor holds the routed experts of a MoE layer.
func (t Tensor) IsExpert() bool { return strings.Contains(t.Name, "_exps") }

// File is a parsed GGUF header (of one or more split files).
type File struct {
	Paths    []string
	Sizes    []int64
	Meta     map[string]any
	Tensors  []Tensor
	DataSize int64 // sum of tensor bytes
}

// Arch returns general.architecture.
func (f *File) Arch() string { s, _ := f.Meta["general.architecture"].(string); return s }

// Uint reads an integer metadata value, also from the architecture's namespace
// when key starts with ".".
func (f *File) Uint(key string) uint64 {
	if strings.HasPrefix(key, ".") {
		key = f.Arch() + key
	}
	switch v := f.Meta[key].(type) {
	case uint64:
		return v
	case int64:
		if v > 0 {
			return uint64(v)
		}
	case []any:
		// Per-layer values (for example head_count_kv of hybrid models): the largest.
		var max uint64
		for _, x := range v {
			if n, ok := toUint(x); ok && n > max {
				max = n
			}
		}
		return max
	}
	return 0
}

// String reads a string metadata value.
func (f *File) String(key string) string {
	if strings.HasPrefix(key, ".") {
		key = f.Arch() + key
	}
	s, _ := f.Meta[key].(string)
	return s
}

// Has reports whether a metadata key exists.
func (f *File) Has(key string) bool { _, ok := f.Meta[key]; return ok }

func toUint(x any) (uint64, bool) {
	switch v := x.(type) {
	case uint64:
		return v, true
	case int64:
		if v >= 0 {
			return uint64(v), true
		}
	}
	return 0, false
}

// Open parses the header of path. For a split model ("-00001-of-00004.gguf")
// pass every part; tensors of all parts are merged.
func Open(paths ...string) (*File, error) {
	f := &File{Paths: paths, Meta: map[string]any{}}
	for i, p := range paths {
		if err := f.read(p, i); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	for _, t := range f.Tensors {
		f.DataSize += t.Size
	}
	return f, nil
}

type reader struct {
	r   *bufio.Reader
	n   int64
	err error
}

func (r *reader) read(b []byte) {
	if r.err != nil {
		return
	}
	n, err := io.ReadFull(r.r, b)
	r.n += int64(n)
	r.err = err
}

func (r *reader) u8() uint8   { var b [1]byte; r.read(b[:]); return b[0] }
func (r *reader) u16() uint16 { var b [2]byte; r.read(b[:]); return binary.LittleEndian.Uint16(b[:]) }
func (r *reader) u32() uint32 { var b [4]byte; r.read(b[:]); return binary.LittleEndian.Uint32(b[:]) }
func (r *reader) u64() uint64 { var b [8]byte; r.read(b[:]); return binary.LittleEndian.Uint64(b[:]) }

func (r *reader) str() string {
	n := r.u64()
	if r.err != nil {
		return ""
	}
	if n > 64<<20 {
		r.err = errors.New("string too long; not a GGUF file?")
		return ""
	}
	b := make([]byte, n)
	r.read(b)
	return string(b)
}

// skipStr reads past a string without keeping it (token lists are large).
func (r *reader) skipStr() {
	n := r.u64()
	if r.err != nil {
		return
	}
	d, err := r.r.Discard(int(n))
	r.n += int64(d)
	if err != nil {
		r.err = err
	}
}

const maxKeptArray = 4096

func (r *reader) value(typ uint32, keep bool) any { return r.valueAt(typ, keep, 0) }

// valueAt reads one value; depth guards against arrays nested on purpose
// to exhaust the stack.
func (r *reader) valueAt(typ uint32, keep bool, depth int) any {
	switch typ {
	case 0:
		return uint64(r.u8())
	case 1:
		return int64(int8(r.u8()))
	case 2:
		return uint64(r.u16())
	case 3:
		return int64(int16(r.u16()))
	case 4:
		return uint64(r.u32())
	case 5:
		return int64(int32(r.u32()))
	case 6:
		return float64(math.Float32frombits(r.u32()))
	case 7:
		return r.u8() != 0
	case 8:
		if !keep {
			r.skipStr()
			return nil
		}
		return r.str()
	case 9:
		et := r.u32()
		n := r.u64()
		keepItems := keep && n <= maxKeptArray
		var out []any
		if keepItems {
			out = make([]any, 0, n)
		}
		for i := uint64(0); i < n && r.err == nil; i++ {
			if depth > 4 {
				r.err = errors.New("arrays nested too deeply; not a valid GGUF file")
				return nil
			}
			v := r.valueAt(et, keepItems, depth+1)
			if keepItems {
				out = append(out, v)
			}
		}
		if !keepItems {
			return int64(n) // only the length of long arrays (token lists)
		}
		return out
	case 10:
		return r.u64()
	case 11:
		return int64(r.u64())
	case 12:
		return math.Float64frombits(r.u64())
	}
	r.err = fmt.Errorf("unknown metadata type %d", typ)
	return nil
}

func (f *File) read(path string, index int) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return err
	}
	f.Sizes = append(f.Sizes, st.Size())
	r := &reader{r: bufio.NewReaderSize(fh, 1<<20)}
	var magic [4]byte
	r.read(magic[:])
	if r.err != nil || string(magic[:]) != "GGUF" {
		return errors.New("not a GGUF file")
	}
	version := r.u32()
	if version < 2 {
		return fmt.Errorf("GGUF version %d is too old", version)
	}
	nTensors := r.u64()
	nKV := r.u64()
	if r.err != nil || nTensors > 1<<24 || nKV > 1<<20 {
		return errors.New("damaged GGUF header")
	}
	for i := uint64(0); i < nKV && r.err == nil; i++ {
		key := r.str()
		typ := r.u32()
		// The chat template is long but useful to know about; token lists are skipped.
		v := r.value(typ, !strings.HasPrefix(key, "tokenizer.ggml."))
		if index == 0 || !strings.HasPrefix(key, "split.") {
			if _, exists := f.Meta[key]; !exists || index == 0 {
				f.Meta[key] = v
			}
		}
	}
	start := len(f.Tensors)
	for i := uint64(0); i < nTensors && r.err == nil; i++ {
		t := Tensor{Name: r.str(), File: index}
		nd := r.u32()
		if nd > 8 {
			return errors.New("damaged tensor info")
		}
		for d := uint32(0); d < nd; d++ {
			t.Dims = append(t.Dims, r.u64())
		}
		t.Type = r.u32()
		t.Offset = int64(r.u64())
		f.Tensors = append(f.Tensors, t)
	}
	if r.err != nil {
		return r.err
	}
	align := int64(32)
	if a, ok := toUint(f.Meta["general.alignment"]); ok && a > 0 {
		align = int64(a)
	}
	dataStart := (r.n + align - 1) / align * align
	part := f.Tensors[start:]
	sort.SliceStable(part, func(i, j int) bool { return part[i].Offset < part[j].Offset })
	for i := range part {
		part[i].Offset += dataStart
		end := st.Size()
		if i+1 < len(part) {
			end = part[i+1].Offset + dataStart
		}
		part[i].Size = end - part[i].Offset
		if part[i].Size < 0 {
			return errors.New("damaged tensor offsets")
		}
	}
	return nil
}

// Overrides are tensor placement rules as llama.cpp's -ot takes them:
// "regex=BUFFER,regex=BUFFER".
type Override struct {
	Re     *regexp.Regexp
	Target string
}

// ParseOverrides parses an -ot value. Patterns llama.cpp accepts but Go's
// regexp does not are skipped.
func ParseOverrides(s string) []Override {
	var out []Override
	for _, part := range splitOverrides(s) {
		i := strings.LastIndexByte(part, '=')
		if i <= 0 {
			continue
		}
		re, err := regexp.Compile(part[:i])
		if err != nil {
			continue
		}
		out = append(out, Override{Re: re, Target: part[i+1:]})
	}
	return out
}

// splitOverrides splits on commas that separate rules, not those inside a
// regex group like {1,3}.
func splitOverrides(s string) []string {
	var out []string
	depth := 0
	last := 0
	for i, c := range s {
		switch c {
		case '(', '{', '[':
			depth++
		case ')', '}', ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[last:i])
				last = i + 1
			}
		}
	}
	if last < len(s) {
		out = append(out, s[last:])
	}
	return out
}
