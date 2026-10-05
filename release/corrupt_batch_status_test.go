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

// rawBatchRecord 用原始 JSON 控制 status 字段的写法：
// 传 nil 且 omitempty 时字段缺失，传 RawMessage("null") 写成 null，
// 从而能模拟“字段缺失/null/空字符串”等 Go 字符串类型无法区分的损坏形态。
type rawBatchRecord struct {
	BatchNo         string          `json:"batchNo"`
	RecipeNo        string          `json:"recipeNo"`
	RecipeVersion   string          `json:"recipeVersion"`
	PlannedPortions int             `json:"plannedPortions"`
	Status          json.RawMessage `json:"status,omitempty"`
	Feedings        []feedingRecord `json:"feedings,omitempty"`
}

type rawState struct {
	Version int              `json:"version"`
	Recipes []*recipeRecord  `json:"recipes"`
	Batches []rawBatchRecord `json:"batches"`
}

// writeRawStatusState 写入一份配方为 R1/v1、批次 B-bad（2 份）状态由
// statusJSON 原样给出（缺失字段传 nil）的台账，返回写入的字节。
func writeRawStatusState(t *testing.T, dir string, statusJSON json.RawMessage, feedings []feedingRecord) []byte {
	t.Helper()
	st := rawState{
		Version: stateVersion,
		Recipes: []*recipeRecord{recipeR1v1()},
		Batches: []rawBatchRecord{{
			BatchNo:         "B-bad",
			RecipeNo:        "R1",
			RecipeVersion:   "v1",
			PlannedPortions: 2, // 计划份数完全合法
			Status:          statusJSON,
			Feedings:        feedings,
		}},
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatalf("序列化台账失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), data, 0o644); err != nil {
		t.Fatalf("写入台账文件失败: %v", err)
	}
	return data
}

// Open 时已保存批次的状态不是精确的 draft/executing/closed 之一，
// 必须返回 ErrCorruptData：其他字符串、大小写不同、前后空格、空字符串、
// null、字段缺失都不合法；即使尚未投料或投料数量已经吻合也不能放行。
// 错误信息指出问题批次编号与实际读到的状态，状态为空或缺失时明确说明；
// 不能报成笼统的“当前状态不允许该操作”；不返回可用对象，不改写原文件。
func TestOpenRejectsUnrecognizedBatchStatus(t *testing.T) {
	// 投料数量与应投吻合（R1/v1 每份 100 克 × 2 份 = 200 克）也不能放行。
	matched := []feedingRecord{
		{Seq: 1, MaterialNo: "M1", GramsMilli: 200000, Time: time.Now(), Registrar: "张三"},
	}
	cases := []struct {
		name       string
		statusJSON json.RawMessage
		feedings   []feedingRecord
		empty      bool // 读不到任何状态内容
		wantShown  string
	}{
		{"其他字符串 ready 且未投料", json.RawMessage(`"ready"`), nil, false, "ready"},
		{"其他字符串 ready 且投料吻合", json.RawMessage(`"ready"`), matched, false, "ready"},
		{"其他字符串 pending", json.RawMessage(`"pending"`), nil, false, "pending"},
		{"首字母大写 Draft", json.RawMessage(`"Draft"`), nil, false, "Draft"},
		{"全大写 DRAFT", json.RawMessage(`"DRAFT"`), nil, false, "DRAFT"},
		{"前导空格", json.RawMessage(`" draft"`), nil, false, " draft"},
		{"尾随空格", json.RawMessage(`"closed "`), nil, false, "closed "},
		{"前后都有空格", json.RawMessage(`" executing "`), nil, false, " executing "},
		{"空字符串", json.RawMessage(`""`), nil, true, ""},
		{"null", json.RawMessage(`null`), nil, true, ""},
		{"字段缺失", nil, nil, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			original := writeRawStatusState(t, dir, tc.statusJSON, tc.feedings)

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("无法识别的状态应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			if !strings.Contains(msg, "B-bad") {
				t.Fatalf("错误信息应指出问题批次编号 B-bad，得到 %v", err)
			}
			if tc.empty {
				if !strings.Contains(msg, "为空或缺失") {
					t.Fatalf("状态无内容时错误信息应明确说明为空或缺失，得到 %v", err)
				}
			} else {
				if !strings.Contains(msg, tc.wantShown) {
					t.Fatalf("错误信息应包含实际读到的状态 %q，得到 %v", tc.wantShown, err)
				}
			}
			if strings.Contains(msg, "不允许该操作") {
				t.Fatalf("读取损坏数据不应报成笼统的操作不允许，得到 %v", err)
			}
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

// 一个批次状态非法时整份台账都不能读取：即使其他批次与配方完全正常，
// Open 也必须失败，不能跳过问题批次返回其余内容。
func TestOpenOneBatchWithBadStatusFailsWholeLedger(t *testing.T) {
	dir := t.TempDir()
	st := rawState{
		Version: stateVersion,
		Recipes: []*recipeRecord{recipeR1v1()},
		Batches: []rawBatchRecord{
			{BatchNo: "B-ok-1", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: json.RawMessage(`"draft"`)},
			{BatchNo: "B-bad", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 2, Status: json.RawMessage(`"ready"`)},
			{BatchNo: "B-ok-2", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 3, Status: json.RawMessage(`"closed"`)},
		},
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("存在状态非法的批次应整体拒绝，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	if !strings.Contains(err.Error(), "B-bad") {
		t.Fatalf("错误信息应指出问题批次编号 B-bad，得到 %v", err)
	}
}

// 台账打开后保存内容中的批次状态变成非法值：下一次查询或带有效请求编号的
// 写入都必须返回 ErrCorruptData，不能继续使用此前读到的正常数据；查询
// 正常批次或配方也不能绕过；被拒绝的操作不改文件、不占请求编号；恢复为
// 合法状态后原记录继续可用，被拒绝过的编号仍可合法提交。
func TestBatchStatusCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	cases := []struct {
		name      string
		toStatus  BatchStatus
		wantShown string
		empty     bool
	}{
		{"改成未知字符串 ready", BatchStatus("ready"), "ready", false},
		{"改成大小写不同", BatchStatus("Draft"), "Draft", false},
		{"改成带空格", BatchStatus(" closed "), " closed ", false},
		{"改成空字符串", BatchStatus(""), "为空或缺失", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
				{MaterialNo: "M1", Grams: "100"},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 2); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 1); err != nil {
				t.Fatal(err)
			}
			if _, err := s.StartBatch("s2", "B2"); err != nil {
				t.Fatal(err)
			}
			fixedTime := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
			if _, err := s.AddFeeding("feed-ok", "B2", "M1", "5", fixedTime, "张三"); err != nil {
				t.Fatal(err)
			}

			good, err := os.ReadFile(filepath.Join(dir, stateFileName))
			if err != nil {
				t.Fatal(err)
			}
			var broken persistedState
			if err := json.Unmarshal(good, &broken); err != nil {
				t.Fatal(err)
			}
			for _, b := range broken.Batches {
				if b.BatchNo == "B1" {
					b.Status = tc.toStatus
				}
			}
			badBytes := writeStateFile(t, dir, &broken)

			// 查询损坏批次：必须报 ErrCorruptData 并指出编号与实际状态。
			_, err = s.GetBatch("B1")
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("状态改坏后查询应返回 ErrCorruptData，得到 %v", err)
			}
			if !strings.Contains(err.Error(), "B1") || !strings.Contains(err.Error(), tc.wantShown) {
				t.Fatalf("错误信息应包含批次编号 B1 与实际状态 %q，得到 %v", tc.wantShown, err)
			}
			// 查询正常批次、配方同样失败。
			if _, err := s.GetBatch("B2"); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("整份台账损坏后查询正常批次也应失败，得到 %v", err)
			}
			if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("整份台账损坏后查询配方也应失败，得到 %v", err)
			}
			// 带有效（从未使用的）请求编号的写入不能绕过。
			if _, err := s.UpdateDraftBatch("u-rejected", "B1", "", "", 5); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("损坏台账上调整草稿应返回 ErrCorruptData，得到 %v", err)
			}
			if _, err := s.StartBatch("start-rejected", "B1"); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("损坏台账上开始执行应返回 ErrCorruptData，得到 %v", err)
			}
			if _, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
			}
			if _, err := s.RegisterRecipe("r-rejected", "R3", "v1", "新配方", []MaterialInput{
				{MaterialNo: "M1", Grams: "1"},
			}); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("损坏台账上登记配方应返回 ErrCorruptData，得到 %v", err)
			}

			// 被拒绝的操作不改文件、不留请求记录。
			after, err := os.ReadFile(filepath.Join(dir, stateFileName))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, badBytes) {
				t.Fatalf("被拒绝的写入不应改动台账文件")
			}
			for _, no := range []string{"u-rejected", "start-rejected", "feed-rejected", "r-rejected"} {
				if bytes.Contains(after, []byte(no)) {
					t.Fatalf("被拒绝的写入不应留下请求记录 %q", no)
				}
			}

			// 恢复合法状态后原记录完整可用；被拒绝过的编号仍可合法提交。
			if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
				t.Fatal(err)
			}
			b1, err := s.GetBatch("B1")
			if err != nil {
				t.Fatalf("恢复后查询 B1 应成功: %v", err)
			}
			if b1.Status != StatusDraft || b1.PlannedPortions != 2 || len(b1.Feedings) != 0 {
				t.Fatalf("B1 原有状态与计划应保留: %+v", b1)
			}
			adjusted, err := s.UpdateDraftBatch("u-rejected", "B1", "", "", 5)
			if err != nil {
				t.Fatalf("被拒绝过的调整编号应仍可合法使用: %v", err)
			}
			if adjusted.PlannedPortions != 5 {
				t.Fatalf("调整结果不正确: %+v", adjusted)
			}
			if _, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "李四"); err != nil {
				t.Fatalf("被拒绝过的投料编号应仍可合法使用: %v", err)
			}
			b2, err := s.GetBatch("B2")
			if err != nil {
				t.Fatal(err)
			}
			if len(b2.Feedings) != 2 {
				t.Fatalf("恢复后 B2 应保留原投料并能追加，得到 %d 条", len(b2.Feedings))
			}
		})
	}
}

