package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// 本文件覆盖“台账 JSON 的 requests 对象中同一请求编号出现多份记录”的损坏
// 场景：Go 的 map 无法表示重复键，正常解析会静默丢掉其中一份，使另一份
// 记录不再进入任何核对，重放该编号时返回哪份结果取决于文件排列顺序。
// 因此读取台账时只要发现重复请求编号就必须整体拒绝（ErrCorruptData，
// 可用 errors.Is 判断），不能挑一份继续、不能合并或删除重复项。

// setupRequestLedger 通过正常 API 建立一份台账：配方 R1/v1；批次 B1 执行中，
// 有两笔内容完全相同的投料（feed-1、feed-2，序号 1 与 2）；批次 B2 执行中，
// 有一笔投料（feed-3）。返回目录与当前完好的台账文件内容。
func setupRequestLedger(t *testing.T) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatalf("创建 B1 失败: %v", err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 5); err != nil {
		t.Fatalf("创建 B2 失败: %v", err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatalf("开始执行 B1 失败: %v", err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatalf("开始执行 B2 失败: %v", err)
	}
	t1 := feedingReplayTime()
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三"); err != nil {
		t.Fatalf("登记 feed-1 失败: %v", err)
	}
	// 同一批次中内容完全相同的第二笔投料，序号为 2。
	if _, err := s.AddFeeding("feed-2", "B1", "M1", "1.000", t1, "张三"); err != nil {
		t.Fatalf("登记 feed-2 失败: %v", err)
	}
	if _, err := s.AddFeeding("feed-3", "B2", "M1", "4", t1, "李四"); err != nil {
		t.Fatalf("登记 feed-3 失败: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭台账失败: %v", err)
	}
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	return dir, good
}

// rewriteRequestsWithDuplicate 把 dir 台账中请求编号 dupNo 的记录在 requests
// 对象里写成两份：第一份保持原样，第二份由 second 基于当前状态给出（可与原
// 记录完全相同，也可是另一请求的记录）。第二份的键按 dupKeyRaw 原文写出
// （已是带引号的 JSON 字符串），用于构造转义写法的同一编号；传空时与第一份
// 写法相同。返回写入后的文件内容。
func rewriteRequestsWithDuplicate(t *testing.T, dir, dupNo, dupKeyRaw string,
	second func(st *persistedState, orig *requestRecord) *requestRecord) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	orig := st.Requests[dupNo]
	if orig == nil {
		t.Fatalf("台账中不存在请求编号 %q", dupNo)
	}
	dup := second(&st, orig)
	if dupKeyRaw == "" {
		k, err := json.Marshal(dupNo)
		if err != nil {
			t.Fatal(err)
		}
		dupKeyRaw = string(k)
	}

	// requests 以外的部分正常序列化。
	head, err := json.Marshal(struct {
		Version  int             `json:"version"`
		Recipes  []*recipeRecord `json:"recipes"`
		Batches  []*batchRecord  `json:"batches"`
	}{st.Version, st.Recipes, st.Batches})
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	buf.Write(head[:len(head)-1]) // 去掉结尾 '}'
	buf.WriteString(`,"requests":{`)
	first := true
	writeEntry := func(rawKey string, rec *requestRecord) {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.WriteString(rawKey)
		buf.WriteByte(':')
		v, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(v)
	}
	reqNos := make([]string, 0, len(st.Requests))
	for reqNo := range st.Requests {
		reqNos = append(reqNos, reqNo)
	}
	sort.Strings(reqNos)
	for _, reqNo := range reqNos {
		k, err := json.Marshal(reqNo)
		if err != nil {
			t.Fatal(err)
		}
		if reqNo == dupNo {
			writeEntry(string(k), orig)
			writeEntry(dupKeyRaw, dup)
			continue
		}
		writeEntry(string(k), st.Requests[reqNo])
	}
	buf.WriteString("}}")
	out := buf.Bytes()
	if err := os.WriteFile(filepath.Join(dir, stateFileName), out, 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

// Open 时 requests 对象中同一请求编号出现两份记录就必须返回 ErrCorruptData：
// 两份记录的操作、提交内容与返回结果完全相同，提交内容相同但保存结果是同批
// 次的另一笔投料（序号为 2 的那条），第二份对应另一批次的请求，或第二份是
// 另一种操作的记录，四种情形都拒绝。错误信息需包含重复的请求编号；不返回
// 可用的台账对象，也不改写原文件；不得挑第一份或最后一份、合并或删除一份
// 后放行。
func TestOpenRejectsDuplicateRequestNo(t *testing.T) {
	cases := []struct {
		name   string
		second func(st *persistedState, orig *requestRecord) *requestRecord
	}{
		{"两份记录的操作提交内容与返回结果完全相同", func(st *persistedState, orig *requestRecord) *requestRecord {
			return orig
		}},
		{"提交内容相同但保存结果是同批次另一笔投料", func(st *persistedState, orig *requestRecord) *requestRecord {
			// 对应“两条内容相同、序号为 1 和 2 的投料，两份成功请求写成同一
			// 编号”的情形：第二份的保存结果是序号为 2 的那笔投料。
			return st.Requests["feed-2"]
		}},
		{"第二份对应另一批次的请求", func(st *persistedState, orig *requestRecord) *requestRecord {
			return st.Requests["feed-3"]
		}},
		{"第二份是另一种操作的记录", func(st *persistedState, orig *requestRecord) *requestRecord {
			return st.Requests["b1"]
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := setupRequestLedger(t)
			original := rewriteRequestsWithDuplicate(t, dir, "feed-1", "", tc.second)

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("重复请求编号应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			if !strings.Contains(err.Error(), "feed-1") {
				t.Fatalf("错误信息应包含重复的请求编号 %q，得到 %v", "feed-1", err)
			}
			// 原台账内容保留：不得挑一份、合并、删除或重新计算后另存。
			got, err := os.ReadFile(filepath.Join(dir, stateFileName))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, original) {
				t.Fatalf("拒绝打开不应改动台账文件")
			}
		})
	}
}

