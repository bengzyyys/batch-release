package release

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 用手工 JSON 构造一份台账：配方固定为 R1/v1（M1=100 克/份），批次记录
// 由调用方以原始 JSON 片段给出，以便覆盖状态字段缺失、为 null 等无法
// 通过 batchRecord 结构体序列化出的形态。
func writeRawBatchState(t *testing.T, dir string, batchJSON ...string) []byte {
	t.Helper()
	data := []byte(fmt.Sprintf(`{
  "version": 1,
  "recipes": [{"recipeNo":"R1","version":"v1","name":"配方一","materials":[{"materialNo":"M1","gramsMilli":100000}]}],
  "batches": [%s],
  "requests": {}
}`, strings.Join(batchJSON, ",")))
	if err := os.WriteFile(filepath.Join(dir, stateFileName), data, 0o644); err != nil {
		t.Fatalf("写入台账文件失败: %v", err)
	}
	return data
}

// 批次状态必须精确为 draft、executing、closed：状态写成其他字符串、
// 大小写不同、前后多了空格、为空、缺失或为 null，Open 都必须返回
// ErrCorruptData。错误信息指出批次编号与实际读到的状态；没有状态内容时
// 明确说明状态为空或缺失。不返回可用的台账对象，不改写原文件；台账里
// 另有完全正常的批次也不能放行，不能跳过问题批次继续返回其他批次。
func TestOpenRejectsUnrecognizedBatchStatus(t *testing.T) {
	cases := []struct {
		name       string
		statusJSON string // 批次记录中 status 字段的原始 JSON（含字段名）；空串表示整个字段缺失
		wantStatus string // 错误信息中应出现的实际状态；空串表示应说明状态为空或缺失
	}{
		{"未定义的状态值", `"status": "ready"`, "ready"},
		{"大小写不同", `"status": "Draft"`, "Draft"},
		{"全大写", `"status": "EXECUTING"`, "EXECUTING"},
		{"前后带空格", `"status": " draft "`, " draft "},
		{"空字符串", `"status": ""`, ""},
		{"字段缺失", ``, ""},
		{"null 值", `"status": null`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			badBatch := `{"batchNo":"B1","recipeNo":"R1","recipeVersion":"v1","plannedPortions":2,` +
				tc.statusJSON + `,"feedings": []}`
			if tc.statusJSON == "" {
				badBatch = `{"batchNo":"B1","recipeNo":"R1","recipeVersion":"v1","plannedPortions":2,"feedings": []}`
			}
			// 另有一个完全正常的已关闭批次，不能因此放行。
			goodBatch := `{"batchNo":"B2","recipeNo":"R1","recipeVersion":"v1","plannedPortions":1,"status":"closed","feedings": []}`
			original := writeRawBatchState(t, dir, badBatch, goodBatch)

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("状态非法应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			if !strings.Contains(msg, "B1") {
				t.Fatalf("错误信息应指出批次编号 B1，得到 %v", err)
			}
			if tc.wantStatus != "" {
				if !strings.Contains(msg, tc.wantStatus) {
					t.Fatalf("错误信息应指出实际读到的状态 %q，得到 %v", tc.wantStatus, err)
				}
			} else {
				if !strings.Contains(msg, "空") && !strings.Contains(msg, "缺失") {
					t.Fatalf("没有状态内容时应明确说明状态为空或缺失，得到 %v", err)
				}
				if strings.Contains(msg, "不允许") {
					t.Fatalf("不应只报笼统的操作不允许，得到 %v", err)
				}
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

// 配方版本与计划份数都正常的批次，状态被写成 ready：即使尚未投料或投料
// 数量已经吻合，也不能根据投料情况替它猜出状态而放行。
func TestOpenRejectsReadyStatusRegardlessOfFeedings(t *testing.T) {
	cases := []struct {
		name     string
		feedings string
	}{
		{"尚未投料", `[]`},
		{"投料数量恰好吻合", `[{"seq":1,"materialNo":"M1","gramsMilli":200000,"time":"2026-08-01T09:00:00Z","registrar":"张三"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeRawBatchState(t, dir,
				`{"batchNo":"B1","recipeNo":"R1","recipeVersion":"v1","plannedPortions":2,"status":"ready","feedings": `+tc.feedings+`}`)

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("状态 ready 应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			if !strings.Contains(err.Error(), "ready") {
				t.Fatalf("错误信息应指出实际状态 ready，得到 %v", err)
			}
		})
	}
}

// 台账打开后保存内容中的批次状态变成非法值：下一次查询或带有效请求编号
// 的写入同样返回 ErrCorruptData，不能继续使用此前读到的正常数据；被拒绝
// 的写入不保存业务变更、不占用请求编号，原台账内容不变。恢复合法状态后
// 原记录完整可用，被拒绝过的请求编号仍可合法提交。
func TestStatusCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
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

	// 把 B1 的保存状态改成非法值（B2 仍完整）。
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	badBytes := bytes.Replace(good, []byte(`"status": "draft"`), []byte(`"status": "ready"`), 1)
	if bytes.Equal(badBytes, good) {
		t.Fatalf("未能替换状态字段")
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), badBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// 查询损坏批次、正常批次与配方都必须失败。
	for _, batchNo := range []string{"B1", "B2"} {
		if _, err := s.GetBatch(batchNo); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		}
	}
	if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("查询配方应返回 ErrCorruptData，得到 %v", err)
	}
	// 带有效请求编号的写入同样不能绕过。
	if _, err := s.UpdateDraftBatch("u-rejected", "B1", "", "", 5); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上调整草稿应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.StartBatch("s-rejected", "B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上开始执行应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的写入不改动文件、不占用请求编号。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, badBytes) {
		t.Fatalf("被拒绝的写入不应改动台账文件")
	}
	for _, reqNo := range []string{"u-rejected", "s-rejected", "feed-rejected"} {
		if bytes.Contains(after, []byte(reqNo)) {
			t.Fatalf("被拒绝的写入不应留下请求记录 %q", reqNo)
		}
	}

	// 恢复后原记录完整可用，被拒绝过的请求编号仍可合法提交。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("恢复后查询应成功: %v", err)
	}
	if b1.Status != StatusDraft {
		t.Fatalf("原有草稿状态应恢复为 draft: %+v", b1)
	}
	if _, err := s.StartBatch("s-rejected", "B2"); err != nil {
		t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
	}
}

// 合法状态不受本次检查影响：无投料的草稿、尚未投料的执行中、没有投料或
// 存在欠投、超投的已关闭批次都能正常打开与查询，不能仅因数量不吻合被拒。
func TestLegitStatusesStillAccepted(t *testing.T) {
	dir := t.TempDir()
	fixedTime := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	writeBatchState(t, dir,
		&batchRecord{
			BatchNo: "B-draft", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 2, Status: StatusDraft,
		},
		&batchRecord{
			BatchNo: "B-exec", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 1, Status: StatusExecuting,
		},
		&batchRecord{
			BatchNo: "B-closed-empty", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 1, Status: StatusClosed,
		},
		&batchRecord{
			BatchNo: "B-closed-under", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 1, Status: StatusClosed,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: 40000, Time: fixedTime, Registrar: "张三"},
			},
		},
		&batchRecord{
			BatchNo: "B-closed-over", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 1, Status: StatusClosed,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: 150000, Time: fixedTime, Registrar: "张三"},
			},
		},
	)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("合法状态的台账应正常打开: %v", err)
	}
	defer s.Close()

	wantStatus := map[string]BatchStatus{
		"B-draft":        StatusDraft,
		"B-exec":         StatusExecuting,
		"B-closed-empty": StatusClosed,
		"B-closed-under": StatusClosed,
		"B-closed-over":  StatusClosed,
	}
	for batchNo, status := range wantStatus {
		view, err := s.GetBatch(batchNo)
		if err != nil {
			t.Fatalf("查询 %q 失败: %v", batchNo, err)
		}
		if view.Status != status {
			t.Fatalf("%q 状态应为 %s，得到 %s", batchNo, status, view.Status)
		}
	}
	// 欠投、超投原样保留，不能仅因数量不吻合被状态检查拒绝。
	under, err := s.GetBatch("B-closed-under")
	if err != nil {
		t.Fatal(err)
	}
	if under.Materials[0].DifferenceGrams != "-60" {
		t.Fatalf("欠投应原样保留: %+v", under.Materials[0])
	}
	over, err := s.GetBatch("B-closed-over")
	if err != nil {
		t.Fatal(err)
	}
	if over.Materials[0].DifferenceGrams != "50" {
		t.Fatalf("超投应原样保留: %+v", over.Materials[0])
	}
}
