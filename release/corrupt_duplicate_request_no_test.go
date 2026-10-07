package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 本文件是“台账 requests 对象中同一请求编号只能有一份记录”的回归保障：
// 某执行中批次有两条内容相同、序号为 1 和 2 的投料，若两份保存的成功投料
// 请求在文件里被写成同一个编号，标准库把 JSON 对象读进 map 时只会静默保留
// 其中一份，另一份脱离一切核对——重放该编号可能返回后写入的第 2 条结果，
// 而不是首次成功的第 1 条。读取已有台账时（打开及之后每次查询、写入的重新
// 读取），只要 requests 对象中同一请求编号出现两次或更多次，就必须以
// ErrCorruptData 拒绝整份台账，错误信息包含重复编号并说明存在多份同编号
// 请求记录：两份记录完全相同要拒绝，内容不同、对应不同批次或不同操作同样
// 如此；不能挑第一份或最后一份继续，不能合并、删除重复项或重新计算结果后
// 掩盖冲突。被拒绝的操作保留原文件、不留业务变更、不占用新的请求编号。

// rewriteRequestsWithDuplicate 把台账文件的 requests 对象改写为 target 编号
// 出现两份记录：第一份保留原记录原文，第二份以 dupKey（JSON 字符串原文，
// 可写成转义形式）为键、dupEntry 为值（传 nil 表示与原记录完全相同），其余
// 请求记录保持原样。返回改写后的文件内容。标准 map 序列化不可能产生重复键，
// 因此这里手工拼接 requests 对象原文。
func rewriteRequestsWithDuplicate(t *testing.T, good []byte, target, dupKey string, dupEntry []byte) []byte {
	t.Helper()
	var raw struct {
		Version  int             `json:"version"`
		Recipes  json.RawMessage `json:"recipes"`
		Batches  json.RawMessage `json:"batches"`
		Requests json.RawMessage `json:"requests"`
	}
	if err := json.Unmarshal(good, &raw); err != nil {
		t.Fatal(err)
	}
	var reqs map[string]json.RawMessage
	if err := json.Unmarshal(raw.Requests, &reqs); err != nil {
		t.Fatal(err)
	}
	entry, ok := reqs[target]
	if !ok {
		t.Fatalf("正常台账中应存在 %q 请求记录", target)
	}
	if dupEntry == nil {
		dupEntry = entry
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	buf.WriteString(strconv.Quote(target))
	buf.WriteByte(':')
	buf.Write(entry)
	buf.WriteByte(',')
	buf.WriteString(dupKey)
	buf.WriteByte(':')
	buf.Write(dupEntry)
	for k, v := range reqs {
		if k == target {
			continue
		}
		buf.WriteByte(',')
		buf.WriteString(strconv.Quote(k))
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	raw.Requests = buf.Bytes()
	out, err := json.Marshal(&raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// checkDuplicateRequestNoOpen 断言打开 dir 中的台账以 ErrCorruptData 拒绝，
// 错误信息包含重复编号并说明存在多份同编号请求记录，不返回可用对象，
// 且台账文件保持 rejected 原文不变。
func checkDuplicateRequestNoOpen(t *testing.T, dir, dupNo string, rejected []byte) {
	t.Helper()
	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("同一请求编号存在多份记录应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{dupNo, "多份同编号请求记录"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, rejected) {
		t.Fatalf("拒绝打开不应改动台账文件")
	}
}

// 两份保存的成功投料请求被写成同一个编号（操作、提交内容与返回结果完全相同）
// 时，打开台账必须返回 ErrCorruptData：不能挑第一份或最后一份继续使用。
func TestOpenRejectsDuplicateRequestNoIdenticalRecords(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	bad := rewriteRequestsWithDuplicate(t, good, "feed-1", `"feed-1"`, nil)
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	checkDuplicateRequestNoOpen(t, dir, "feed-1", bad)
}

// 编号是否重复按 JSON 字符串解码后的实际内容判断：第二份记录的键把连字符
// 写成 Unicode 转义形式，解码后与 "feed-1" 是同一个编号，同样必须拒绝。
func TestOpenRejectsDuplicateRequestNoEscapedKey(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	// escapedKey 把键中的连字符写成 Unicode 转义形式，解码后与 "feed-1"
	// 表示同一个编号。
	escapedKey := `"feed` + "\\u002d" + `1"`
	bad := rewriteRequestsWithDuplicate(t, good, "feed-1", escapedKey, nil)
	if !bytes.Contains(bad, []byte(escapedKey)) {
		t.Fatalf("改坏后的文件应包含转义写法的重复键")
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	checkDuplicateRequestNoOpen(t, dir, "feed-1", bad)
}

// 两份同编号记录的内容不同（不同操作、不同提交内容）同样必须拒绝：重复
// 判断只看请求编号，与记录内容是否一致无关。
func TestOpenRejectsDuplicateRequestNoDifferentContent(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	dup, err := json.Marshal(map[string]string{
		"op":      "createBatch",
		"payload": `{"batchNo":"B9","recipeNo":"R1","version":"v1","portions":3}`,
		"result":  `{"batchNo":"B9"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	bad := rewriteRequestsWithDuplicate(t, good, "feed-1", `"feed-1"`, dup)
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	checkDuplicateRequestNoOpen(t, dir, "feed-1", bad)
}

// 台账正常打开后文件才被改坏（feed-1 的请求记录被写成两份）：下一次查询或
// 写入重新读取时必须返回 ErrCorruptData——即使只访问另一个正常批次 B2 也
// 不能绕过；被拒绝的重放不得返回任何一份保存结果，被拒绝的新写入不留业务
// 记录、不占用请求编号。恢复后原请求各自取回第一次成功的结果，同编号改换
// 内容仍返回 ErrRequestConflict，被拒绝过的新编号仍可正常使用。
func TestDuplicateRequestNoDetectedOnEveryReload(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	tm := feedingAliasBaseTime()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bad := rewriteRequestsWithDuplicate(t, good, "feed-1", `"feed-1"`, nil)
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// 查询损坏批次与另一个正常批次都必须失败，不能沿用此前读到的内容。
	for _, batchNo := range []string{"B1", "B2"} {
		v, err := s.GetBatch(batchNo)
		if !errors.Is(err, ErrCorruptData) {
			t.Fatalf("损坏未修复时查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		}
		if v != nil {
			t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
		}
	}
	// 用原编号重放：load 先于幂等重放执行，不能把任何一份同编号记录的结果
	// 当成第一次成功的结果返回。
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", tm, "张三"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("重放重复编号请求应先在重新读取时返回 ErrCorruptData，得到 %v", err)
	}
	// 对正常批次 B2 的幂等重放与新写入同样不能绕过。
	if _, err := s.AddFeeding("feed-b2", "B2", "M1", "1.000", tm, "张三"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上重放正常批次的请求应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.AddFeeding("feed-x", "B2", "M1", "1.000", tm, "钱七"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上写入正常批次应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的访问不改动文件，新写入不留下请求记录、不占用请求编号。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("被拒绝的访问不应改动台账文件")
	}
	if bytes.Contains(after, []byte("feed-x")) {
		t.Fatalf("被拒绝的写入不应留下请求记录或占用请求编号")
	}

	// 恢复原文件后：两个原请求分别取回各自第一次成功的记录（序号 1、2）；
	// 同编号改换内容仍是 ErrRequestConflict；被拒绝过的新编号可以正常使用。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	r1, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", tm, "张三")
	if err != nil {
		t.Fatalf("恢复后重放 feed-1 失败: %v", err)
	}
	checkFeedingView(t, r1, 1, "M1", "1", tm, "张三")
	r2, err := s.AddFeeding("feed-2", "B1", "M1", "1.000", tm, "张三")
	if err != nil {
		t.Fatalf("恢复后重放 feed-2 失败: %v", err)
	}
	checkFeedingView(t, r2, 2, "M1", "1", tm, "张三")
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "2.000", tm, "张三"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号改换内容应返回 ErrRequestConflict，得到 %v", err)
	}
	r3, err := s.AddFeeding("feed-x", "B1", "M1", "1.000", tm, "张三")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号恢复后应可正常使用: %v", err)
	}
	checkFeedingView(t, r3, 3, "M1", "1", tm, "张三")
}

// 没有成功请求记录的正常台账仍可使用：requests 保存为空对象或 null 都不算
// 重复，打开、查询与后续写入一切正常。
func TestEmptyRequestsObjectStillUsable(t *testing.T) {
	for name, requests := range map[string]string{
		"空对象": `{}`,
		"null": `null`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			data := []byte(`{"version":1,"recipes":[],"batches":[],"requests":` + requests + `}`)
			if err := os.WriteFile(filepath.Join(dir, stateFileName), data, 0o644); err != nil {
				t.Fatal(err)
			}
			s, err := Open(dir)
			if err != nil {
				t.Fatalf("没有成功请求记录的空台账应能打开: %v", err)
			}
			defer s.Close()
			if _, err := s.GetBatch("B1"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("空台账查询批次应返回 ErrNotFound，得到 %v", err)
			}
			registerStandardRecipe(t, s, "recipe-1")
			if _, err := s.GetRecipe("R1", "v1"); err != nil {
				t.Fatalf("空台账应可正常写入并查询: %v", err)
			}
		})
	}
}