// 编号是否重复按 JSON 字符串解码后的实际编号判断：直接写出的 "feed-1" 与把
// 连字符写成 Unicode 转义的 "feed\u002d1" 是同一个编号，同样必须拒绝。
func TestOpenRejectsDuplicateRequestNoEscapedKey(t *testing.T) {
	dir, _ := setupRequestLedger(t)
	rewriteRequestsWithDuplicate(t, dir, "feed-1", `"feed\u002d1"`,
		func(st *persistedState, orig *requestRecord) *requestRecord { return orig })

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("转义写法的重复请求编号应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	if !strings.Contains(err.Error(), "feed-1") {
		t.Fatalf("错误信息应包含解码后的重复请求编号 %q，得到 %v", "feed-1", err)
	}
}

// 不同编号仍按精确匹配区分：大小写不同、前后多空格的编号都是另一个编号，
// 不做归一化，也不误判为重复。
func TestDuplicateRequestNoExactMatchOnly(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}
	t1 := feedingReplayTime()
	// 三个只是相近、并不相同的请求编号各自成功登记。
	for _, reqNo := range []string{"feed-1", "FEED-1", " feed-1"} {
		if _, err := s.AddFeeding(reqNo, "B1", "M1", "1", t1, "张三"); err != nil {
			t.Fatalf("相近但不同的请求编号 %q 应可正常使用: %v", reqNo, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭台账失败: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("相近但不同的请求编号不应误判为重复: %v", err)
	}
	defer s2.Close()
	view, err := s2.GetBatch("B1")
	if err != nil {
		t.Fatalf("重新打开后查询失败: %v", err)
	}
	if len(view.Feedings) != 3 {
		t.Fatalf("三笔投料都应保留，得到 %d 条", len(view.Feedings))
	}
}

// 台账打开后本地文件被改坏（requests 中多出一份同编号记录）：下一次查询或
// 写入都必须返回 ErrCorruptData，即使查询的是另一个正常批次或只访问配方；
// 不得凭此前读取过的内容返回旧结果，也不能用保存的请求结果绕过本次损坏；
// 被拒绝的写入不留业务记录、不占用请求编号；恢复后原记录继续可用。
func TestDuplicateRequestNoAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	t1 := feedingReplayTime()
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三"); err != nil {
		t.Fatal(err)
	}

	// 保存完好时的文件内容，随后把 feed-1 的请求记录在文件里写成两份。
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	badBytes := rewriteRequestsWithDuplicate(t, dir, "feed-1", "",
		func(st *persistedState, orig *requestRecord) *requestRecord { return orig })

	// 查询：重复编号涉及的批次、另一个正常批次、配方，全部失败；
	// 不能凭此前读取过的内容返回旧结果。
	if _, err := s.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("文件损坏后查询批次应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.GetBatch("B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("即使只访问另一个正常批次也必须拒绝，得到 %v", err)
	}
	if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("即使只查询配方也必须拒绝，得到 %v", err)
	}

	// 写入：投料、创建批次、登记配方都必须被拒绝。
	if _, err := s.AddFeeding("feed-new", "B1", "M1", "1", t1, "李四"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.CreateBatch("b3", "B3", "R1", "v1", 1); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上创建批次应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.RegisterRecipe("r2", "R2", "v1", "新配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上登记配方应返回 ErrCorruptData，得到 %v", err)
	}

	// 即使提交的是之前成功过的相同请求，也不能用保存的请求结果
	// 绕过这次台账损坏。
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上重放原成功请求也应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的操作不留痕迹：文件内容不变，请求编号未被占用。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, badBytes) {
		t.Fatalf("被拒绝的写入不应改动台账文件")
	}
	for _, kw := range []string{"feed-new", "B3", "R2"} {
		if bytes.Contains(after, []byte(kw)) {
			t.Fatalf("被拒绝的写入不应留下请求结果或业务记录（%q）", kw)
		}
	}

	// 恢复完好内容后，原有记录完整可用。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("恢复后查询应成功: %v", err)
	}
	if len(b1.Feedings) != 1 || b1.Feedings[0].Seq != 1 {
		t.Fatalf("恢复后批次投料应保持原样: %+v", b1.Feedings)
	}
	// 损坏前已成功的请求仍可幂等重放，返回首次成功的结果。
	replay, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	if replay.Seq != 1 {
		t.Fatalf("重放应返回第一次成功的结果，得到 seq=%d", replay.Seq)
	}
	// 被拒绝过的请求编号仍可用于合法提交。
	f, err := s.AddFeeding("feed-new", "B1", "M1", "1", t1, "李四")
	if err != nil {
		t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
	}
	if f.Seq != 2 {
		t.Fatalf("新投料应取得连续的下一个序号 2，得到 %d", f.Seq)
	}
}

// 正常台账不受本检查影响：没有成功请求记录的空台账仍可正常使用；同编号同
// 内容重复提交仍返回首次成功结果，同编号改换内容仍返回 ErrRequestConflict
// （不升级为 ErrCorruptData）。
func TestNormalLedgerRequestRulesUnchanged(t *testing.T) {
	// 没有任何成功请求记录的空台账。
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("空台账应可正常打开: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	t1 := feedingReplayTime()
	first, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatal(err)
	}
	// 同编号同内容：返回首次成功结果。
	replay, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("同编号同内容重放应成功: %v", err)
	}
	if replay.Seq != first.Seq {
		t.Fatalf("重放应返回首次成功结果，得到 seq=%d", replay.Seq)
	}
	// 同编号不同内容：ErrRequestConflict，不是 ErrCorruptData。
	_, err = s.AddFeeding("feed-1", "B1", "M1", "2", t1, "张三")
	if !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号不同内容应返回 ErrRequestConflict，得到 %v", err)
	}
	if errors.Is(err, ErrCorruptData) {
		t.Fatalf("正常台账上的请求冲突不应归为数据损坏，得到 %v", err)
	}
}
