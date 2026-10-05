package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件是“已保存的成功投料请求的返回结果必须与实际投料一致”的回归保障：
// 执行中的批次用请求编号登记投料后，按原编号、原内容再次提交，只能取回
// 第一次成功登记的那条投料。台账里保存的请求结果即使变成 null、空对象，
// 或被替换成另一条投料，也不能被当作成功结果返回——打开台账及之后的每次
// 查询/写入重载，都必须核对保存结果与原请求批次中同序号的实际投料一致，
// 并符合原提交内容；不一致按 ErrCorruptData 拒绝整份台账，不删除请求、
// 不补造投料，也不重新执行原登记。

// feedingRequestBaseTime 是投料请求结果回归测试使用的固定时间基。
func feedingRequestBaseTime() time.Time {
	return time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
}

// setupFeedingRequestLedger 在 dir 建立一份正常台账并关闭：
//   - B1（执行中）：feed-1 登记 M1 1.000 克（序号 1，结果显示为 1），
//     feed-2 登记 M1 3 克（序号 2，另一登记人）；
//   - B2（执行中）：feed-b2 登记 M1 5 克（序号 1，与 B1 各自编号）。
//
// 返回落盘后的台账文件内容，供各用例改坏后重写。
func setupFeedingRequestLedger(t *testing.T, dir string) []byte {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerStandardRecipe(t, s, "recipe-1") // M1=100、M2=0.5、M3=0.010
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatal(err)
	}
	t1 := feedingRequestBaseTime()
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("feed-2", "B1", "M1", "3", t1.Add(2*time.Hour), "王五"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("feed-b2", "B2", "M1", "5", t1.Add(time.Hour), "李四"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	return good
}

// corruptFeedingRequestResult 把正常台账内容中 feed-1 请求的保存结果替换为
// mutate 给出的值（mutate 返回 nil 表示删除该字段），返回改写后的文件内容。
func corruptFeedingRequestResult(t *testing.T, good []byte, mutate func() json.RawMessage) []byte {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	req := st.Requests["feed-1"]
	if req == nil {
		t.Fatalf("正常台账中应存在 feed-1 请求记录")
	}
	req.Result = mutate()
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// marshalFeedingView 把一条投料视图序列化为保存结果使用的 JSON。
func marshalFeedingView(t *testing.T, v FeedingView) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// 已保存的成功投料请求的结果缺失、为 null、为空对象，或被替换成另一条投料、
// 指向批次中不存在的序号、内容（物料、克数、时间、登记人）与实际投料或原
// 提交内容不符时，Open 必须返回 ErrCorruptData：错误信息指出问题请求编号
// 及关联批次，不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsCorruptFeedingRequestResult(t *testing.T) {
	t1 := feedingRequestBaseTime()
	cases := []struct {
		name   string
		mutate func() json.RawMessage
	}{
		{"结果缺失", func() json.RawMessage { return nil }},
		{"结果为 null", func() json.RawMessage { return json.RawMessage("null") }},
		{"结果为空对象", func() json.RawMessage { return json.RawMessage(`{}`) }},
		{"结果不是投料记录对象", func() json.RawMessage { return json.RawMessage(`"not-an-object"`) }},
		{"结果被替换成同批次另一条投料", func() json.RawMessage {
			// B1 序号 2 的记录真实存在，但它属于 feed-2，不是 feed-1 的登记。
			return marshalFeedingView(t, FeedingView{Seq: 2, MaterialNo: "M1", Grams: "3", Time: t1.Add(2 * time.Hour), Registrar: "王五"})
		}},
		{"结果指向批次中不存在的序号", func() json.RawMessage {
			return marshalFeedingView(t, FeedingView{Seq: 9, MaterialNo: "M1", Grams: "1", Time: t1, Registrar: "张三"})
		}},
		{"结果物料与实际投料不符", func() json.RawMessage {
			return marshalFeedingView(t, FeedingView{Seq: 1, MaterialNo: "M2", Grams: "1", Time: t1, Registrar: "张三"})
		}},
		{"结果克数与实际投料不符", func() json.RawMessage {
			return marshalFeedingView(t, FeedingView{Seq: 1, MaterialNo: "M1", Grams: "2", Time: t1, Registrar: "张三"})
		}},
		{"结果时间与实际投料不符", func() json.RawMessage {
			return marshalFeedingView(t, FeedingView{Seq: 1, MaterialNo: "M1", Grams: "1", Time: t1.Add(time.Minute), Registrar: "张三"})
		}},
		{"结果登记人与实际投料不符", func() json.RawMessage {
			return marshalFeedingView(t, FeedingView{Seq: 1, MaterialNo: "M1", Grams: "1", Time: t1, Registrar: "李四"})
		}},
		{"结果被替换成其他批次的同序号投料", func() json.RawMessage {
			// B2 的序号 1 与 B1 各自编号；B2 的记录不能作为 feed-1 的对应投料。
			return marshalFeedingView(t, FeedingView{Seq: 1, MaterialNo: "M1", Grams: "5", Time: t1.Add(time.Hour), Registrar: "李四"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupFeedingRequestLedger(t, dir)
			bad := corruptFeedingRequestResult(t, good, tc.mutate)
			if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("保存结果损坏应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"feed-1", "B1"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
				}
			}
			got, err := os.ReadFile(filepath.Join(dir, stateFileName))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, bad) {
				t.Fatalf("拒绝打开不应改动台账文件")
			}
		})
	}
}

// 原请求对应的批次不存在时，保存的结果不能单独作为登记成功的依据：
// 即使结果本身内容完整、与其他批次的某条投料完全吻合，也按损坏处理。
func TestOpenRejectsFeedingRequestResultWhenBatchMissing(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	// 删除 B1（feed-1、feed-2 对应的批次），保留完整的 B2 及其请求。
	kept := st.Batches[:0]
	for _, b := range st.Batches {
		if b.BatchNo != "B1" {
			kept = append(kept, b)
		}
	}
	st.Batches = kept
	bad, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("原批次不存在但保留请求结果应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	if !strings.Contains(err.Error(), "B1") {
		t.Fatalf("错误信息应指出关联批次 B1，得到 %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bad) {
		t.Fatalf("拒绝打开不应改动台账文件")
	}
}

// 正常写法差异不能误判为损坏：原请求填写 1.000、保存结果显示 1 是同一次
// 登记；保存结果的投料时间与实际记录表示同一时刻、仅时区写法不同也合法。
// 台账应正常打开，重放仍返回第一次成功登记的结果，批次查询显示实际投料。
func TestOpenAcceptsFeedingRequestResultFormatVariants(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingRequestLedger(t, dir)
	t1 := feedingRequestBaseTime()

	// 把 feed-1 保存结果的时间改成同一时刻的另一种时区写法（克数保持
	// 系统写入的 “1”，与原提交的 “1.000” 本就写法不同）。
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	var result FeedingView
	if err := json.Unmarshal(st.Requests["feed-1"].Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.Grams != "1" {
		t.Fatalf("系统保存的结果克数应为 1（原提交 1.000），得到 %q", result.Grams)
	}
	result.Time = t1.In(time.FixedZone("UTC+8", 8*3600))
	st.Requests["feed-1"].Result = marshalFeedingView(t, result)
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("写法差异不应判为损坏: %v", err)
	}
	defer s.Close()

	// 重放取回第一次成功登记的那条投料：序号 1、M1、显示 1 克、
	// 时间与原投料为同一时刻、登记人张三。
	replay, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("重放应成功: %v", err)
	}
	checkFeedingView(t, replay, 1, "M1", "1", t1, "张三")

	// 批次查询仍显示实际投料，重放没有追加记录。
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Feedings) != 2 {
		t.Fatalf("B1 应仍为 2 条投料，得到 %d 条", len(view.Feedings))
	}
	checkFeedingView(t, &view.Feedings[0], 1, "M1", "1", t1, "张三")
	checkFeedingView(t, &view.Feedings[1], 2, "M1", "3", t1.Add(2*time.Hour), "王五")
}

