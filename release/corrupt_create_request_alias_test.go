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

// 本文件是“同一批次不能有多个成功创建请求”的回归保障：同一批次的首次创建
// 请求若被保存到两个不同的请求编号下，两份内容与返回结果各自单独核对都能
// 通过（提交完全相同、保存结果都是合法的草稿结果、配方份数与数量核对全都
// 一致；或两个提交采用不同配方版本/份数、各自保存结果仍符合对应提交），
// 随后两个编号都会被当成各自创建成功——但一个批次只可能经历一次首次创建。
// 打开台账及之后每次查询、写入的重新读取都必须以 ErrCorruptData 拒绝整份
// 台账，错误信息说明同一批次有多个成功创建请求，并包含批次编号与发生冲突
// 的两个请求编号；不挑一个请求保留后继续，不删除请求、不合并、不改号、不
// 重新创建批次，被拒绝的写入不留业务记录、不占用请求编号，原文件保持不变。

// addCreateRequestRecord 在正常台账内容中追加一条创建批次请求记录（操作
// 类别 createBatch，提交内容 payload 原文、保存结果 result），返回改写后
// 的文件内容。本文件用它把同一批次的首次创建请求保存到第二个请求编号下。
func addCreateRequestRecord(t *testing.T, good []byte, reqNo, payload string, result json.RawMessage) []byte {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	if _, exists := st.Requests[reqNo]; exists {
		t.Fatalf("请求编号 %q 已存在，无法用于构造重复创建", reqNo)
	}
	st.Requests[reqNo] = &requestRecord{Op: opCreateBatch, Payload: payload, Result: result}
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// duplicateCreateRequestIdentical 把 create-b1 的请求记录原样复制到
// create-b1-dup 下：提交内容与保存结果完全相同。两份记录各自单独核对都
// 通过，唯有“同一批次有多个成功创建请求”这一跨请求关系不成立。
func duplicateCreateRequestIdentical(t *testing.T, good []byte) []byte {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	req := st.Requests["create-b1"]
	if req == nil {
		t.Fatalf("正常台账中应存在 create-b1 请求记录")
	}
	return addCreateRequestRecord(t, good, "create-b1-dup", req.Payload, req.Result)
}

// duplicateCreateRequestDifferentPlan 为批次 B1 构造第二条成功创建请求
// create-b1-alt：原提交改用 R1/v2、5 份（与 create-b1 的 R1/v1、8 份不同，
// 也与 B1 当前计划的 3 份不同），保存结果是对应这份提交的合法草稿结果
// （R1/v2、5 份、草稿、空投料、数量核对按 v2 每份克数 × 5 计算）。逐条核对
// 时该请求自身完全成立，不能被解释成两次合法创建只能靠跨请求核对发现。
func duplicateCreateRequestDifferentPlan(t *testing.T, good []byte) []byte {
	t.Helper()
	result := BatchView{
		BatchNo:         "B1",
		RecipeNo:        "R1",
		RecipeVersion:   "v2",
		RecipeName:      "配方改版",
		PlannedPortions: 5,
		Status:          StatusDraft,
		Feedings:        []FeedingView{},
		Materials: []MaterialRequirement{
			{MaterialNo: "M1", RequiredGrams: "1250", ActualGrams: "0", DifferenceGrams: "-1250"},
			{MaterialNo: "M4", RequiredGrams: "10", ActualGrams: "0", DifferenceGrams: "-10"},
		},
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return addCreateRequestRecord(t, good, "create-b1-alt",
		`{"BatchNo":"B1","RecipeNo":"R1","Version":"v2","Portions":5}`, raw)
}

// checkOpenRejectsDuplicateCreate 打开改坏后的台账，核对：返回 ErrCorruptData、
// 不返回可用对象、错误信息包含两个请求编号与批次编号并说明同一批次有多个
// 成功创建请求、原文件保持不变。
func checkOpenRejectsDuplicateCreate(t *testing.T, dir string, bad []byte, reqNo1, reqNo2 string) {
	t.Helper()
	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("同一批次有多个成功创建请求应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{reqNo1, reqNo2, "B1", "多个成功创建请求"} {
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

// 同一批次的首次创建请求被保存到两个不同的请求编号下（提交与结果完全相同）：
// 两份记录各自核对都通过，Open 仍必须按数据损坏拒绝整份台账。
func TestOpenRejectsDuplicateCreateRequestsIdentical(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateRequestLedger(t, dir)
	bad := duplicateCreateRequestIdentical(t, good)
	if bytes.Equal(good, bad) {
		t.Fatal("改坏辅助函数未改动台账内容")
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	checkOpenRejectsDuplicateCreate(t, dir, bad, "create-b1", "create-b1-dup")
}

// 两个创建提交采用不同配方版本与份数、各自保存结果仍符合对应提交时，同样
// 不能解释成两次合法创建：Open 必须按数据损坏拒绝整份台账。
func TestOpenRejectsDuplicateCreateRequestsDifferentPlan(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateRequestLedger(t, dir)
	bad := duplicateCreateRequestDifferentPlan(t, good)
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	checkOpenRejectsDuplicateCreate(t, dir, bad, "create-b1", "create-b1-alt")
}

// 台账正常打开后文件才被改坏（同一批次多出第二个成功创建请求）：下一次
// 查询或写入重新读取时必须返回 ErrCorruptData——查询其他正常批次或配方也
// 不能绕过；被拒绝的查询不返回局部业务结果，被拒绝的写入不留业务记录、
// 不占用请求编号，原文件保持不变。恢复后原创建请求仍取回首次结果，被
// 拒绝过的请求编号仍可正常使用。
func TestDuplicateCreateRequestDetectedOnEveryReload(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateRequestLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bad := duplicateCreateRequestIdentical(t, good)
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// 查询损坏批次与正常批次都必须失败，不能沿用此前读到的内容。
	for _, batchNo := range []string{"B1", "B2", "B3"} {
		v, err := s.GetBatch(batchNo)
		if !errors.Is(err, ErrCorruptData) {
			t.Fatalf("损坏未修复时查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		}
		if v != nil {
			t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
		}
	}
	// 查询正常配方同样不能绕过。
	if r, err := s.GetRecipe("R1", "v2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏未修复时查询配方应返回 ErrCorruptData，得到 %v", err)
	} else if r != nil {
		t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", r)
	}
	// 用原编号、原内容重放两个创建请求：load 先于幂等重放执行，都不能把
	// 保存结果当成自己的首次创建返回。
	for _, reqNo := range []string{"create-b1", "create-b1-dup"} {
		if _, err := s.CreateBatch(reqNo, "B1", "R1", "v1", 8); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("重放请求 %q 应先在重新读取时返回 ErrCorruptData，得到 %v", reqNo, err)
		}
	}
	// 对正常批次的新写入同样不能绕过。
	if _, err := s.StartBatch("start-b3", "B3"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上开始正常批次应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.CreateBatch("create-b4", "B4", "R1", "v2", 2); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上创建新批次应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的访问不改动文件，新写入不留下请求记录、不占用请求编号。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("被拒绝的访问不应改动台账文件")
	}
	for _, leaked := range []string{"start-b3", "create-b4", "B4"} {
		if bytes.Contains(after, []byte(leaked)) {
			t.Fatalf("被拒绝的写入不应留下业务记录或占用请求编号（发现了 %q）", leaked)
		}
	}

	// 恢复原文件后：原创建请求仍取回首次创建结果，被拒绝过的请求编号
	// 可以正常使用。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	replay, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("恢复后重放原创建请求失败: %v", err)
	}
	checkFirstCreateView(t, replay)
	created, err := s.CreateBatch("create-b4", "B4", "R1", "v2", 2)
	if err != nil {
		t.Fatalf("被拒绝过的请求编号恢复后应可正常使用: %v", err)
	}
	if created.BatchNo != "B4" || created.Status != StatusDraft {
		t.Fatalf("B4 应被正常创建为草稿，得到 %+v", created)
	}
}

// 不同批次各有自己的成功创建请求，即使采用同一配方版本与份数也不属于
// 重复创建；草稿调整、开始执行与关闭请求引用同一批次属于正常后续操作，
// 不纳入重复创建判断。正常台账必须可以打开、重放与查询。
func TestCreateRequestsForDifferentBatchesAreNotDuplication(t *testing.T) {
	dir := t.TempDir()
	setupCreateRequestLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("不同批次各有创建请求的正常台账应能打开: %v", err)
	}
	defer s.Close()

	// B1 的创建请求重放仍返回首次创建结果（R1/v1、8 份、草稿），批次后来
	// 调整、开始执行、投料并关闭都不影响这份创建记录的合法性。
	replay, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("重放 create-b1 失败: %v", err)
	}
	checkFirstCreateView(t, replay)
	// B2、B3 与 B1 的当前计划采用同一配方版本，各自的创建请求互不影响。
	for _, reqNo := range []string{"create-b2", "create-b3"} {
		if _, err := s.GetBatch(strings.ToUpper(strings.TrimPrefix(reqNo, "create-"))); err != nil {
			t.Fatalf("查询 %s 对应批次失败: %v", reqNo, err)
		}
	}
	// 依附于 B1 的调整、开始、关闭请求与创建请求引用同一批次，属于正常
	// 操作：重放仍返回各自首次成功的结果。
	if _, err := s.UpdateDraftBatch("u-final", "B1", "R1", "v2", 3); err != nil {
		t.Fatalf("重放草稿调整请求失败: %v", err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); err != nil {
		t.Fatalf("重放开始执行请求失败: %v", err)
	}
	if _, err := s.CloseBatch("close-b1", "B1"); err != nil {
		t.Fatalf("重放关闭请求失败: %v", err)
	}
	checkClosedCurrentAfterSetup(t, mustGetBatch(t, s, "B1"))
}

// 已有批次没有保存创建请求时继续沿用原来的读取要求，不要求补造请求：
// 删除 create-b1 的请求记录后，批次 B1 及其余请求记录仍在，打开、查询
// 与其他请求的重放都正常。
func TestBatchWithoutCreateRequestStillReadable(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateRequestLedger(t, dir)

	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	delete(st.Requests, "create-b1")
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("批次缺少创建请求记录不应判为损坏: %v", err)
	}
	defer s.Close()

	// 批次现状与其他请求的重放都不受影响。
	checkClosedCurrentAfterSetup(t, mustGetBatch(t, s, "B1"))
	if _, err := s.CloseBatch("close-b1", "B1"); err != nil {
		t.Fatalf("重放关闭请求失败: %v", err)
	}
	// create-b1 编号已被删除、不再占用：用同一编号提交相同内容会被当成
	// 新请求，而批次 B1 已存在，按既有规则返回 ErrDuplicateBatch。
	if _, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8); !errors.Is(err, ErrDuplicateBatch) {
		t.Fatalf("删除创建请求记录后同编号创建应返回 ErrDuplicateBatch，得到 %v", err)
	}
}
