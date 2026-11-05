package runtime

// 存储子系统：memory（键值）与 vector（向量检索）。
//
// 为什么放在运行时而不是语言里：存储是**部署事实**（路径、远端、配额），
// 而语言只该描述"读一个键""存一条向量"。把它写进语法，等于让每个 workflow
// 都硬编码一份部署参数 —— 那正是 `backend` 用 env 注入要消灭的东西。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store 是文件存储的句柄：一个目录，里面 memory.json 与 vectors.json。
//
// 为什么是文件而不是数据库：红线要求零第三方依赖，而"能用"的第一步是
// **不需要额外服务**。文件存储可被替换（同样的接口换 Redis/Postgres），
// 但默认实现必须让 `hermes run` 一个进程就够。
type Store struct {
	Dir string

	mu      sync.Mutex
	memory  map[string]json.RawMessage
	vectors map[string]storedVector
	loaded  bool
}

type storedVector struct {
	Text  string    `json:"text"`
	Vec   []float32 `json:"vec"`
	Model string    `json:"model"`
	// Norm 是 vec 的 L2 模长，写入时算一次。
	//
	// 为什么存它：检索要对**每个**条目算余弦，而余弦的归一化项只取决于
	// 向量自己 —— 每次查询重算 n 次平方和是纯浪费。存下来之后每次比较
	// 只剩一次点积。旧文件里没有这个字段，读的时候补算（见 ensureNorms）。
	Norm   float64 `json:"norm,omitempty"`
	Source string  `json:"source,omitempty"`
}

// OpenStore 打开（或创建）一个存储目录。
func OpenStore(dir string) (*Store, error) {
	if dir == "" {
		dir = os.Getenv("HERMES_STORE")
	}
	if dir == "" {
		return nil, errors.New("没有配置存储位置：给 hermes run/serve 传 --store <dir>，或设 HERMES_STORE")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建存储目录失败: %w", err)
	}
	return &Store{Dir: dir, memory: map[string]json.RawMessage{}, vectors: map[string]storedVector{}}, nil
}

func (s *Store) load() error {
	if s.loaded {
		return nil
	}
	if b, err := os.ReadFile(filepath.Join(s.Dir, "memory.json")); err == nil {
		if err := json.Unmarshal(b, &s.memory); err != nil {
			return fmt.Errorf("memory.json 损坏: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if b, err := os.ReadFile(filepath.Join(s.Dir, "vectors.json")); err == nil {
		if err := json.Unmarshal(b, &s.vectors); err != nil {
			return fmt.Errorf("vectors.json 损坏: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	s.loaded = true
	return nil
}

// flush 原子写：先写临时文件再 rename。
//
// 直接覆盖写会在中途崩溃时留下半个文件 —— 而"半个 JSON"是不可恢复的，
// 下次启动直接解析失败。rename 在同一个目录内是原子的。
func (s *Store) flush() error {
	if err := s.writeJSON("memory.json", s.memory); err != nil {
		return err
	}
	return s.writeJSON("vectors.json", s.vectors)
}

func (s *Store) writeJSON(name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.Dir, name+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.Dir, name))
}

// expireEnvelope 是带 TTL 的条目外壳。
//
// 为什么用一个**带 $ 前缀的键**而不是改整个文件结构：旧的 memory.json 里
// 直接就是值。加一层外壳的话，旧文件里的每个值都要迁移；而"读的时候认
// 这个外壳、不认就当普通值"可以让新旧共存，不需要迁移脚本。
const expireKey = "$hermes_exp"

type memEnvelope struct {
	Exp int64           `json:"$hermes_exp"`
	V   json.RawMessage `json:"v"`
}

// MemoryGet 读一个键。不存在（含已过期）返回 nil。
func (s *Store) MemoryGet(key string) (any, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, false, err
	}
	raw, ok := s.memory[key]
	if !ok {
		return nil, false, nil
	}
	body, expired := unwrapMemory(raw, time.Now().Unix())
	if expired {
		// 惰性删除：过期条目在**被读到**时清掉。没有后台清理线程，
		// 因为那需要生命周期管理，而读时清掉已经足够避免无限增长。
		delete(s.memory, key)
		_ = s.flush()
		return nil, false, nil
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, false, err
	}
	return v, true, nil
}

// MemoryPut 写一个键。ttl > 0 时该条目到期后自动不可见。
func (s *Store) MemoryPut(key string, value any, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if ttl > 0 {
		env := memEnvelope{Exp: time.Now().Add(ttl).Unix(), V: b}
		b, err = json.Marshal(env)
		if err != nil {
			return err
		}
	}
	s.memory[key] = b
	return s.flush()
}

// unwrapMemory 剥掉 TTL 外壳。返回内容与"是否已过期"。
func unwrapMemory(raw json.RawMessage, now int64) (json.RawMessage, bool) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return raw, false
	}
	expRaw, ok := probe[expireKey]
	if !ok {
		return raw, false
	}
	var exp int64
	if err := json.Unmarshal(expRaw, &exp); err != nil {
		return raw, false
	}
	if exp > 0 && now >= exp {
		return nil, true
	}
	if v, ok := probe["v"]; ok {
		return v, false
	}
	return raw, false
}