// 三种合法状态仍按原规则读取：草稿只能在无投料时存在，执行中可以尚未
// 投料，已关闭可以没有投料或存在欠投、超投——状态检查不夹带数量吻合
// 要求，合法批次不应被这次修正拒绝。
func TestExactValidStatusesStillAccepted(t *testing.T) {
	dir := t.TempDir()
	fixedTime := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	writeBatchState(t, dir,
		&batchRecord{BatchNo: "B-draft", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 2, Status: StatusDraft},
		&batchRecord{BatchNo: "B-exec-empty", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusExecuting},
		&batchRecord{BatchNo: "B-closed-empty", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusClosed},
		&batchRecord{BatchNo: "B-closed-under", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusClosed,
			Feedings: []feedingRecord{{Seq: 1, MaterialNo: "M1", GramsMilli: 40000, Time: fixedTime, Registrar: "张三"}}},
		&batchRecord{BatchNo: "B-closed-over", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusClosed,
			Feedings: []feedingRecord{{Seq: 1, MaterialNo: "M1", GramsMilli: 150000, Time: fixedTime, Registrar: "张三"}}},
	)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("全部状态合法的台账应正常打开: %v", err)
	}
	defer s.Close()
	for _, no := range []string{"B-draft", "B-exec-empty", "B-closed-empty", "B-closed-under", "B-closed-over"} {
		if _, err := s.GetBatch(no); err != nil {
			t.Fatalf("合法批次 %q 应可查询: %v", no, err)
		}
	}
	// 关闭后的投料保护不变。
	if _, err := s.AddFeeding("f-closed", "B-closed-over", "M1", "1", time.Now(), "张三"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("关闭后投料仍应返回 ErrInvalidState，得到 %v", err)
	}
	// 无投料草稿仍可调整，执行中仍可投料。
	if _, err := s.UpdateDraftBatch("u1", "B-draft", "", "", 3); err != nil {
		t.Fatalf("合法草稿应仍可调整: %v", err)
	}
	if _, err := s.AddFeeding("f1", "B-exec-empty", "M1", "10", time.Now(), "张三"); err != nil {
		t.Fatalf("执行中批次应仍可投料: %v", err)
	}
}
