package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件是“已保存的成功创建批次请求的返回结果必须对应原提交内容第一次创建
// 成功时的内容”的回归保障：批次用请求编号创建后，按原编号、原内容再次提交
// CreateBatch，只能取回第一次创建时的结果（原选配方版本与份数、草稿状态、
// 空投料、实投为零、差额为应投量的负值）。台账里保存的创建结果即使变成
// null、空对象，或被改成另一批次的结果、被改掉配方绑定/份数/状态，或混入
// 了批次后来的调整、投料与关闭现状，也不能被当作成功结果返回——打开台账及
// 之后的每次查询/写入重载，都必须核对保存结果与原提交内容（原请求对应的
// 批次与原选配方版本必须仍然存在）一致；不一致按 ErrCorruptData 拒绝整份
// 台账，不删除请求、不补造批次，也不用当前查询结果覆盖损坏结果。

// setupCreateRequestLedger 在 dir 建立一份正常台账：
//   - B1（已关闭）：由 create-b1 用 R1/v1、8 份创建，随后草稿调整为 R1/v2、
//     3 份，开始执行、登记两条投料（f1、f2）并关闭——其保存的创建结果必须
//     仍是首次创建时的内容（R1/v1、8 份、草稿、空投料），不能混入后来的
//     调整、投料与关闭现状；
//   - B2（草稿）：用 R1/v2、3 份创建——其创建结果与 B1 的当前计划内容相同，
//     但那是另一批次的创建结果，用于验证不能顶替；
//   - B3（草稿）：R1/v2、2 份的正常批次，用于验证损坏的创建请求不能被
//     “本次访问的是另一个正常批次”绕过。
//
// 返回落盘后的台账文件内容，供各用例改坏后重写。
func setupCreateRequestLedger(t *testing.T, dir string) []byte {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerCreateReplayRecipes(t, s) // R1/v1：M1=0.125、M2=0.001；R1/v2：M1=250、M4=2
	if _, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraftBatch("u-final", "B1", "R1", "v2", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); err != nil {
		t.Fatal(err)
	}
	base := replayFeedTime()
	if _, err := s.AddFeeding("f1", "B1", "M1", "100.5", base, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("f2", "B1", "M4", "2", base, "李四"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseBatch("close-b1", "B1"); err != nil {
		t.Fatal(err)
	}
	// B2：配方与份数和 B1 的当前计划相同，仅批次编号与请求编号不同。
	if _, err := s.CreateBatch("create-b2", "B2", "R1", "v2", 3); err != nil {
		t.Fatal(err)
	}
	// B3：正常批次，不参与任何改坏。
	if _, err := s.CreateBatch("create-b3", "B3", "R1", "v2", 2); err != nil {
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

// createResultRaw 取出正常台账中 reqNo 请求的保存结果原文。
func createResultRaw(t *testing.T, good []byte, reqNo string) json.RawMessage {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	req := st.Requests[reqNo]
	if req == nil {
		t.Fatalf("正常台账中应存在 %s 请求记录", reqNo)
	}
	return req.Result
}

// mutatedCreateView 读出 create-b1 的保存结果，按 edit 修改后重新序列化。
func mutatedCreateView(t *testing.T, good []byte, edit func(*BatchView)) json.RawMessage {
	t.Helper()
	var v BatchView
	if err := json.Unmarshal(createResultRaw(t, good, "create-b1"), &v); err != nil {
		t.Fatal(err)
	}
	edit(&v)
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// corruptCreateRequestResult 把正常台账内容中 create-b1 请求的保存结果替换为
// mutate 给出的值（mutate 返回 nil 表示删除该字段），返回改写后的文件内容。
func corruptCreateRequestResult(t *testing.T, good []byte, mutate func() json.RawMessage) []byte {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	req := st.Requests["create-b1"]
	if req == nil {
		t.Fatalf("正常台账中应存在 create-b1 请求记录")
	}
	req.Result = mutate()
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// 已保存的成功创建批次请求的结果缺失、为 null、为空对象、无法读成批次结果，
// 或与原提交内容第一次创建成功时的内容不一致（批次编号、配方绑定、计划份数、
// 状态、投料列表、逐物料应投量/实投量/差额有任何差异，含被另一批次的结果
// 顶替、混入后来的调整、投料或关闭现状）时，Open 必须返回 ErrCorruptData：
// 错误信息指出问题请求编号及关联批次，不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsCorruptCreateRequestResult(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(good []byte) json.RawMessage
	}{
		{"结果缺失", func(good []byte) json.RawMessage { return nil }},
		{"结果为 null", func(good []byte) json.RawMessage { return json.RawMessage("null") }},
		{"结果为空对象", func(good []byte) json.RawMessage { return json.RawMessage(`{}`) }},
		{"结果不是批次结果对象", func(good []byte) json.RawMessage { return json.RawMessage(`"not-an-object"`) }},
		{"结果被替换成另一批次的创建结果", func(good []byte) json.RawMessage {
			// B2 的配方、份数与 B1 的当前计划相同，但那是另一批次的创建结果。
			return createResultRaw(t, good, "create-b2")
		}},
		{"结果批次编号被改成另一批次", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.BatchNo = "B2" })
		}},
		{"结果配方编号被改", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.RecipeNo = "RX" })
		}},
		{"结果沿用后来调整的版本", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.RecipeVersion = "v2" })
		}},
		{"结果配方名称被改", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.RecipeName = "被改的名称" })
		}},
		{"结果沿用后来调整的份数", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.PlannedPortions = 3 })
		}},
		{"结果状态被改成关闭后的现状", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.Status = StatusClosed })
		}},
		{"结果混入了后来追加的投料", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) {
				base := replayFeedTime()
				v.Feedings = []FeedingView{
					{Seq: 1, MaterialNo: "M1", Grams: "100.5", Time: base, Registrar: "张三"},
					{Seq: 2, MaterialNo: "M4", Grams: "2", Time: base, Registrar: "李四"},
				}
			})
		}},
		{"物料核对项少一项", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) {
				v.Materials = v.Materials[:1]
			})
		}},
		{"物料核对项混入后来版本的物料", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) {
				v.Materials = append(v.Materials, MaterialRequirement{
					MaterialNo: "M4", RequiredGrams: "6", ActualGrams: "0", DifferenceGrams: "-6",
				})
			})
		}},
		{"物料核对顺序被调换", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) {
				v.Materials[0], v.Materials[1] = v.Materials[1], v.Materials[0]
			})
		}},
		{"应投量被改成当前计划的值", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.Materials[0].RequiredGrams = "750" })
		}},
		{"实投量被改成非零", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.Materials[0].ActualGrams = "100.5" })
		}},
		{"差额被改坏", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.Materials[0].DifferenceGrams = "0" })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupCreateRequestLedger(t, dir)
			bad := corruptCreateRequestResult(t, good, func() json.RawMessage { return tc.mutate(good) })
			if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("保存的创建结果损坏应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"create-b1", "B1"} {
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

// 原提交内容本身被改坏（无法解析，或批次编号为空、份数不是正整数等不符合
// 创建要求的内容）时，不可能对应一次成功的创建：Open 必须返回
// ErrCorruptData，错误信息指出问题请求编号。
func TestOpenRejectsCorruptCreateRequestPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"提交内容无法解析", `not-json`},
		{"提交内容批次编号为空", `{"BatchNo":"","RecipeNo":"R1","Version":"v1","Portions":8}`},
		{"提交内容份数为零", `{"BatchNo":"B1","RecipeNo":"R1","Version":"v1","Portions":0}`},
		{"提交内容份数为负", `{"BatchNo":"B1","RecipeNo":"R1","Version":"v1","Portions":-2}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupCreateRequestLedger(t, dir)
			var st persistedState
			if err := json.Unmarshal(good, &st); err != nil {
				t.Fatal(err)
			}
			st.Requests["create-b1"].Payload = tc.payload
			bad, err := json.MarshalIndent(&st, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("提交内容损坏应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			if !strings.Contains(err.Error(), "create-b1") {
				t.Fatalf("错误信息应指出问题请求编号 create-b1，得到 %v", err)
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

// 原请求对应的批次不存在时，保存的创建结果不能单独作为创建成功的依据：
// 即使结果本身内容完整，也按损坏处理。
func TestOpenRejectsCreateRequestResultWhenBatchMissing(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	// 删除 B1，并移除依附于它的后续请求记录（调整、开始、投料、关闭），
	// 让 create-b1 的批次缺失成为唯一的损坏点。
	kept := st.Batches[:0]
	for _, b := range st.Batches {
		if b.BatchNo != "B1" {
			kept = append(kept, b)
		}
	}
	st.Batches = kept
	for _, reqNo := range []string{"u-final", "start-b1", "f1", "f2", "close-b1"} {
		delete(st.Requests, reqNo)
	}
	bad, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("原批次不存在但保留创建结果应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"create-b1", "B1"} {
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
}

// 原提交所选的配方版本不存在时，保存的创建结果不能单独作为创建成功的依据：
// 同编号配方的其他版本（R1/v2）即使仍在使用，也不能顶替原选的 R1/v1。
func TestOpenRejectsCreateRequestResultWhenRecipeVersionMissing(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	// 删除 R1/v1：B1 后来已改用 R1/v2，批次绑定仍然完整。依附 R1/v1 的
	// 配方登记请求记录一并移除，让 create-b1 的原选版本缺失成为唯一的
	// 损坏点（B3 用 v2，不受影响）。
	kept := st.Recipes[:0]
	for _, r := range st.Recipes {
		if !(r.RecipeNo == "R1" && r.Version == "v1") {
			kept = append(kept, r)
		}
	}
	st.Recipes = kept
	delete(st.Requests, "c-recipe-v1")
	bad, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("原选配方版本不存在但保留创建结果应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"create-b1", "B1"} {
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
}

// 正常写法差异不能误判为损坏：保存结果的应投量写成 1.000、实投量写成
// 0.000、差额写成 -1.000，与首次创建时的数量是同一数值；合法的零实投与
// 负差额不是损坏。台账应正常打开，重放仍返回保存的写法。
func TestOpenAcceptsCreateRequestResultFormatVariants(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateRequestLedger(t, dir)

	bad := corruptCreateRequestResult(t, good, func() json.RawMessage {
		return mutatedCreateView(t, good, func(v *BatchView) {
			v.Materials[0].RequiredGrams = "1.000" // 与 1 是同一数量
			v.Materials[0].ActualGrams = "0.000"   // 与 0 是同一数量
			v.Materials[0].DifferenceGrams = "-1.000"
			v.Materials[1].RequiredGrams = "0.008" // 保持原值
		})
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("写法差异不应判为损坏: %v", err)
	}
	defer s.Close()

	// 重放取回保存的结果原文：核对项保持 1.000、0.000、-1.000 写法，
	// 但与首次创建时的数量是同一数值。
	replay, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("重放创建请求应成功: %v", err)
	}
	if replay.Materials[0].RequiredGrams != "1.000" ||
		replay.Materials[0].ActualGrams != "0.000" ||
		replay.Materials[0].DifferenceGrams != "-1.000" {
		t.Fatalf("重放应返回保存的核对项写法，得到 %+v", replay.Materials[0])
	}
}

// 批次后来的调整、开始、投料与关闭都属于批次现状，不能混入第一次创建的
// 结果，也不能成为拒绝合法旧结果的理由：台账中 B1 已改选配方、调整份数、
// 开始执行、追加投料并关闭，其保存的创建结果仍是首次创建时的内容
// （R1/v1、8 份、草稿、空投料、零实投），打开与重放都必须正常；重复提交
// 原创建请求继续返回首次结果，查询继续显示当前批次，两者互不覆盖。
func TestCreateRequestResultStaysFirstCreateAfterAdjustAndClose(t *testing.T) {
	dir := t.TempDir()
	setupCreateRequestLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("已调整并关闭的批次不应让其首次创建结果被判损坏: %v", err)
	}
	defer s.Close()

	replay, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("重放创建请求应成功: %v", err)
	}
	checkFirstCreateView(t, replay)
	// 查询继续显示当前批次（已关闭、R1/v2、3 份、保留两条投料），
	// 不被首次创建结果回退。
	checkClosedCurrentAfterSetup(t, mustGetBatch(t, s, "B1"))
}

// 台账正常打开后，保存内容中的创建请求结果被改坏：下一次查询或写入必须
// 返回 ErrCorruptData——即使本次访问的是另一个正常批次，也不能忽略损坏的
// 创建请求；被拒绝的重放不得返回损坏的保存结果，被拒绝的写入不留业务变化、
// 不占用请求编号，原台账内容不变。恢复后原成功请求仍可幂等重放，被拒绝过
// 的请求编号可以正常使用。
func TestCreateRequestResultCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateRequestLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 打开后把 create-b1 的保存结果改坏为空对象（其余记录保持完整）。
	bad := corruptCreateRequestResult(t, good, func() json.RawMessage {
		return json.RawMessage(`{}`)
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// 查询损坏批次与正常批次都必须失败，不能沿用此前读到的内容。
	for _, batchNo := range []string{"B1", "B3"} {
		if v, err := s.GetBatch(batchNo); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		} else if v != nil {
			t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
		}
	}
	// 用原编号、原内容重放损坏的创建请求：不得把损坏的保存结果当成成功
	// 结果返回，必须报损坏。
	if _, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("重放损坏的创建请求应返回 ErrCorruptData，得到 %v", err)
	}
	// 对正常批次的新写入同样不能绕过。
	if _, err := s.StartBatch("start-b3", "B3"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上开始正常批次应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的访问不改动文件、不占用请求编号。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("被拒绝的访问不应改动台账文件")
	}
	if bytes.Contains(after, []byte("start-b3")) {
		t.Fatalf("被拒绝的写入不应留下请求记录")
	}

	// 恢复后：原成功请求仍取回第一次创建时的结果，被拒绝过的请求编号
	// 可以正常使用。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	replay, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	checkFirstCreateView(t, replay)
	started, err := s.StartBatch("start-b3", "B3")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法提交: %v", err)
	}
	if started.Status != StatusExecuting || started.BatchNo != "B3" {
		t.Fatalf("B3 应被正常开始执行，得到 %+v", started)
	}
}

// 任何一条成功创建请求损坏都应拒绝整份台账：改坏 create-b2 的结果后，
// 即使调用方只查询正常的 B1、B3，Open 与后续访问也必须返回
// ErrCorruptData。
func TestAnyCorruptCreateRequestRejectsWholeLedger(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	st.Requests["create-b2"].Result = json.RawMessage(`{}`)
	bad, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("任一创建请求损坏都应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	if !strings.Contains(err.Error(), "create-b2") {
		t.Fatalf("错误信息应指出问题请求编号 create-b2，得到 %v", err)
	}
}

// 白盒核对：正常台账中 create-b1 的保存结果确实是首次创建时的内容，
// 且本文件的改坏辅助函数确实改动了文件内容（防止测试因辅助函数失效而
// 假性通过）。
func TestCreateRequestCorruptionHelpersWork(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateRequestLedger(t, dir)

	var saved BatchView
	if err := json.Unmarshal(createResultRaw(t, good, "create-b1"), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.BatchNo != "B1" || saved.RecipeNo != "R1" || saved.RecipeVersion != "v1" ||
		saved.RecipeName != "配方初版" || saved.PlannedPortions != 8 ||
		saved.Status != StatusDraft || len(saved.Feedings) != 0 || len(saved.Materials) != 2 {
		t.Fatalf("正常台账中保存的创建结果应为首次创建内容，得到 %+v", saved)
	}
	bad := corruptCreateRequestResult(t, good, func() json.RawMessage {
		return json.RawMessage(`{}`)
	})
	if bytes.Equal(good, bad) {
		t.Fatal("改坏辅助函数未改动台账内容")
	}
	// 正常台账本身必须能打开：所有“正常后续操作不误判”的前提。
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("正常台账应能打开: %v", err)
	}
	s.Close()
	// 每个改坏用例产生的文件都与正常台账不同。
	for i, mutate := range []func() json.RawMessage{
		func() json.RawMessage { return nil },
		func() json.RawMessage { return json.RawMessage("null") },
		func() json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.PlannedPortions = 3 })
		},
	} {
		if bytes.Equal(good, corruptCreateRequestResult(t, good, mutate)) {
			t.Fatalf("第 %d 个改坏用例未改动台账内容", i)
		}
	}
}

// 正常台账上的既有行为保持不变：请求冲突、批次编号重复与返回对象可独立
// 修改不受创建结果核对影响。
func TestCreateRequestValidationKeepsExistingBehavior(t *testing.T) {
	dir := t.TempDir()
	setupCreateRequestLedger(t, dir)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 沿用原请求编号、改变创建内容 → ErrRequestConflict。
	if _, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 3); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("改变创建内容应返回 ErrRequestConflict，得到 %v", err)
	}
	// 换未使用的请求编号创建同编号批次 → ErrDuplicateBatch。
	if _, err := s.CreateBatch("create-fresh", "B1", "R1", "v1", 8); !errors.Is(err, ErrDuplicateBatch) {
		t.Fatalf("同编号批次应返回 ErrDuplicateBatch，得到 %v", err)
	}
	// 重放返回的对象可独立修改，不影响台账与下一次重放。
	replay, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatal(err)
	}
	replay.PlannedPortions = 99
	replay.Materials[0].ActualGrams = "42"
	replay2, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatal(err)
	}
	checkFirstCreateView(t, replay2)
	if fmt.Sprint(replay2.Materials[0].ActualGrams) != "0" {
		t.Fatalf("重放结果不应受调用方改写影响: %+v", replay2.Materials[0])
	}
}