// MemoryDelete 删除一个键。返回是否真的删掉了（不存在返回 false）。
func (s *Store) MemoryDelete(key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return false, err
	}
	if _, ok := s.memory[key]; !ok {
		return false, nil
	}
	delete(s.memory, key)
	return true, s.flush()
}

// VectorDelete 删除一条向量。
func (s *Store) VectorDelete(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return false, err
	}
	if _, ok := s.vectors[id]; !ok {
		return false, nil
	}
	delete(s.vectors, id)
	return true, s.flush()
}

// VectorPut 存一条向量。
func (s *Store) VectorPut(id, text string, vec []float32, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	s.vectors[id] = storedVector{Text: text, Vec: vec, Model: model, Norm: l2norm(vec)}
	return s.flush()
}

// Match 是一条检索结果。
type Match struct {
	ID    string  `json:"id"`
	Text  string  `json:"text"`
	Score float64 `json:"score"`
}

// VectorQuery 做余弦相似度检索，返回前 k 条。
// VectorQuery 只返回 ns 命名空间下的条目（ns 为空表示不筛）。
// 命名空间在写入时就已加进 Key，所以这里按前缀过滤即可，不需要额外的索引。
func (s *Store) VectorQuery(ns string, query []float32, k int) ([]Match, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, err
	}
	if k <= 0 {
		k = 5
	}
	qNorm := l2norm(query)
	out := make([]Match, 0, len(s.vectors))
	for id, v := range s.vectors {
		// 命名空间过滤：不同 unit 的条目互不可见。
		if ns != "" && !hasPrefix(id, ns) {
			continue
		}
		// 旧条目没有 Norm：补算一次并记住（不立即落盘，下次 Put 时一起写）。
		vn := float64(v.Norm)
		if vn == 0 {
			vn = l2norm(v.Vec)
			v.Norm = vn
			s.vectors[id] = v
		}
		out = append(out, Match{ID: id, Text: v.Text, Score: cosineWithNorm(query, qNorm, v.Vec, vn)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].ID < out[j].ID // 分数相同按 id 排序：**稳定**比"随便"重要
		}
		return out[i].Score > out[j].Score
	})
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// hasPrefix 判断键是否属于某个命名空间（`unit/...`）。
// 用 key 自身做判断而不是加字段：旧文件里没有 namespace 字段，加字段要迁移。
func hasPrefix(key, ns string) bool {
	if len(key) <= len(ns) || key[len(ns)] != '/' {
		return false
	}
	return key[:len(ns)] == ns
}

// l2norm 返回向量的 L2 模长。
func l2norm(v []float32) float64 {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	return math.Sqrt(n)
}

// cosineWithNorm 用预先算好的 |a| 计算余弦：少一遍平方和。
func cosineWithNorm(a []float32, aNorm float64, b []float32, bNorm float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
	}
	if aNorm == 0 || bNorm == 0 {
		return 0
	}
	return dot / (aNorm * bNorm)
}

func cosine(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// NormalizeKey 让 key 里的空白不参与相等性判断。
//
// 记忆的 key 常常来自用户输入（"用户: 12345"），而多一个空格就让"同一个键"
// 变成两个 —— 这种故障很难发现，所以在入口处收敛。
func NormalizeKey(k string) string { return strings.TrimSpace(k) }

// Embed 调一次 embeddings 面，返回向量。
//
// 走 OpenAI 兼容的 /v1/embeddings：这是唯一一个几乎所有人都实现的向量接口，
// 也意味着"换一个 provider"不需要改语言、不需要改 workflow。
func Embed(baseURL, apiKey, model, text string) ([]float32, error) {
	if baseURL == "" {
		baseURL = os.Getenv("HERMES_EMBED_URL")
	}
	if baseURL == "" {
		baseURL = os.Getenv("GATEWAY_URL")
	}
	if baseURL == "" {
		return nil, errors.New("没有 embeddings 地址：设 HERMES_EMBED_URL 或 GATEWAY_URL")
	}
	if model == "" {
		model = os.Getenv("HERMES_EMBED_MODEL")
	}
	if model == "" {
		model = "text-embedding-3-small"
	}
	body, _ := json.Marshal(map[string]any{"model": model, "input": text})
	req, err := newJSONRequest(strings.TrimRight(baseURL, "/")+"/v1/embeddings", body, apiKey)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("embeddings 返回 %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var parsed struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("embeddings 响应不是合法 JSON: %w", err)
	}
	if len(parsed.Data) == 0 || len(parsed.Data[0].Embedding) == 0 {
		return nil, errors.New("embeddings 响应里没有向量")
	}
	return parsed.Data[0].Embedding, nil
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func newJSONRequest(url string, body []byte, apiKey string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey == "" {
		apiKey = os.Getenv("HERMES_API_KEY")
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return req, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
