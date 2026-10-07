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

// 本文件是“台账最外层的请求记录字段 requests 只能出现一次”的回归保障：
// 标准库把顶层对象解码进 persistedState 时，每出现一次 requests 就对
// Requests 字段调一次 UnmarshalJSON 并整体替换——后一段覆盖前一段，第一
// 次登记保存的请求结果会静默消失；调用方再拿原投料请求提交时会被当成新
// 登记，平白增加一条投料。打开台账及之后每次查询、写入的重新读取，只要
// 最外层 requests 出现第二次（直接写出、Unicode 转义解码后指向 requests，
// 或现有读取能识别的大小写写法），就必须以 ErrCorruptData 拒绝整份台账：
// 两段内容相同、各自保存不同编号，或其中一段为空对象、null 都不能选择、
// 拼接或重建；记录内部嵌套对象里的同名字段则不算最外层重复。被拒绝的
// 操作保留原文件、不返回局部结果、不增加投料或改变批次状态，新写入也不
// 占用请求编号。

// rebuildTopLevelLedger 按给定的顶层字段原文重新拼出整份台账：version、
// recipes、batches 取自正常台账，最外层放入两个请求记录字段成员（键为
// 已带引号的 JSON 键原文，可写成 Unicode 转义或不同大小写；值为各段
// requests 对象原文）。标准序列化不可能在同一对象产生两个同名成员，因此
// 这里手工拼接顶层对象原文。
func rebuildTopLevelLedger(t *testing.T, good []byte, firstKey, secondKey string, firstVal, secondVal json.RawMessage) []byte {
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
	var buf bytes.Buffer
	buf.WriteString(`{"version":`)
	buf.WriteString(strconv.Itoa(raw.Version))
	buf.WriteString(`,"recipes":`)
	buf.Write(raw.Recipes)
	buf.WriteString(`,"batches":`)
	buf.Write(raw.Batches)
	buf.WriteByte(',')
	buf.WriteString(firstKey)
	buf.WriteByte(':')
	buf.Write(firstVal)
	buf.WriteByte(',')
	buf.WriteString(secondKey)
	buf.WriteByte(':')
	buf.Write(secondVal)
	buf.WriteByte('}')
	return buf.Bytes()
}

// requestsEntries 把正常台账的 requests 对象解成“编号 → 记录原文”的映射。
func requestsEntries(t *testing.T, good []byte) map[string]json.RawMessage {
	t.Helper()
	var raw struct {
		Requests json.RawMessage `json:"requests"`
	}
	if err := json.Unmarshal(good, &raw); err != nil {
		t.Fatal(err)
	}
	var reqs map[string]json.RawMessage
	if err := json.Unmarshal(raw.Requests, &reqs); err != nil {
		t.Fatal(err)
	}
	return reqs
}