// 台账正常打开后，保存内容中的投料请求结果被改坏：下一次查询或写入必须
// 返回 ErrCorruptData——即使本次访问的是另一条正常记录或另一个正常批次，
// 也不能忽略损坏的投料请求；被拒绝的重放不得返回损坏的保存结果，被拒绝
// 的写入不留业务变化、不占用请求编号，原台账内容不变。恢复后原成功请求
// 仍可幂等重放，被拒绝过的请求编号可以正常使用。
func TestFeedingRequestResultCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingRequestLedger(t, dir)
	t1 := feedingRequestBaseTime()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 打开后把 feed-1 的保存结果改坏为空对象（其余记录保持完整）。
	bad := corruptFeedingRequestResult(t, good, func() json.RawMessage {
		return json.RawMessage(`{}`)
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// 查询损坏批次与正常批次都必须失败，不能沿用此前读到的内容。
	for _, batchNo := range []string{"B1", "B2"} {
		if v, err := s.GetBatch(batchNo); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		} else if v != nil {
			t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
		}
	}
	// 用原编号、原内容重放损坏的请求：不得把损坏的保存结果当成成功结果
	// 返回，必须报损坏。
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("重放损坏的投料请求应返回 ErrCorruptData，得到 %v", err)
	}
	// 对正常批次的新写入同样不能绕过。
	if _, err := s.AddFeeding("feed-new", "B2", "M1", "1", t1, "赵六"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上写入正常批次应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的访问不改动文件、不占用请求编号。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("被拒绝的访问不应改动台账文件")
	}
	if bytes.Contains(after, []byte("feed-new")) {
		t.Fatalf("被拒绝的写入不应留下请求记录")
	}

	// 恢复后：原成功请求仍取回第一次登记的结果，批次投料保持原样，
	// 被拒绝过的请求编号可以正常使用并取得连续序号。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	replay, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	checkFeedingView(t, replay, 1, "M1", "1", t1, "张三")
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Feedings) != 2 {
		t.Fatalf("B1 应仍为 2 条投料，得到 %d 条", len(view.Feedings))
	}
	f, err := s.AddFeeding("feed-new", "B2", "M1", "1", t1, "赵六")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法提交: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}

// 读取核对规则不改变请求内容匹配：正常台账中，用同一编号把原来的 1.000
// 改成数值相等的 1 再提交，仍返回 ErrRequestConflict；原内容重放仍取回
// 第一次的结果，批次中始终只有真正登记的投料。
func TestFeedingRequestResultCheckKeepsPayloadMatching(t *testing.T) {
	dir := t.TempDir()
	setupFeedingRequestLedger(t, dir)
	t1 := feedingRequestBaseTime()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.AddFeeding("feed-1", "B1", "M1", "1", t1, "张三"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("1.000 改成数值相等的 1 应返回 ErrRequestConflict，得到 %v", err)
	}
	replay, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("原内容重放应成功: %v", err)
	}
	checkFeedingView(t, replay, 1, "M1", "1", t1, "张三")
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Feedings) != 2 {
		t.Fatalf("冲突不应追加投料，B1 应仍为 2 条，得到 %d 条", len(view.Feedings))
	}
}
