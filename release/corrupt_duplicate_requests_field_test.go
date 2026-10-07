package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件是“同一份台账最外层请求记录字段 requests 只能出现一次”的回归保障：
// requests 对象内部同一请求编号写两次已被拒绝，但标准库把 JSON 对象读进
// 结构体时，最外层同名字段写两次只会静默地用后一段覆盖前一段——台账最外层
// 若写了两次 requests，前一段保存的成功请求结果就此丢失，而已有配方、批次
// 与投料仍在；调用方按原投料请求重放时会被当成新登记，再增加一条投料。
// 读取台账时（打开及之后每次查询、写入的重新读取），最外层请求记录字段
// 出现第二次即必须以 ErrCorruptData 拒绝整份台账：两段内容相同、各自保存
// 不同编号，或其中一段为空对象、null 都不能挑选、拼接两段或重建请求结果；
// 字段名中的字符写成 Unicode 码位转义（解码后仍是 requests）或现有读取
// 能识别的大小写变体同样不能绕过。被拒绝的操作保留原文件、不返回局部
// 查询结果、不留业务变更、不占用请求编号。只有一段 requests 的正常台账
// （含缺失、空对象或 null）行为保持不变；嵌套记录内部
// 出现同名字段不算最外层重复。

// stateTopLevel 保留台账最外层各字段的 JSON 原文，便于手工重组含两段
// requests 字段的文件（标准序列化不可能产生重复字段）。
type stateTopLevel struct {
	Version  json.RawMessage `json:"version"`
	Recipes  json.RawMessage `json:"recipes"`
	Batches  json.RawMessage `json:"batches"`
	Requests json.RawMessage `json:"requests"`
}

// buildTopLevelWithDuplicateRequests 手工拼出最外层含两段请求记录字段的
// 台账原文。firstKey/secondKey 是 JSON 键的完整原文（含引号，可写成
// Unicode 转义或大小写变体，如 `"requests"`、含 q 转义的写法、
// `"Requests"`）；firstVal/secondVal 是两段字段值的原文。其余字段
// （version、recipes、batches）沿用 good 台账。
func buildTopLevelWithDuplicateRequests(t *testing.T, good []byte, firstKey string, firstVal json.RawMessage, secondKey string, secondVal json.RawMessage) []byte {
	t.Helper()
	var raw stateTopLevel
	if err := json.Unmarshal(good, &raw); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	buf.WriteString(`"version":`)
	buf.Write(raw.Version)
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
	// 拼出的文件本身必须是合法 JSON（语义上损坏，但能解析），否则测到的
	// 只是一般的解析失败，而不是重复字段规则。
	var probe any
	if err := json.Unmarshal(buf.Bytes(), &probe); err != nil {
		t.Fatalf("拼出的重复字段台账应为合法 JSON: %v", err)
	}
	return buf.Bytes()
}

// requestsEntries 把台账的 requests 对象解析成以编号为键的原文表。
func requestsEntries(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var reqs map[string]json.RawMessage
	if err := json.Unmarshal(raw, &reqs); err != nil {
		t.Fatal(err)
	}
	return reqs
}

