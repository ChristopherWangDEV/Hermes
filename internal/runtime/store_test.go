package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMemoryPersistsAcrossOpens 守"记忆真的跨进程活着"。
//
// 只测内存里的 map 是没用的：记忆的全部价值在于**关掉再打开还在**。
func TestMemoryPersistsAcrossOpens(t *testing.T) {
	dir := t.TempDir()

	s1, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.MemoryPut("user:1", map[string]any{"last": "你好"}, 0); err != nil {
		t.Fatal(err)
	}

	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, ok, err := s2.MemoryGet("user:1")
	if err != nil || !ok {
		t.Fatalf("重新打开后读不到: ok=%v err=%v", ok, err)
	}
	m, _ := v.(map[string]any)
	if m["last"] != "你好" {
		t.Fatalf("值不对: %v", v)
	}
}

// TestMemoryMissingKeyIsNotAnError 守"没有历史"不是错误。
//
// 第一次对话时键必然不存在。若这算错误，用户就得先 try/catch 一次 ——
// 那是把正常路径写成异常路径。
func TestMemoryMissingKeyIsNotAnError(t *testing.T) {
	s, _ := OpenStore(t.TempDir())
	v, ok, err := s.MemoryGet("nobody")
	if err != nil {
		t.Fatalf("缺键不该报错: %v", err)
	}
	if ok || v != nil {
		t.Fatalf("缺键应返回 (nil,false)，实际 (%v,%v)", v, ok)
	}
}

// TestVectorQueryRanksBySimilarity 守向量检索的方向：最像的排第一。
func TestVectorQueryRanksBySimilarity(t *testing.T) {
	s, _ := OpenStore(t.TempDir())
	_ = s.VectorPut("a", "退款", []float32{1, 0, 0}, "m")
	_ = s.VectorPut("b", "物流", []float32{0, 1, 0}, "m")
	_ = s.VectorPut("c", "退货退款", []float32{0.9, 0.1, 0}, "m")

	got, err := s.VectorQuery("", []float32{1, 0, 0}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应返回 2 条，实际 %d", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("排序不对: %+v", got)
	}
}

// TestEmbedUsesOpenAICompatibleFace 守"换 provider 不改 workflow"。
func TestEmbedUsesOpenAICompatibleFace(t *testing.T) {
	var path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()

	vec, err := Embed(srv.URL, "", "test-model", "你好")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/embeddings" {
		t.Fatalf("应走 OpenAI 兼容面，实际 %s", path)
	}
	if body["model"] != "test-model" || body["input"] != "你好" {
		t.Fatalf("请求体不对: %v", body)
	}
	if len(vec) != 3 || vec[1] != 0.2 {
		t.Fatalf("向量不对: %v", vec)
	}
}

// TestStoreSurvivesCorruptScratchFile 守原子写：临时文件不该影响读数。
func TestStoreSurvivesCorruptScratchFile(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(dir)
	if err := s.MemoryPut("k", "v", 0); err != nil {
		t.Fatal(err)
	}
	// 模拟"写到一半崩了"：留下一个 .tmp，正式文件仍然可读。
	if err := writeTemp(filepath.Join(dir, "memory.json.tmp")); err != nil {
		t.Fatal(err)
	}
	s2, _ := OpenStore(dir)
	if _, ok, err := s2.MemoryGet("k"); err != nil || !ok {
		t.Fatalf("临时文件不该影响读数: ok=%v err=%v", ok, err)
	}
}

func writeTemp(p string) error {
	return os.WriteFile(p, []byte(`{"half":`), 0o644)
}

