// Package hf talks to the Hugging Face Hub: search, repository files and
// the GGUF versions others made of a model.
package hf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Client is a Hugging Face Hub client. Token is optional (gated models).
type Client struct {
	Base  string // https://huggingface.co
	Token func() string
	HTTP  *http.Client
}

// NewClient returns a client for the public hub.
func NewClient(token func() string) *Client {
	return &Client{Base: "https://huggingface.co", Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// ErrNotFound means the repository does not exist or is private.
var ErrNotFound = errors.New("not found")

func (c *Client) get(ctx context.Context, p string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+p, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "socrates-api")
	if t := c.token(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("Hugging Face does not answer: %v", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("Hugging Face refused access (%d). For gated or private models, set a token in the API settings", resp.StatusCode)
	case resp.StatusCode >= 300:
		return fmt.Errorf("Hugging Face answered %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (c *Client) token() string {
	if c.Token == nil {
		return ""
	}
	return c.Token()
}

// SearchResult is one model found.
type SearchResult struct {
	ID        string `json:"id"`
	Downloads int64  `json:"downloads"`
	Likes     int64  `json:"likes"`
	GGUF      bool   `json:"gguf"`
	UpdatedAt string `json:"updatedAt"`
	Pipeline  string `json:"pipeline"`
}

type apiModel struct {
	ID           string   `json:"id"`
	Downloads    int64    `json:"downloads"`
	Likes        int64    `json:"likes"`
	Tags         []string `json:"tags"`
	PipelineTag  string   `json:"pipeline_tag"`
	LastModified string   `json:"lastModified"`
	CreatedAt    string   `json:"createdAt"`
	Gated        any      `json:"gated"`
	Private      bool     `json:"private"`
	CardData     struct {
		License   any `json:"license"`
		BaseModel any `json:"base_model"`
	} `json:"cardData"`
	Config struct {
		Architectures []string `json:"architectures"`
		ModelType     string   `json:"model_type"`
	} `json:"config"`
	Safetensors struct {
		Total int64 `json:"total"`
	} `json:"safetensors"`
	GGUFInfo struct {
		Total        int64  `json:"total"`
		Architecture string `json:"architecture"`
		ContextLen   int64  `json:"context_length"`
	} `json:"gguf"`
	Siblings []struct {
		Name string `json:"rfilename"`
		Size int64  `json:"size"`
		LFS  *struct {
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
		} `json:"lfs"`
	} `json:"siblings"`
}

func hasTag(tags []string, t string) bool {
	for _, x := range tags {
		if x == t {
			return true
		}
	}
	return false
}

// Search finds models by name, the most downloaded first.
func (c *Client) Search(ctx context.Context, q string) ([]SearchResult, error) {
	v := url.Values{"search": {q}, "limit": {"25"}, "sort": {"downloads"}}
	var ms []apiModel
	if err := c.get(ctx, "/api/models?"+v.Encode(), &ms); err != nil {
		return nil, err
	}
	out := make([]SearchResult, 0, len(ms))
	for _, m := range ms {
		out = append(out, SearchResult{ID: m.ID, Downloads: m.Downloads, Likes: m.Likes, GGUF: hasTag(m.Tags, "gguf"),
			UpdatedAt: m.LastModified, Pipeline: m.PipelineTag})
	}
	return out, nil
}

// RepoFile is a file of a repository.
type RepoFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

// Variant is one quantisation of a GGUF repository (split files grouped).
type Variant struct {
	Quant       string     `json:"quant"`
	Files       []string   `json:"files"`
	FileInfo    []RepoFile `json:"-"`
	Size        int64      `json:"size"`
	Bits        float64    `json:"bits"`
	Fit         string     `json:"fit"`
	Note        string     `json:"note"`
	Recommended bool       `json:"recommended"`
}

// MMProj is a vision projector file of a repository.
type MMProj struct {
	File string `json:"file"`
	Size int64  `json:"size"`
}

// Related is another repository: a GGUF version of this model.
type Related struct {
	ID        string `json:"id"`
	Downloads int64  `json:"downloads"`
}

// Repo is what the interface needs to pick a file.
type Repo struct {
	ID          string     `json:"id"`
	Gated       bool       `json:"gated"`
	License     string     `json:"license"`
	Pipeline    string     `json:"pipeline"`
	Params      int64      `json:"params"`
	BaseModel   string     `json:"baseModel"`
	Arch        string     `json:"arch"`
	GGUF        bool       `json:"gguf"`
	Variants    []Variant  `json:"variants"`
	MMProj      []RepoFile `json:"-"`
	MMProjFiles []MMProj   `json:"mmproj"`
	Quantized   []Related  `json:"quantized"`
	Convertible bool       `json:"convertible"`
	Safetensors int64      `json:"safetensorsBytes"`
	Files       []RepoFile `json:"-"`
}

// File returns a file of the repository by name.
func (r *Repo) File(name string) (RepoFile, bool) {
	for _, f := range r.Files {
		if f.Name == name {
			return f, true
		}
	}
	return RepoFile{}, false
}

// ParseID accepts "owner/name", a huggingface.co URL or "hf.co/owner/name:quant".
func ParseID(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range []string{"https://", "http://"} {
		s = strings.TrimPrefix(s, p)
	}
	for _, p := range []string{"www.huggingface.co/", "huggingface.co/", "hf.co/"} {
		s = strings.TrimPrefix(s, p)
	}
	if i := strings.IndexAny(s, ":?#"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ValidID reports whether id looks like owner/name.
func ValidID(id string) bool { return validID.MatchString(id) && !strings.Contains(id, "..") }

// Repo reads a repository and groups its GGUF files.
func (c *Client) Repo(ctx context.Context, id string) (*Repo, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("%q is not a Hugging Face model id (owner/name)", id)
	}
	var m apiModel
	if err := c.get(ctx, "/api/models/"+id+"?blobs=true", &m); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("There is no model %s on Hugging Face (or it is private: set a token)", id)
		}
		return nil, err
	}
	r := &Repo{ID: m.ID, Pipeline: m.PipelineTag, Arch: m.Config.ModelType}
	switch g := m.Gated.(type) {
	case bool:
		r.Gated = g
	case string:
		r.Gated = g != "" && g != "false"
	}
	r.License = fmt.Sprint(firstOf(m.CardData.License))
	r.BaseModel = fmt.Sprint(firstOf(m.CardData.BaseModel))
	if r.License == "<nil>" {
		r.License = ""
	}
	if r.BaseModel == "<nil>" {
		r.BaseModel = ""
	}
	r.Params = m.Safetensors.Total
	if r.Params == 0 {
		r.Params = m.GGUFInfo.Total
	}
	if r.Arch == "" {
		r.Arch = m.GGUFInfo.Architecture
	}
	var ggufs []RepoFile
	for _, s := range m.Siblings {
		f := RepoFile{Name: s.Name, Size: s.Size}
		if s.LFS != nil {
			f.SHA256 = s.LFS.SHA256
			if f.Size == 0 {
				f.Size = s.LFS.Size
			}
		}
		r.Files = append(r.Files, f)
		low := strings.ToLower(s.Name)
		switch {
		case strings.HasSuffix(low, ".safetensors"):
			r.Safetensors += f.Size
		case strings.HasSuffix(low, ".gguf"):
			if strings.Contains(path.Base(low), "mmproj") {
				r.MMProj = append(r.MMProj, f)
			} else {
				ggufs = append(ggufs, f)
			}
		}
	}
	r.MMProjFiles = []MMProj{}
	for _, f := range r.MMProj {
		r.MMProjFiles = append(r.MMProjFiles, MMProj{File: f.Name, Size: f.Size})
	}
	r.GGUF = len(ggufs) > 0
	r.Variants = groupVariants(ggufs)
	r.Convertible = !r.GGUF && r.Safetensors > 0 && len(m.Config.Architectures) > 0
	if !r.GGUF {
		r.Quantized, _ = c.quantized(ctx, id)
	}
	return r, nil
}

func firstOf(v any) any {
	if a, ok := v.([]any); ok {
		if len(a) == 0 {
			return nil
		}
		return a[0]
	}
	return v
}

func (c *Client) quantized(ctx context.Context, id string) ([]Related, error) {
	v := url.Values{"filter": {"base_model:quantized:" + id}, "limit": {"40"}, "sort": {"downloads"}}
	var ms []apiModel
	if err := c.get(ctx, "/api/models?"+v.Encode(), &ms); err != nil {
		return nil, err
	}
	out := []Related{}
	for _, m := range ms {
		if hasTag(m.Tags, "gguf") {
			out = append(out, Related{ID: m.ID, Downloads: m.Downloads})
		}
	}
	return out, nil
}

var (
	splitRe = regexp.MustCompile(`(?i)-(\d{5})-of-(\d{5})\.gguf$`)
	quantRe = regexp.MustCompile(`(?i)(?:^|[-_.])((?:UD-)?(?:IQ\d_(?:XXS|XS|S|M|NL|K_S|K_M)|Q\d_K_XL|Q\d_K_[SML]|Q\d_K|Q\d_[01]|Q8_0|TQ\d_0|MXFP4(?:_MOE)?|NVFP4|BF16|F16|F32|GSQ-RCO-IQ\d_[A-Z]+))(?:[-_.]|$)`)
)

// QuantOf guesses the quantisation from a file name.
func QuantOf(name string) string {
	base := path.Base(name)
	base = splitRe.ReplaceAllString(base, ".gguf")
	base = strings.TrimSuffix(base, ".gguf")
	if m := quantRe.FindAllStringSubmatch(base, -1); len(m) > 0 {
		return strings.ToUpper(m[len(m)-1][1])
	}
	// A folder named after the quant (unsloth puts big quants in folders).
	if dir := path.Dir(name); dir != "." {
		if m := quantRe.FindStringSubmatch(path.Base(dir)); m != nil {
			return strings.ToUpper(m[1])
		}
	}
	return base
}

// bitsOf is the rough bits per weight of a quant, for sorting.
func bitsOf(q string) float64 {
	q = strings.TrimPrefix(strings.ToUpper(q), "UD-")
	table := map[string]float64{
		"IQ1_S": 1.6, "IQ1_M": 1.8, "IQ2_XXS": 2.1, "IQ2_XS": 2.3, "IQ2_S": 2.5, "IQ2_M": 2.7, "Q2_K": 2.9, "Q2_K_S": 2.8, "Q2_K_XL": 3.0, "Q2_0": 2.5,
		"IQ3_XXS": 3.1, "IQ3_XS": 3.3, "IQ3_S": 3.4, "IQ3_M": 3.6, "Q3_K_S": 3.5, "Q3_K_M": 3.9, "Q3_K_L": 4.3, "Q3_K_XL": 4.0,
		"IQ4_XS": 4.3, "IQ4_NL": 4.5, "Q4_0": 4.5, "Q4_1": 5.0, "Q4_K_S": 4.6, "Q4_K": 4.9, "Q4_K_M": 4.9, "Q4_K_L": 5.2, "Q4_K_XL": 5.0, "MXFP4": 4.25, "MXFP4_MOE": 4.25,
		"Q5_0": 5.5, "Q5_1": 6.0, "Q5_K_S": 5.5, "Q5_K": 5.7, "Q5_K_M": 5.7, "Q5_K_L": 6.0, "Q5_K_XL": 5.9,
		"Q6_K": 6.6, "Q6_K_L": 6.8, "Q6_K_XL": 6.9, "Q8_0": 8.5, "Q8_K_XL": 9.0, "BF16": 16, "F16": 16, "F32": 32,
	}
	if b, ok := table[q]; ok {
		return b
	}
	return 0
}

func stemOf(name string) string {
	base := path.Base(name)
	base = splitRe.ReplaceAllString(base, ".gguf")
	return strings.TrimSuffix(strings.TrimSuffix(base, ".gguf"), ".GGUF")
}

// groupVariants groups split files and names every variant by what tells
// it apart from the others: the file name without the part all share
// ("Huihui-Qwen3.8-27B-abliterated-UD-Q4_K_XL" becomes "UD-Q4_K_XL").
func groupVariants(files []RepoFile) []Variant {
	groups := map[string]*Variant{}
	var order []string
	for _, f := range files {
		low := strings.ToLower(f.Name)
		base := path.Base(low)
		// Patches, separate MTP drafts and tiny helper files are no models;
		// the first part of a split model may be tiny, it holds the header.
		if strings.Contains(low, "patch") || strings.Contains(base, "imatrix") || strings.HasPrefix(base, "mtp-") || strings.HasPrefix(low, "mtp/") ||
			(f.Size > 0 && f.Size < 50<<20 && !splitRe.MatchString(f.Name)) {
			continue
		}
		key := splitRe.ReplaceAllString(f.Name, "")
		v := groups[key]
		if v == nil {
			v = &Variant{Quant: stemOf(f.Name)}
			groups[key] = v
			order = append(order, key)
		}
		v.Files = append(v.Files, f.Name)
		v.FileInfo = append(v.FileInfo, f)
		v.Size += f.Size
	}
	attachShared(groups, &order)
	stems := make([]string, 0, len(order))
	for _, k := range order {
		stems = append(stems, groups[k].Quant)
	}
	cut := commonPrefix(stems)
	out := make([]Variant, 0, len(order))
	for _, k := range order {
		v := groups[k]
		sort.Strings(v.Files)
		sort.Slice(v.FileInfo, func(i, j int) bool { return v.FileInfo[i].Name < v.FileInfo[j].Name })
		label := strings.Trim(v.Quant[cut:], "-_. ")
		if label == "" {
			label = v.Quant
		}
		v.Bits = bitsOf(QuantOf(v.Files[0]))
		v.Quant = label
		out = append(out, *v)
	}
	// The same name in two folders (a re-upload): the folder tells them apart.
	seen := map[string]int{}
	for _, v := range out {
		seen[v.Quant]++
	}
	for i := range out {
		if dir := path.Dir(out[i].Files[0]); seen[out[i].Quant] > 1 && dir != "." {
			out[i].Quant += " (" + path.Base(dir) + ")"
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Size < out[j].Size })
	return out
}

// SplitParts reads "-00002-of-00006.gguf": part 2 of 6; 0, 0 for other names.
func SplitParts(name string) (part, total int) {
	m := splitRe.FindStringSubmatch(name)
	if m == nil {
		return 0, 0
	}
	fmt.Sscan(m[1], &part)
	fmt.Sscan(m[2], &total)
	return part, total
}

// IsShared reports whether a file is a part that several builds share
// (the Engram lookup tables of DeepSeek V4.1: one file that llama.cpp
// opens as the last part of every build).
func IsShared(name string) bool {
	low := strings.ToLower(path.Base(name))
	return strings.Contains(low, "engram") && !splitRe.MatchString(name)
}

// attachShared adds a shared file to the split builds it completes and
// drops it as a variant of its own.
func attachShared(groups map[string]*Variant, order *[]string) {
	var shared []string
	for _, k := range *order {
		if v := groups[k]; len(v.Files) == 1 && IsShared(v.Files[0]) {
			shared = append(shared, k)
		}
	}
	if len(shared) != 1 {
		return
	}
	sv := groups[shared[0]]
	used := false
	for _, k := range *order {
		v := groups[k]
		_, total := SplitParts(v.Files[0])
		if total > 0 && len(v.Files) == total-1 {
			v.Files = append(v.Files, sv.Files[0])
			v.FileInfo = append(v.FileInfo, sv.FileInfo...)
			v.Size += sv.Size
			used = true
		}
	}
	if used {
		out := (*order)[:0]
		for _, k := range *order {
			if k != shared[0] {
				out = append(out, k)
			}
		}
		*order = out
	}
}

// commonPrefix is the length of the prefix all names share, cut back to a
// separator so no quant name is cut in half.
func commonPrefix(names []string) int {
	if len(names) < 2 {
		// One file: drop everything before the quant.
		if len(names) == 1 {
			if m := quantRe.FindStringSubmatchIndex(names[0]); m != nil {
				return m[2]
			}
		}
		return 0
	}
	p := names[0]
	for _, n := range names[1:] {
		i := 0
		for i < len(p) && i < len(n) && p[i] == n[i] {
			i++
		}
		p = p[:i]
	}
	i := strings.LastIndexAny(p, "-_.")
	if i < 0 {
		return 0
	}
	return i + 1
}

// Machine is what the verdict per variant is based on.
type Machine struct {
	VRAM     int64
	RAM      int64 // available to a model
	DiskFree int64
}

const gib = 1 << 30

// Assess marks every variant with where it would run and recommends one.
func Assess(vs []Variant, m Machine, mmproj int64) {
	best := -1
	bestHybrid := -1
	for i := range vs {
		v := &vs[i]
		size := v.Size
		overhead := 2*gib + size/12 // context and compute buffers at 32k, roughly
		switch {
		case m.VRAM > 0 && size+overhead+mmproj+gib <= m.VRAM:
			v.Fit = "gpu"
			v.Note = "Fits on the GPU with room for a long context"
		case m.VRAM > 0 && size+mmproj+gib <= m.VRAM:
			v.Fit = "gpu"
			v.Note = "Fits on the GPU, with a shorter context"
		case size <= m.VRAM+m.RAM-4*gib:
			v.Fit = "hybrid"
			v.Note = fmt.Sprintf("GPU + RAM: about %.0f GB in RAM", float64(size-m.VRAM+3*gib)/gib)
		case m.DiskFree == 0 || size < m.DiskFree:
			v.Fit = "disk"
			v.Note = "Larger than GPU + RAM: parts are read from the NVMe while it runs (slow)"
		default:
			v.Fit = "no"
			v.Note = "Not enough free disk space"
		}
		if v.Bits >= 16 || v.Bits == 0 {
			continue
		}
		if v.Fit == "gpu" && size+overhead+mmproj+gib <= m.VRAM && v.Bits >= 3.9 && v.Bits <= 6.7 {
			if best < 0 || better(v, &vs[best]) {
				best = i
			}
		}
		if v.Fit == "hybrid" && v.Bits >= 2.9 && v.Bits <= 5 {
			bestHybrid = i // sorted by size: the largest that fits RAM
		}
	}
	if best < 0 {
		best = bestHybrid
	}
	if best < 0 {
		// The smallest that runs at all.
		for i := range vs {
			if vs[i].Fit != "no" && vs[i].Bits > 0 && vs[i].Bits < 16 {
				best = i
				break
			}
		}
	}
	if best >= 0 {
		vs[best].Recommended = true
	}
}

// better prefers ~4.5-5 bits on the GPU: quality per GB is best there and
// the context keeps room.
func better(a, b *Variant) bool {
	score := func(v *Variant) float64 {
		d := v.Bits - 4.9
		if d < 0 {
			d = -d * 1.5
		}
		s := -d
		if strings.HasPrefix(v.Quant, "UD-") {
			s += 0.05
		}
		return s
	}
	return score(a) > score(b)
}
