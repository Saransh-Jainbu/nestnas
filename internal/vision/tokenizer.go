package vision

import (
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
)

// The CLIP text tokenizer: lowercase, split into words, map bytes to printable characters,
// merge character pairs by learned rank (BPE), then look the pieces up in the vocabulary.
// The vocabulary and merge list come from the model's tokenizer.json.

var clipSplit = regexp.MustCompile(`<\|startoftext\|>|<\|endoftext\|>|'s|'t|'re|'ve|'m|'ll|'d|[\p{L}]+|[\p{N}]|[^\s\p{L}\p{N}]+`)

type tokenizer struct {
	vocab map[string]int
	rank  map[[2]string]int
	b2u   [256]rune
	bos   int
	eos   int
	cache map[string][]int
}

func loadTokenizer(path string) (*tokenizer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var j struct {
		Model struct {
			Vocab  map[string]int  `json:"vocab"`
			Merges json.RawMessage `json:"merges"`
		} `json:"model"`
	}
	if err := json.Unmarshal(b, &j); err != nil || len(j.Model.Vocab) == 0 {
		return nil, errors.New("unreadable tokenizer")
	}
	t := &tokenizer{vocab: j.Model.Vocab, rank: map[[2]string]int{}, cache: map[string][]int{}}
	// merges are "a b" strings, or [a, b] pairs in newer files
	var strs []string
	var pairs [][2]string
	if json.Unmarshal(j.Model.Merges, &strs) == nil {
		for i, m := range strs {
			if p := strings.SplitN(m, " ", 2); len(p) == 2 {
				t.rank[[2]string{p[0], p[1]}] = i
			}
		}
	} else if json.Unmarshal(j.Model.Merges, &pairs) == nil {
		for i, p := range pairs {
			t.rank[p] = i
		}
	} else {
		return nil, errors.New("unreadable merges")
	}
	var ok1, ok2 bool
	t.bos, ok1 = t.vocab["<|startoftext|>"]
	t.eos, ok2 = t.vocab["<|endoftext|>"]
	if !ok1 || !ok2 {
		return nil, errors.New("tokenizer lacks start/end tokens")
	}
	// bytes -> printable unicode, as in GPT-2/CLIP
	var bs []int
	for i := '!'; i <= '~'; i++ {
		bs = append(bs, int(i))
	}
	for i := 0xA1; i <= 0xAC; i++ {
		bs = append(bs, i)
	}
	for i := 0xAE; i <= 0xFF; i++ {
		bs = append(bs, i)
	}
	has := map[int]bool{}
	for _, b := range bs {
		has[b] = true
		t.b2u[b] = rune(b)
	}
	n := 0
	for b := 0; b < 256; b++ {
		if !has[b] {
			t.b2u[b] = rune(256 + n)
			n++
		}
	}
	return t, nil
}

func (t *tokenizer) bpe(word string) []int {
	if ids, ok := t.cache[word]; ok {
		return ids
	}
	var parts []string
	rs := []rune{}
	for _, b := range []byte(word) {
		rs = append(rs, t.b2u[b])
	}
	for i, r := range rs {
		p := string(r)
		if i == len(rs)-1 {
			p += "</w>"
		}
		parts = append(parts, p)
	}
	for len(parts) > 1 {
		best, bi := int(^uint(0)>>1), -1
		for i := 0; i+1 < len(parts); i++ {
			if r, ok := t.rank[[2]string{parts[i], parts[i+1]}]; ok && r < best {
				best, bi = r, i
			}
		}
		if bi < 0 {
			break
		}
		parts[bi] += parts[bi+1]
		parts = append(parts[:bi+1], parts[bi+2:]...)
	}
	ids := make([]int, 0, len(parts))
	for _, p := range parts {
		if id, ok := t.vocab[p]; ok {
			ids = append(ids, id)
		} else {
			ids = append(ids, t.eos) // unknown piece
		}
	}
	if len(t.cache) < 20000 {
		t.cache[word] = ids
	}
	return ids
}

// encode returns the token ids for text, wrapped in start/end tokens and cut to max tokens.
func (t *tokenizer) encode(text string, max int) []int {
	text = strings.ToLower(strings.Join(strings.Fields(text), " "))
	ids := []int{t.bos}
	for _, w := range clipSplit.FindAllString(text, -1) {
		ids = append(ids, t.bpe(w)...)
	}
	if len(ids) > max-1 {
		ids = ids[:max-1]
	}
	return append(ids, t.eos)
}