// marshalRequestsSubset 从完整 requests 原文中挑出 keys 指定的编号，
// 序列化成一个新的 requests 对象原文。
func marshalRequestsSubset(t *testing.T, raw json.RawMessage, keys ...string) json.RawMessage {
	t.Helper()
	all := requestsEntries(t, raw)
	sub := map[string]json.RawMessage{}
	for _, k := range keys {
		v, ok := all[k]
		if !ok {
			t.Fatalf("正常台账中应存在 %q 请求记录", k)
		}
		sub[k] = v
	}
	out, err := json.Marshal(sub)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// checkDuplicateRequestsFieldOpen 断言打开 dir 中的台账以 ErrCorruptData
// 拒绝：errors.Is 可判定、不返回可用对象、错误信息明确指出最外层请求记录
// 字段重复并写出规范名 requests，且台账文件保持 rejected 原文不变。
func checkDuplicateRequestsFieldOpen(t *testing.T, dir string, rejected []byte) {
	t.Helper()
	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("最外层 requests 字段重复应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"requests", "最外层", "重复"} {
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

// 两段 requests 内容完全相同（同一段原文写两次）时，打开台账必须返回
// ErrCorruptData：不能挑第一份或最后一份继续。
func TestOpenRejectsDuplicateRequestsFieldIdentical(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	var raw stateTopLevel
	if err := json.Unmarshal(good, &raw); err != nil {
		t.Fatal(err)
	}
	bad := buildTopLevelWithDuplicateRequests(t, good, `"requests"`, raw.Requests, `"requests"`, raw.Requests)
	writeLedger(t, dir, bad)
	checkDuplicateRequestsFieldOpen(t, dir, bad)
}

// 两段 requests 各自保存不同编号（第一段含 feed-1，第二段含 feed-2 等
// 其余请求）时同样必须拒绝：不能拼接两段；否则第一段登记的投料请求结果
// 会整段丢失，而投料仍在，重放原请求会被当成新登记再投一次料。
func TestOpenRejectsDuplicateRequestsFieldDifferentSegments(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	var raw stateTopLevel
	if err := json.Unmarshal(good, &raw); err != nil {
		t.Fatal(err)
	}
	first := marshalRequestsSubset(t, raw.Requests, "feed-1")
	second := marshalRequestsSubset(t, raw.Requests, "feed-2", "feed-b2", "b1", "b2", "s1", "s2", "recipe-1")
	bad := buildTopLevelWithDuplicateRequests(t, good, `"requests"`, first, `"requests"`, second)
	writeLedger(t, dir, bad)
	checkDuplicateRequestsFieldOpen(t, dir, bad)
}

// 其中一段为空对象时不能把另一段当作唯一的请求记录：空段在前或在后都
// 必须拒绝，两种排列各测一次。
func TestOpenRejectsDuplicateRequestsFieldEmptyObject(t *testing.T) {
	for _, emptyFirst := range []bool{true, false} {
		name := "空对象在后"
		if emptyFirst {
			name = "空对象在前"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupFeedingAliasLedger(t, dir)
			var raw stateTopLevel
			if err := json.Unmarshal(good, &raw); err != nil {
				t.Fatal(err)
			}
			var bad []byte
			if emptyFirst {
				bad = buildTopLevelWithDuplicateRequests(t, good, `"requests"`, []byte(`{}`), `"requests"`, raw.Requests)
			} else {
				bad = buildTopLevelWithDuplicateRequests(t, good, `"requests"`, raw.Requests, `"requests"`, []byte(`{}`))
			}
			writeLedger(t, dir, bad)
			checkDuplicateRequestsFieldOpen(t, dir, bad)
		})
	}
}

// 其中一段为 null 时同样不能选择另一段：null 在前或在后都必须拒绝。
func TestOpenRejectsDuplicateRequestsFieldNullSegment(t *testing.T) {
	for _, nullFirst := range []bool{true, false} {
		name := "null在后"
		if nullFirst {
			name = "null在前"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupFeedingAliasLedger(t, dir)
			var raw stateTopLevel
			if err := json.Unmarshal(good, &raw); err != nil {
				t.Fatal(err)
			}
			var bad []byte
			if nullFirst {
				bad = buildTopLevelWithDuplicateRequests(t, good, `"requests"`, []byte(`null`), `"requests"`, raw.Requests)
			} else {
				bad = buildTopLevelWithDuplicateRequests(t, good, `"requests"`, raw.Requests, `"requests"`, []byte(`null`))
			}
			writeLedger(t, dir, bad)
			checkDuplicateRequestsFieldOpen(t, dir, bad)
		})
	}
}

// 第二个字段名把 q 写成 Unicode 码位转义（文件里该键为 re 加转义序列加
// uests，JSON 解码后仍是 requests）时必须识别为同一个请求记录字段，
// 不能靠转义写法绕过；转义字段在前、正常写法在后同样拒绝。
func TestOpenRejectsDuplicateRequestsFieldEscapedName(t *testing.T) {
	normal := `"requests"`
	// 拆成两段书写，避免任何一段本身就构成完整的 Unicode 转义序列；
	// 两段在运行时拼出 JSON 键原文，其中 q 以六位码位转义写出，
	// 经 JSON 解码后得到的字段名仍是 requests。
	escaped := `"re\u00` + `71uests"`
	cases := map[string][2]string{
		"转义写法在后": {normal, escaped},
		"转义写法在前": {escaped, normal},
	}
	for name, keys := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupFeedingAliasLedger(t, dir)
			var raw stateTopLevel
			if err := json.Unmarshal(good, &raw); err != nil {
				t.Fatal(err)
			}
			bad := buildTopLevelWithDuplicateRequests(t, good, keys[0], raw.Requests, keys[1], raw.Requests)
			if !bytes.Contains(bad, []byte(`\u00`+`71`)) {
				t.Fatalf("改坏后的文件应包含 Unicode 转义写法的字段名")
			}
			writeLedger(t, dir, bad)
			checkDuplicateRequestsFieldOpen(t, dir, bad)
		})
	}
}