// marshalRequestsEntries 把一部分请求记录重新序列化成一个 requests 对象
// 原文（映射序列化按键排序，内容与原记录逐字节语义一致）。
func marshalRequestsEntries(t *testing.T, entries map[string]json.RawMessage, nos ...string) json.RawMessage {
	t.Helper()
	part := make(map[string]json.RawMessage, len(nos))
	for _, no := range nos {
		v, ok := entries[no]
		if !ok {
			t.Fatalf("正常台账中应存在 %q 请求记录", no)
		}
		part[no] = v
	}
	data, err := json.Marshal(part)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// checkDuplicateTopLevelRequestsOpen 断言打开 dir 中的台账以 ErrCorruptData
// 拒绝：错误信息明确指出最外层请求记录字段重复并写出 requests，不返回可用
// 对象，且台账文件保持 rejected 原文不变。
func checkDuplicateTopLevelRequestsOpen(t *testing.T, dir string, rejected []byte) {
	t.Helper()
	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("最外层请求记录字段重复应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"最外层", "requests", "重复"} {
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

// writeLedger 把 data 写入 dir 的台账文件。
func writeLedger(t *testing.T, dir string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, stateFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// 最外层 requests 写了两次、两段内容完全相同时，打开台账必须返回
// ErrCorruptData：不能挑其中一段继续，即使两段逐字节相同。
func TestOpenRejectsDuplicateTopLevelRequestsIdentical(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	var raw struct {
		Requests json.RawMessage `json:"requests"`
	}
	if err := json.Unmarshal(good, &raw); err != nil {
		t.Fatal(err)
	}
	bad := rebuildTopLevelLedger(t, good, `"requests"`, `"requests"`, raw.Requests, raw.Requests)
	writeLedger(t, dir, bad)
	checkDuplicateTopLevelRequestsOpen(t, dir, bad)
}

// 两段各自保存不同的请求编号时同样必须拒绝：不能把两段拼接成一份完整
// 请求记录，也不能只取其中一段（否则第一次登记保存的结果仍会丢失）。
func TestOpenRejectsDuplicateTopLevelRequestsDisjoint(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	entries := requestsEntries(t, good)
	// 第一段只放 feed-1，第二段放其余全部编号；任一段单独都不足以代表
	// 整份台账，拼接两段才“完整”——但拼接同样不允许。
	first := marshalRequestsEntries(t, entries, "feed-1")
	second := marshalRequestsEntries(t, entries,
		"recipe-1", "b1", "s1", "b2", "s2", "feed-2", "feed-b2")
	bad := rebuildTopLevelLedger(t, good, `"requests"`, `"requests"`, first, second)
	writeLedger(t, dir, bad)
	checkDuplicateTopLevelRequestsOpen(t, dir, bad)
}

// 其中一段为空对象或 null（无论出现在前还是在后）都不能选择非空的一段
// 继续，也不能把 null/空对象当成“没写”：第二次出现即损坏。
func TestOpenRejectsDuplicateTopLevelRequestsEmptyOrNull(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	var raw struct {
		Requests json.RawMessage `json:"requests"`
	}
	if err := json.Unmarshal(good, &raw); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		firstKey, secondKey string
		first, second       json.RawMessage
	}{
		"空对象在前":  {`"requests"`, `"requests"`, json.RawMessage(`{}`), raw.Requests},
		"空对象在后":  {`"requests"`, `"requests"`, raw.Requests, json.RawMessage(`{}`)},
		"null在前": {`"requests"`, `"requests"`, json.RawMessage(`null`), raw.Requests},
		"null在后": {`"requests"`, `"requests"`, raw.Requests, json.RawMessage(`null`)},
		"两段皆空":   {`"requests"`, `"requests"`, json.RawMessage(`null`), json.RawMessage(`{}`)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			setupFeedingAliasLedger(t, dir)
			bad := rebuildTopLevelLedger(t, good, tc.firstKey, tc.secondKey, tc.first, tc.second)
			writeLedger(t, dir, bad)
			checkDuplicateTopLevelRequestsOpen(t, dir, bad)
		})
	}
}

// 第二段把 requests 中的字母 q 写成 Unicode 转义形式（键原文为
// "requests"，含反斜杠），解码后与直接写出的 requests 是同一
// 字段名：必须识别为最外层重复并拒绝。
func TestOpenRejectsDuplicateTopLevelRequestsEscapedName(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	var raw struct {
		Requests json.RawMessage `json:"requests"`
	}
	if err := json.Unmarshal(good, &raw); err != nil {
		t.Fatal(err)
	}
	// escapedKey 是 JSON 键原文：外层一对引号，内部把字母 q 写成其
	// Unicode 转义六个字符。用拼接构造，使 Go 源码中不直接出现该转义。
	esc := `\u` + `0071` // 解码到 JSON 字符串后是字母 q
	escapedKey := `"re` + esc + `uests"`
	if !bytes.Contains([]byte(escapedKey), []byte(esc)) {
		t.Fatalf("测试键应保留 Unicode 转义原文，得到 %q", escapedKey)
	}
	if kv, err := strconv.Unquote(escapedKey); err != nil || kv != "requests" {
		t.Fatalf("转义键解码后应为 requests，得到 %q（err=%v）", kv, err)
	}
	bad := rebuildTopLevelLedger(t, good, `"requests"`, escapedKey, raw.Requests, json.RawMessage(`{}`))
	writeLedger(t, dir, bad)
	checkDuplicateTopLevelRequestsOpen(t, dir, bad)
}

// 现有读取能识别的大小写写法（Requests、REQUESTS）同样不能用来让
// requests 第二次出现：字段匹配按标准库同一套大小写折叠规则判断。
func TestOpenRejectsDuplicateTopLevelRequestsCaseVariants(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	var raw struct {
		Requests json.RawMessage `json:"requests"`
	}
	if err := json.Unmarshal(good, &raw); err != nil {
		t.Fatal(err)
	}
	for _, secondKey := range []string{`"Requests"`, `"REQUESTS"`, `"ReQuEsTs"`} {
		secondKey := secondKey
		t.Run(secondKey, func(t *testing.T) {
			dir := t.TempDir()
			setupFeedingAliasLedger(t, dir)
			bad := rebuildTopLevelLedger(t, good, `"requests"`, secondKey, raw.Requests, json.RawMessage(`{}`))
			writeLedger(t, dir, bad)
			checkDuplicateTopLevelRequestsOpen(t, dir, bad)
		})
	}
}

// 台账正常打开后文件才被改坏（最外层 requests 写成两段）：下一次查询或
// 写入重新读取时必须返回 ErrCorruptData——即使访问另一个正常批次 B2、
// 或重放原本成功的请求，都不能取旧结果继续；被拒绝的新写入不留业务
// 记录、不占用请求编号、不改变批次状态。恢复原文件后，原请求仍各自取回
// 第一次成功的结果（不新增投料），被拒绝过的新编号可正常使用。
func TestDuplicateTopLevelRequestsDetectedOnEveryReload(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	tm := feedingAliasBaseTime()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var raw struct {
		Requests json.RawMessage `json:"requests"`
	}
	if err := json.Unmarshal(good, &raw); err != nil {
		t.Fatal(err)
	}
	bad := rebuildTopLevelLedger(t, good, `"requests"`, `"requests"`,
		raw.Requests, json.RawMessage(`{}`))
	writeLedger(t, dir, bad)

	// 查询损坏台账涉及的批次 B1 与另一个正常批次 B2 都必须失败，不能沿用
	// 打开时读到的内存内容。
	for _, batchNo := range []string{"B1", "B2"} {
		v, err := s.GetBatch(batchNo)
		if !errors.Is(err, ErrCorruptData) {
			t.Fatalf("损坏未修复时查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		}
		if v != nil {
			t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
		}
	}
	// 重放原本成功的请求：load 先于幂等重放执行，不能取回旧的第一次成功
	// 结果继续返回。
	for _, reqNo := range []string{"feed-1", "feed-2"} {
		if _, err := s.AddFeeding(reqNo, "B1", "M1", "1.000", tm, "张三"); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("重放请求 %q 应先在重新读取时返回 ErrCorruptData，得到 %v", reqNo, err)
		}
	}
	// 对正常批次 B2 的幂等重放与新写入同样不能绕过。
	if _, err := s.AddFeeding("feed-b2", "B2", "M1", "1.000", tm, "张三"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上重放正常批次的请求应返回 ErrCorruptData，得到 %v", err)
	}
	newFeeding, err := s.AddFeeding("feed-x", "B2", "M1", "1.000", tm, "钱七")
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上写入正常批次应返回 ErrCorruptData，得到 %v", err)
	}
	if newFeeding != nil {
		t.Fatalf("被拒绝的写入不应返回投料结果: %+v", newFeeding)
	}

	// 被拒绝的访问不改动文件：投料不增加、批次状态不变、新写入不留下请求
	// 记录或占用请求编号。
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

	// 恢复原文件后：原请求各自取回第一次成功的结果（序号 1、2），不新增
	// 投料；同编号改换内容仍是 ErrRequestConflict；被拒绝过的新编号可以
	// 正常使用，取得连续序号 3。
	writeLedger(t, dir, good)
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
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Feedings) != 2 || view.Status != StatusExecuting {
		t.Fatalf("被拒绝的写入不应增加投料或改变批次状态，得到 %d 条投料、状态 %s",
			len(view.Feedings), view.Status)
	}
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "2.000", tm, "张三"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号改换内容应返回 ErrRequestConflict，得到 %v", err)
	}
	r3, err := s.AddFeeding("feed-x", "B1", "M1", "1.000", tm, "张三")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号恢复后应可正常使用: %v", err)
	}
	checkFeedingView(t, r3, 3, "M1", "1", tm, "张三")
}

