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

// 本文件锁定“草稿批次不能带有投料记录”的读取侧规则。
//
// 写入侧只允许执行中的批次登记投料（AddFeeding 对草稿返回 ErrInvalidState），
// 但台账文件可能来自旧版本、外部工具或被直接改写：其中的草稿批次可能已经
// 带着投料。读取已有台账时，只要任一草稿批次包含至少一条投料记录，整份
// 台账就必须判为损坏——即使这些投料的物料都属于绑定版本、克数均为合法
// 正数，或累计实投恰好等于应投量也不能接受；不能丢弃投料后当作干净草稿，
// 不能把批次自动改为执行中或已关闭，也不能替调用方改选配方版本后继续。

// 构造一个处于草稿状态、但带有投料记录的批次台账。
// 投料的物料必然属于绑定版本 R1/v1，克数为合法正数——用来排除
// “物料不属于配方”与“数量非法”这两条既有的拒绝理由。
func writeDraftFeedingState(t *testing.T, dir string, batches []*batchRecord) []byte {
	t.Helper()
	return writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{
			{RecipeNo: "R1", Version: "v1", Name: "配方一", Materials: []materialRecord{
				{MaterialNo: "M1", GramsMilli: 100000},
				{MaterialNo: "M2", GramsMilli: 50000},
			}},
		},
		Batches: batches,
	})
}

// Open 读取已有台账时，草稿批次只要带有投料记录就必须返回 ErrCorruptData。
// 错误信息要指出批次编号并说明“草稿状态下存在投料记录”，让调用方能把它
// 与配方版本缺失、投料物料不属于绑定版本或投料数量非法区分开；
// 不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsDraftBatchWithFeedings(t *testing.T) {
	cases := []struct {
		name    string
		batches []*batchRecord
	}{
		{
			name: "单条合法投料且累计恰好等于应投",
			// 2 份 × M1 每份 100 克 = 应投 200 克，实投恰好 200 克。
			batches: []*batchRecord{{
				BatchNo: "B-bad", RecipeNo: "R1", RecipeVersion: "v1",
				PlannedPortions: 2, Status: StatusDraft,
				Feedings: []feedingRecord{
					{Seq: 1, MaterialNo: "M1", GramsMilli: 200000, Time: time.Now(), Registrar: "张三"},
				},
			}},
		},
		{
			name: "多种物料各投一条且各自恰好等于应投",
			// 1 份：M1 应投 100、M2 应投 50，两条实投都恰好吻合。
			batches: []*batchRecord{{
				BatchNo: "B-bad", RecipeNo: "R1", RecipeVersion: "v1",
				PlannedPortions: 1, Status: StatusDraft,
				Feedings: []feedingRecord{
					{Seq: 1, MaterialNo: "M1", GramsMilli: 100000, Time: time.Now(), Registrar: "张三"},
					{Seq: 2, MaterialNo: "M2", GramsMilli: 50000, Time: time.Now(), Registrar: "李四"},
				},
			}},
		},
		{
			name: "投料数量远低于应投，仍不能接受",
			batches: []*batchRecord{{
				BatchNo: "B-bad", RecipeNo: "R1", RecipeVersion: "v1",
				PlannedPortions: 10, Status: StatusDraft,
				Feedings: []feedingRecord{
					{Seq: 1, MaterialNo: "M1", GramsMilli: 1000, Time: time.Now(), Registrar: "张三"},
				},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			original := writeDraftFeedingState(t, dir, tc.batches)

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("草稿带投料应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可继续使用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"B-bad", "草稿", "投料"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含 %q 以说明草稿状态下存在投料，得到 %v", want, err)
				}
			}
			// 原因必须能与配方缺失、物料归属、数量非法这几类损坏区分开。
			for _, notWant := range []string{"未登记", "不属于", "不是正数", "超出上限"} {
				if strings.Contains(msg, notWant) {
					t.Fatalf("草稿带投料应以独立原因报错，错误信息不应混入 %q，得到 %v", notWant, err)
				}
			}
			// 拒绝打开时不得丢弃投料、自动改状态或补写台账。
			got, readErr := os.ReadFile(filepath.Join(dir, stateFileName))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(got, original) {
				t.Fatalf("拒绝打开不应改动台账文件")
			}
		})
	}
}

// 台账里另有正常批次（无投料草稿、执行中带合法投料、已关闭欠投/超投），
// 也不能绕过：只要一个草稿批次带投料，Open 整体失败。
func TestOpenDraftFeedingNotBypassedByCompleteRecords(t *testing.T) {
	dir := t.TempDir()
	writeDraftFeedingState(t, dir, []*batchRecord{
		// 正常：无投料的草稿。
		{BatchNo: "B-draft", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusDraft},
		// 正常：执行中带合法投料。
		{BatchNo: "B-exec", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 2, Status: StatusExecuting,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: 300000, Time: time.Now(), Registrar: "张三"}, // 超投
			}},
		// 正常：已关闭且零投料（关闭不保证数量吻合，也不要求必须有投料）。
		{BatchNo: "B-closed0", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 2, Status: StatusClosed},
		// 正常：已关闭但欠投。
		{BatchNo: "B-closed-under", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 2, Status: StatusClosed,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M2", GramsMilli: 10000, Time: time.Now(), Registrar: "李四"}, // 应投 100，实投 10
			}},
		// 损坏：草稿却带投料（物料属于绑定版本、数量合法、恰好等于应投）。
		{BatchNo: "B-bad", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusDraft,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: 100000, Time: time.Now(), Registrar: "王五"},
			}},
	})

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("即使其他批次全部正常，草稿带投料也应整体拒绝，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可继续使用的 Store 对象")
	}
	if !strings.Contains(err.Error(), "B-bad") {
		t.Fatalf("错误信息应指出有问题的批次编号，得到 %v", err)
	}
}