// 现有读取能识别的大小写写法（Requests、REQUESTS 等）同样指向 requests
// 字段，第二次出现必须拒绝，不能靠换大小写绕过。
func TestOpenRejectsDuplicateRequestsFieldCaseVariants(t *testing.T) {
	for _, variant := range []string{`"Requests"`, `"REQUESTS"`, `"ReQuEsTs"`} {
		t.Run(variant, func(t *testing.T) {
			dir := t.TempDir()
			good := setupFeedingAliasLedger(t, dir)
			var raw stateTopLevel
			if err := json.Unmarshal(good, &raw); err != nil {
				t.Fatal(err)
			}
			bad := buildTopLevelWithDuplicateRequests(t, good, `"requests"`, raw.Requests, variant, raw.Requests)
			writeLedger(t, dir, bad)
			checkDuplicateRequestsFieldOpen(t, dir, bad)
		})
	}
}

// 字段名里的字符经 Unicode 简单大小写折叠后等价（长 s U+017F 折叠成 s）
// 时，标准库同样会把该键写入 requests 字段；文件里以码位转义书写该字符，
// 第二次出现也必须按重复拒绝。用 rune 构造键，避免在源码里直接写易混淆
// 字符；转义拆成两段书写，使单段不构成完整转义序列。
func TestOpenRejectsDuplicateRequestsFieldUnicodeFold(t *testing.T) {
	folded := `"reque\u01` + `7fts"` // JSON 解码为 reque + 长s + ts
	if !json.Valid([]byte(`{` + folded + `:{}}`)) {
		t.Fatalf("折叠写法的键应构成合法 JSON")
	}
	for name, keys := range map[string][2]string{
		"折叠写法在后": {`"requests"`, folded},
		"折叠写法在前": {folded, `"requests"`},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupFeedingAliasLedger(t, dir)
			var raw stateTopLevel
			if err := json.Unmarshal(good, &raw); err != nil {
				t.Fatal(err)
			}
			bad := buildTopLevelWithDuplicateRequests(t, good, keys[0], raw.Requests, keys[1], raw.Requests)
			writeLedger(t, dir, bad)
			checkDuplicateRequestsFieldOpen(t, dir, bad)
		})
	}
}

// 只是字形相似、但不与 requests 做 Unicode 大小写折叠的键（把首字符换成
// 西里尔字母 р U+0440）不属于请求记录字段：标准库会把它当作未知字段忽略，
// 重复检查也不能把它误判成 requests 而拒绝一份本来能读取的台账。
func TestHomoglyphNonFoldingKeyIsNotRequestsField(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	var raw stateTopLevel
	if err := json.Unmarshal(good, &raw); err != nil {
		t.Fatal(err)
	}
	// 唯一的请求记录字段用西里尔字母 р 起头：与 requests 不折叠，标准库
	// 忽略该未知字段，台账等同于没有请求记录（配方与批次仍完整）。
	homoglyph := `"` + string(rune(0x0440)) + `equests"`
	var buf bytes.Buffer
	buf.WriteString(`{"version":`)
	buf.Write(raw.Version)
	buf.WriteString(`,"recipes":`)
	buf.Write(raw.Recipes)
	buf.WriteString(`,"batches":`)
	buf.Write(raw.Batches)
	buf.WriteByte(',')
	buf.WriteString(homoglyph)
	buf.WriteByte(':')
	buf.Write(raw.Requests)
	buf.WriteByte('}')
	writeLedger(t, dir, buf.Bytes())

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("非折叠的同形键是未知字段，不应判为 requests 重复: %v", err)
	}
	defer s.Close()
	if _, err := s.GetRecipe("R1", "v1"); err != nil {
		t.Fatalf("配方与批次应仍可正常查询: %v", err)
	}
	if _, err := s.GetBatch("B1"); err != nil {
		t.Fatalf("配方与批次应仍可正常查询: %v", err)
	}
}

// 台账正常打开后文件才被改坏（最外层出现第二段 requests）：下一次查询或
// 写入重新读取时必须返回 ErrCorruptData——即使只访问另一个正常批次 B2、
// 或重放原本成功的请求，都不能沿用打开时读到的旧结果；被拒绝的读取不
// 返回局部视图，被拒绝的写入不增加投料、不改变批次状态、不占用请求编号。
// 恢复原文件后，原请求重放仍返回第一次成功的结果，新写入按现状继续编号。
func TestDuplicateRequestsFieldDetectedOnEveryReload(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	tm := feedingAliasBaseTime()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var raw stateTopLevel
	if err := json.Unmarshal(good, &raw); err != nil {
		t.Fatal(err)
	}
	bad := buildTopLevelWithDuplicateRequests(t, good, `"requests"`, raw.Requests, `"requests"`, raw.Requests)
	writeLedger(t, dir, bad)

	// 查询损坏相关批次与另一个正常批次都必须失败，不能返回旧快照。
	for _, batchNo := range []string{"B1", "B2"} {
		v, err := s.GetBatch(batchNo)
		if !errors.Is(err, ErrCorruptData) {
			t.Fatalf("损坏未修复时查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		}
		if v != nil {
			t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
		}
	}
	// 重放原本成功的投料请求：重新读取先于幂等重放，不能取回旧结果后
	// 静默放行（否则调用方会把这次返回误当成第一次成功登记的那条投料）。
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", tm, "张三"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上重放原请求应返回 ErrCorruptData，得到 %v", err)
	}
	// 对正常批次 B2 的幂等重放与全新写入同样不能绕过。
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

	// 恢复原文件后：原请求各自取回第一次成功的结果（B1 的序号 1、2），
	// 不新增投料；被拒绝过的新编号可以正常使用，按现状继续编号。
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
	r3, err := s.AddFeeding("feed-x", "B1", "M1", "1.000", tm, "张三")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号恢复后应可正常使用: %v", err)
	}
	checkFeedingView(t, r3, 3, "M1", "1", tm, "张三")
}