// TestStoreNamespaceIsolation 守"两个 unit 共用目录时不串号"。
//
// 命名空间取自 unit 名（编译期常量），写入时加进键；读取与向量检索按前缀过滤。
// 没有它，同名键会互相覆盖 —— 那是数据串号，不是性能问题。
func TestStoreNamespaceIsolation(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MemoryPut(nsKey("unitA", "k"), "a", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.MemoryPut(nsKey("unitB", "k"), "b", 0); err != nil {
		t.Fatal(err)
	}
	va, ok, err := s.MemoryGet(nsKey("unitA", "k"))
	if err != nil || !ok || va != "a" {
		t.Fatalf("unitA 应读到 a，实际 %v ok=%v err=%v", va, ok, err)
	}
	vb, ok, err := s.MemoryGet(nsKey("unitB", "k"))
	if err != nil || !ok || vb != "b" {
		t.Fatalf("unitB 应读到 b，实际 %v ok=%v err=%v", vb, ok, err)
	}
	// 未加命名空间的键与两者都不冲突
	if _, ok, _ := s.MemoryGet("k"); ok {
		t.Fatal("未命名空间的键不应存在")
	}
}

// TestVectorQueryFiltersByNamespace 守向量检索同样按命名空间隔离。
func TestVectorQueryFiltersByNamespace(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VectorPut(nsKey("unitA", "x"), "alpha", []float32{1, 0}, "m"); err != nil {
		t.Fatal(err)
	}
	if err := s.VectorPut(nsKey("unitB", "y"), "beta", []float32{1, 0}, "m"); err != nil {
		t.Fatal(err)
	}
	got, err := s.VectorQuery("unitA", []float32{1, 0}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "alpha" {
		t.Fatalf("unitA 只该看到 alpha，实际 %+v", got)
	}
}

// TestMemoryTTL 守"过期条目自动不可见"。
//
// 没有 TTL 时记忆只增不减：一个每天跑的 agent 会把 memory.json 写成
// 一条永远在变长的历史。过期必须在**读**的时候生效，而不是靠外部清理。
func TestMemoryTTL(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MemoryPut("k", "v", 0); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.MemoryGet("k"); err != nil || !ok || v != "v" {
		t.Fatalf("无 TTL 的条目应永久可见：%v ok=%v err=%v", v, ok, err)
	}

	// 手写一条已过期的条目（不 sleep：测试不该依赖时钟）。
	if err := s.MemoryPut("expired", "gone", time.Hour); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.memory["expired"] = []byte(`{"$hermes_exp":1,"v":"gone"}`)
	s.mu.Unlock()

	if v, ok, err := s.MemoryGet("expired"); err != nil || ok {
		t.Fatalf("过期条目应不可见，实际 %v ok=%v err=%v", v, ok, err)
	}
}

// TestMemoryOldFormatStillReadable 守"旧文件不需要迁移"。
func TestMemoryOldFormatStillReadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.json"), []byte(`{"old":"plain"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.MemoryGet("old")
	if err != nil || !ok || v != "plain" {
		t.Fatalf("旧格式应能读：%v ok=%v err=%v", v, ok, err)
	}
}

// TestVectorNormIsReused 守"模长存一次、查询不重算"。
//
// 数值上必须和逐次重算完全一致：这是一次**性能**改动，不能带任何语义漂移。
func TestVectorNormIsReused(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	vecs := map[string][]float32{
		"a": {1, 0, 0},
		"b": {0.6, 0.8, 0},
		"c": {0, 1, 0},
	}
	for id, v := range vecs {
		if err := s.VectorPut(id, id, v, "m"); err != nil {
			t.Fatal(err)
		}
	}
	q := []float32{1, 0.2, 0}
	got, err := s.VectorQuery("", q, 3)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	stored := map[string]storedVector{}
	for k, v := range s.vectors {
		stored[k] = v
	}
	s.mu.Unlock()

	qn := l2norm(q)
	for _, m := range got {
		want := cosineWithNorm(q, qn, stored[m.ID].Vec, l2norm(stored[m.ID].Vec))
		if diff := m.Score - want; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("%s 的分数应等于逐次重算的结果：%v vs %v", m.ID, m.Score, want)
		}
	}
	// 存下来的模长必须非零（否则说明没写进去）。
	for _, v := range stored {
		if v.Norm == 0 {
			t.Fatal("写入时应记录模长")
		}
	}
}

// TestMemoryAndVectorDelete 守删除只影响自己、且删除不存在的键返回 false。
func TestMemoryAndVectorDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MemoryPut(nsKey("u", "k"), "v", 0); err != nil {
		t.Fatal(err)
	}
	ok, err := s.MemoryDelete(nsKey("other", "k"))
	if err != nil || ok {
		t.Fatalf("删别人的键应失败：ok=%v err=%v", ok, err)
	}
	if v, found, _ := s.MemoryGet(nsKey("u", "k")); !found || v != "v" {
		t.Fatal("别的命名空间的删除不该影响这个键")
	}
	if ok, err := s.MemoryDelete(nsKey("u", "k")); err != nil || !ok {
		t.Fatalf("应删除成功：ok=%v err=%v", ok, err)
	}
	if _, found, _ := s.MemoryGet(nsKey("u", "k")); found {
		t.Fatal("删除后应读不到")
	}
	if ok, _ := s.MemoryDelete(nsKey("u", "k")); ok {
		t.Fatal("删不存在的键应返回 false")
	}

	if err := s.VectorPut("u/a", "t", []float32{1, 0}, "m"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.VectorDelete("u/a"); err != nil || !ok {
		t.Fatalf("向量应删除成功：ok=%v err=%v", ok, err)
	}
	if got, _ := s.VectorQuery("u", []float32{1, 0}, 5); len(got) != 0 {
		t.Fatalf("删除后不该召回：%+v", got)
	}
}