// 兼容性：只有一段 requests 时，即使字段名整段用 Unicode 转义写法或大小
// 写变体写出，多条不同编号的成功请求仍可读取，原请求重放仍返回第一次
// 成功结果——现有读取本来就识别这些写法。
func TestSingleTopLevelRequestsWithEscapeOrCaseStillUsable(t *testing.T) {
	tm := feedingAliasBaseTime()
	esc := `\u` + `0071`
	for name, key := range map[string]string{
		"Unicode转义写法": `"re` + esc + `uests"`,
		"大小写变体":       `"Requests"`,
	} {
		key := key
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupFeedingAliasLedger(t, dir)
			var raw struct {
				Version  int             `json:"version"`
				Recipes  json.RawMessage `json:"recipes"`
				Batches  json.RawMessage `json:"batches"`
				Requests json.RawMessage `json:"requests"`
			}
			if err := json.Unmarshal(good, &raw); err != nil {
				t.Fatal(err)
			}
			// 只有一段请求记录字段，仅键的写法不同。
			data := rebuildTopLevelLedgerSingle(t, raw.Version, raw.Recipes, raw.Batches, key, raw.Requests)
			writeLedger(t, dir, data)

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("只有一段请求记录字段（写法 %s）应能打开: %v", name, err)
			}
			defer s.Close()
			view, err := s.GetBatch("B1")
			if err != nil {
				t.Fatal(err)
			}
			if len(view.Feedings) != 2 {
				t.Fatalf("多条不同编号的成功请求仍应可读，B1 应为 2 条投料，得到 %d 条", len(view.Feedings))
			}
			r1, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", tm, "张三")
			if err != nil {
				t.Fatalf("原请求重放应返回第一次成功结果: %v", err)
			}
			checkFeedingView(t, r1, 1, "M1", "1", tm, "张三")
			again, _ := s.GetBatch("B1")
			if len(again.Feedings) != 2 {
				t.Fatalf("重放不应新增投料，得到 %d 条", len(again.Feedings))
			}
		})
	}
}