// 嵌套记录内部出现同名的 "requests" 字段不算最外层重复：在配方记录里
// 多带一个 requests 字段（标准解析会忽略未知字段），台账仍应正常打开，
// 不能因为扫描到这个嵌套键就误判为损坏。
func TestNestedRequestsFieldIsNotTopLevelDuplicate(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	var raw stateTopLevel
	if err := json.Unmarshal(good, &raw); err != nil {
		t.Fatal(err)
	}
	var recipes []map[string]json.RawMessage
	if err := json.Unmarshal(raw.Recipes, &recipes); err != nil {
		t.Fatal(err)
	}
	if len(recipes) == 0 {
		t.Fatalf("正常台账应至少有一条配方记录")
	}
	// 在第一条配方记录内部加入同名字段；它处于 recipes 数组的嵌套对象里，
	// 不是台账最外层键。
	recipes[0]["requests"] = json.RawMessage(`{"feed-9":null}`)
	recipesRaw, err := json.Marshal(recipes)
	if err != nil {
		t.Fatal(err)
	}
	top := map[string]json.RawMessage{
		"version":  raw.Version,
		"recipes":  recipesRaw,
		"batches":  raw.Batches,
		"requests": raw.Requests,
	}
	data, err := json.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"requests"`)) {
		t.Fatalf("测试数据中应包含嵌套的 requests 字段")
	}
	writeLedger(t, dir, data)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("嵌套记录内部的同名字段不应导致台账损坏: %v", err)
	}
	defer s.Close()
	if _, err := s.GetRecipe("R1", "v1"); err != nil {
		t.Fatalf("嵌套同名字段不应影响正常查询: %v", err)
	}
	// requests 对象内部的多条不同编号成功请求仍可逐条读取。
	tm := feedingAliasBaseTime()
	r1, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", tm, "张三")
	if err != nil {
		t.Fatalf("正常台账中的成功请求应仍可重放: %v", err)
	}
	checkFeedingView(t, r1, 1, "M1", "1", tm, "张三")
}

// 正常台账兼容性：只有一段 requests 时，多条不同编号的成功请求都可读取，
// 同一编号与相同内容重复提交仍返回首次成功结果、不新增投料。
func TestSingleRequestsSegmentRemainsCompatible(t *testing.T) {
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
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	tm := feedingAliasBaseTime()
	first, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", tm, "张三")
	if err != nil {
		t.Fatal(err)
	}
	// 相同编号 + 相同内容重复提交：返回首次结果（序号仍是 1），不新增投料。
	replay, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", tm, "张三")
	if err != nil {
		t.Fatalf("同编号同内容重复提交应返回首次成功结果: %v", err)
	}
	checkFeedingView(t, replay, first.Seq, "M1", "1", tm, "张三")
	b, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Feedings) != 1 {
		t.Fatalf("重复提交不应新增投料，得到 %d 条", len(b.Feedings))
	}
}

// requests 字段只出现一次时，缺失、空对象或 null 沿用原有行为：台账可以
// 正常打开、查询与写入。
func TestSingleOrMissingRequestsSegmentStillUsable(t *testing.T) {
	for name, data := range map[string]string{
		"字段缺失":     `{"version":1,"recipes":[],"batches":[]}`,
		"空对象":      `{"version":1,"recipes":[],"batches":[],"requests":{}}`,
		"null":     `{"version":1,"recipes":[],"batches":[],"requests":null}`,
		"空对象加未知字段": `{"version":1,"recipes":[],"batches":[],"other":1,"requests":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeLedger(t, dir, []byte(data))
			s, err := Open(dir)
			if err != nil {
				t.Fatalf("只出现一段 requests（含缺失、空对象、null）时应能打开: %v", err)
			}
			defer s.Close()
			if _, err := s.GetBatch("B1"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("空台账查询批次应返回 ErrNotFound，得到 %v", err)
			}
			registerStandardRecipe(t, s, "recipe-1")
			if _, err := s.GetRecipe("R1", "v1"); err != nil {
				t.Fatalf("台账应可正常写入并查询: %v", err)
			}
		})
	}
}