// 本次约束只针对草稿已有投料：无投料草稿正常查询与调整，执行中尚未投料
// 正常，已关闭批次可以没有投料，也可以欠投、超投；合法草稿的数量核对仍
// 列出全部配方物料，累计实投为零，差额为应投量的负值。重新打开（触发完整
// 读取校验）后这些数据仍完整可用。
func TestDraftWithoutFeedingsAndClosedVariantsStillValid(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}); err != nil {
		t.Fatal(err)
	}
	// 无投料草稿。
	if _, err := s1.CreateBatch("b-draft", "B-draft", "R1", "v1", 2); err != nil {
		t.Fatal(err)
	}
	// 执行中尚未投料。
	if _, err := s1.CreateBatch("b-exec", "B-exec", "R1", "v1", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.StartBatch("s-exec", "B-exec"); err != nil {
		t.Fatal(err)
	}
	// 已关闭但零投料。
	if _, err := s1.CreateBatch("b-closed0", "B-closed0", "R1", "v1", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.StartBatch("s-closed0", "B-closed0"); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.CloseBatch("c-closed0", "B-closed0"); err != nil {
		t.Fatal(err)
	}
	// 已关闭且超投/欠投并存（M1 应投 200 实投 300；M2 应投 1 实投 0）。
	if _, err := s1.CreateBatch("b-mix", "B-mix", "R1", "v1", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.StartBatch("s-mix", "B-mix"); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.AddFeeding("f-mix", "B-mix", "M1", "300", time.Now(), "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.CloseBatch("c-mix", "B-mix"); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("全部为合法组合的台账重新打开应成功: %v", err)
	}
	defer s2.Close()

	// 无投料草稿：列出全部配方物料，实投为零，差额为应投的负值，且仍可调整。
	draft, err := s2.GetBatch("B-draft")
	if err != nil {
		t.Fatalf("无投料草稿应可查询: %v", err)
	}
	if draft.Status != StatusDraft || len(draft.Feedings) != 0 {
		t.Fatalf("草稿状态与空投料列表应保留: %+v", draft)
	}
	dm := materialsMap(draft)
	if len(dm) != 2 {
		t.Fatalf("草稿数量核对应列出全部配方物料，得到 %d 项", len(dm))
	}
	checkRequirement(t, dm, "M1", "200", "0", "-200")
	checkRequirement(t, dm, "M2", "1", "0", "-1")
	updated, err := s2.UpdateDraftBatch("u-draft", "B-draft", "", "", 3)
	if err != nil {
		t.Fatalf("无投料草稿应仍可调整计划份数: %v", err)
	}
	if updated.PlannedPortions != 3 || updated.Status != StatusDraft {
		t.Fatalf("草稿调整结果不正确: %+v", updated)
	}

	// 执行中尚未投料。
	exec, err := s2.GetBatch("B-exec")
	if err != nil {
		t.Fatalf("执行中批次应可查询: %v", err)
	}
	if exec.Status != StatusExecuting || len(exec.Feedings) != 0 {
		t.Fatalf("执行中且无投料的批次应原样保留: %+v", exec)
	}

	// 已关闭且零投料。
	closed0, err := s2.GetBatch("B-closed0")
	if err != nil {
		t.Fatalf("零投料的已关闭批次应可查询: %v", err)
	}
	if closed0.Status != StatusClosed || len(closed0.Feedings) != 0 {
		t.Fatalf("关闭不要求必须有投料: %+v", closed0)
	}

	// 已关闭且数量不吻合：超投、欠投都只是核对结果，不构成损坏。
	mix, err := s2.GetBatch("B-mix")
	if err != nil {
		t.Fatalf("欠投/超投的已关闭批次应可查询: %v", err)
	}
	if mix.Status != StatusClosed {
		t.Fatalf("已关闭状态应保留: %+v", mix)
	}
	mm := materialsMap(mix)
	checkRequirement(t, mm, "M1", "200", "300", "100") // 超投
	checkRequirement(t, mm, "M2", "1", "0", "-1")      // 欠投
}

// 台账正常打开后，文件内容被改成“草稿批次带投料”：下一次查询或带有效
// 请求编号的写入都必须在重新读取时返回 ErrCorruptData，不能沿用此前读到
// 的内容；查询其他正常批次/配方不能绕过；即使是损坏前已成功请求的幂等
// 重放也必须拒绝。被拒绝的操作不保存任何调整结果、不占用请求编号，也不
// 覆盖原文件；恢复文件后原有记录与请求行为完整可用。
func TestDraftFeedingCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}); err != nil {
		t.Fatal(err)
	}
	// B1：始终是草稿（随后被改坏）；B2：执行中且有一条合法投料；B3：已关闭零投料。
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	if _, err := s.AddFeeding("f-b2", "B2", "M1", "5", fixedTime, "李四"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b3", "B3", "R1", "v1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s3", "B3"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseBatch("c3", "B3"); err != nil {
		t.Fatal(err)
	}

	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	// 直接改写文件，给草稿 B1 加入一条投料：M1 1000 克，恰好等于
	// 100 克 × 10 份的应投量，物料属于绑定版本——唯一问题是草稿不能有投料。
	var broken persistedState
	if err := json.Unmarshal(good, &broken); err != nil {
		t.Fatal(err)
	}
	for _, b := range broken.Batches {
		if b.BatchNo == "B1" {
			b.Feedings = append(b.Feedings, feedingRecord{
				Seq: 1, MaterialNo: "M1", GramsMilli: 1000000, Time: fixedTime, Registrar: "张三",
			})
		}
	}
	badBytes := writeStateFile(t, dir, &broken)

	// 查询损坏批次：失败且不返回部分视图。
	if v, err := s.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("改坏后查询 B1 应返回 ErrCorruptData，得到 %v", err)
	} else if v != nil {
		t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
	}
	// 查询其他正常批次、配方同样不能绕过。
	if v, err := s.GetBatch("B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("即使查询另一个正常批次 B2 也必须拒绝，得到 %v", err)
	} else if v != nil {
		t.Fatalf("整份台账损坏时不应返回 B2 的部分视图: %+v", v)
	}
	if _, err := s.GetBatch("B3"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("查询已关闭批次 B3 也必须拒绝，得到 %v", err)
	}
	if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("改坏后查询配方也应返回 ErrCorruptData，得到 %v", err)
	}

	// 带有效请求编号的写入：调整草稿、开始执行、对正常批次投料、关闭、
	// 登记配方，全部在重新读取时被拒绝。
	if _, err := s.UpdateDraftBatch("upd-b1", "B1", "", "", 5); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上调整草稿应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上开始草稿应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.AddFeeding("feed-b2-new", "B2", "M1", "1", time.Now(), "王五"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上对正常批次投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.CloseBatch("close-b2", "B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上关闭正常批次应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.RegisterRecipe("r2", "R2", "v1", "新配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上登记配方也应失败，得到 %v", err)
	}
	// 即使是损坏前已成功的相同请求，幂等重放也发生在重新读取之后，
	// 不能用保存的请求结果绕过这次损坏。
	if _, err := s.AddFeeding("f-b2", "B2", "M1", "5", fixedTime, "李四"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上重放原成功请求也应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的操作不落盘：文件内容保持改坏后的原样，请求编号未被占用。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, badBytes) {
		t.Fatalf("被拒绝的操作不应改动台账文件")
	}
	for _, marker := range []string{"upd-b1", "start-b1", "feed-b2-new", "close-b2", "R2"} {
		if bytes.Contains(after, []byte(marker)) {
			t.Fatalf("被拒绝的操作不应留下请求记录或业务数据 %q", marker)
		}
	}

	// 恢复原内容后：B1 仍是无投料草稿，B2/B3 状态与投料完整，
	// 被拒绝过的请求编号可以正常使用，原成功请求仍可幂等重放。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("恢复后查询 B1 应成功: %v", err)
	}
	if b1.Status != StatusDraft || b1.PlannedPortions != 10 || len(b1.Feedings) != 0 {
		t.Fatalf("B1 应恢复为无投料草稿: %+v", b1)
	}
	b1m := materialsMap(b1)
	checkRequirement(t, b1m, "M1", "1000", "0", "-1000")
	b2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatalf("恢复后查询 B2 应成功: %v", err)
	}
	if b2.Status != StatusExecuting || len(b2.Feedings) != 1 || b2.Feedings[0].Grams != "5" {
		t.Fatalf("B2 原有投料与状态应完整保留: %+v", b2)
	}
	b3, err := s.GetBatch("B3")
	if err != nil {
		t.Fatalf("恢复后查询 B3 应成功: %v", err)
	}
	if b3.Status != StatusClosed || len(b3.Feedings) != 0 {
		t.Fatalf("B3 应为零投料的已关闭批次: %+v", b3)
	}
	replay, err := s.AddFeeding("f-b2", "B2", "M1", "5", fixedTime, "李四")
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	if replay.Seq != 1 {
		t.Fatalf("重放应返回第一次成功的结果，得到 seq=%d", replay.Seq)
	}
	updated, err := s.UpdateDraftBatch("upd-b1", "B1", "", "", 5)
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法使用: %v", err)
	}
	if updated.PlannedPortions != 5 || updated.Status != StatusDraft {
		t.Fatalf("恢复后草稿调整结果不正确: %+v", updated)
	}
	f, err := s.AddFeeding("feed-b2-new", "B2", "M1", "1", time.Now(), "王五")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法投料: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}