// rebuildTopLevelLedgerSingle 拼出只有一段请求记录字段的台账（键写法可
// 自定义），用于兼容性用例。
func rebuildTopLevelLedgerSingle(t *testing.T, version int, recipes, batches json.RawMessage, key string, requests json.RawMessage) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString(`{"version":`)
	buf.WriteString(strconv.Itoa(version))
	buf.WriteString(`,"recipes":`)
	buf.Write(recipes)
	buf.WriteString(`,"batches":`)
	buf.Write(batches)
	buf.WriteByte(',')
	buf.WriteString(key)
	buf.WriteByte(':')
	buf.Write(requests)
	buf.WriteByte('}')
	return buf.Bytes()
}

// 兼容性：requests 缺失、为空对象或 null（只出现或不出现一次）沿用原行为，
// 台账可正常打开与写入。
func TestTopLevelRequestsMissingOnceStillUsable(t *testing.T) {
	cases := map[string]string{
		"缺失":   `{"version":1,"recipes":[],"batches":[]}`,
		"空对象":  `{"version":1,"recipes":[],"batches":[],"requests":{}}`,
		"null": `{"version":1,"recipes":[],"batches":[],"requests":null}`,
	}
	for name, data := range cases {
		data := data
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeLedger(t, dir, []byte(data))
			s, err := Open(dir)
			if err != nil {
				t.Fatalf("requests %s 的空台账应能打开: %v", name, err)
			}
			defer s.Close()
			registerStandardRecipe(t, s, "recipe-1")
			if _, err := s.GetRecipe("R1", "v1"); err != nil {
				t.Fatalf("空台账应可正常写入并查询: %v", err)
			}
		})
	}
}

// 兼容性：请求记录内部（某条请求记录对象里）出现同名的 requests 键不算
// 最外层重复，不得因此拒绝正常台账。
func TestNestedRequestsKeyInsideRecordNotRejected(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)

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
	// 在 feed-1 这条请求记录对象内部加入一个同名键（该键不属于
	// requestRecord 的已知字段，解码时忽略）。
	var entry map[string]json.RawMessage
	if err := json.Unmarshal(reqs["feed-1"], &entry); err != nil {
		t.Fatal(err)
	}
	entry["requests"] = json.RawMessage(`{"nested":true}`)
	entryBytes, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	reqs["feed-1"] = entryBytes
	reqsBytes, err := json.Marshal(reqs)
	if err != nil {
		t.Fatal(err)
	}
	data := rebuildTopLevelLedgerSingle(t, raw.Version, raw.Recipes, raw.Batches, `"requests"`, reqsBytes)
	writeLedger(t, dir, data)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("记录内部的同名字段不应判为最外层重复: %v", err)
	}
	defer s.Close()
	tm := feedingAliasBaseTime()
	r1, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", tm, "张三")
	if err != nil {
		t.Fatalf("正常台账的请求重放应成功: %v", err)
	}
	checkFeedingView(t, r1, 1, "M1", "1", tm, "张三")
}

// 兼容性：只有一段 requests 时，多条不同编号的成功请求正常读取，同一编号
// 相同内容重复提交仍返回首次成功结果（既有行为不被新校验破坏）。
func TestSingleRequestsSegmentIdempotencyPreserved(t *testing.T) {
	dir := t.TempDir()
	setupFeedingAliasLedger(t, dir)
	tm := feedingAliasBaseTime()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r1, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", tm, "张三")
	if err != nil {
		t.Fatal(err)
	}
	checkFeedingView(t, r1, 1, "M1", "1", tm, "张三")
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Feedings) != 2 {
		t.Fatalf("单段 requests 的正常台账重放不应新增投料，得到 %d 条", len(view.Feedings))
	}
}
