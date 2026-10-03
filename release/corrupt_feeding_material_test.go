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

// 两版配方：R1/v1 只有 M1、M2；R1/v2 才含 M3。
func recipesR1TwoVersions() []*recipeRecord {
	return []*recipeRecord{
		{RecipeNo: "R1", Version: "v1", Name: "配方初版", Materials: []materialRecord{
			{MaterialNo: "M1", GramsMilli: 100000},
			{MaterialNo: "M2", GramsMilli: 500},
		}},
		{RecipeNo: "R1", Version: "v2", Name: "配方改版", Materials: []materialRecord{
			{MaterialNo: "M1", GramsMilli: 100000},
			{MaterialNo: "M3", GramsMilli: 2000},
		}},
	}
}

// Open 时已保存投料的物料不属于批次绑定版本：必须返回 ErrCorruptData。
// 即使同编号配方的 v2 已登记且包含该物料（M3），绑定 v1 的批次仍按 v1 判定。
// 错误信息需指出批次编号、问题物料编号及绑定的配方编号、版本号；
// 不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsFeedingMaterialNotInBoundVersion(t *testing.T) {
	dir := t.TempDir()
	original := writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: recipesR1TwoVersions(),
		Batches: []*batchRecord{{
			BatchNo:         "B1",
			RecipeNo:        "R1",
			RecipeVersion:   "v1",
			PlannedPortions: 2,
			Status:          StatusExecuting,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: 1000, Time: time.Now(), Registrar: "张三"},
				// M3 只在 R1/v2 中，v1 没有：数量为正、未超上限也不能接受。
				{Seq: 2, MaterialNo: "M3", GramsMilli: 2000, Time: time.Now(), Registrar: "李四"},
			},
		}},
	})

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("投料物料不属于绑定版本应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"B1", "M3", "R1", "v1"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
		}
	}
	if strings.Contains(msg, "v2") {
		t.Fatalf("归属应只按绑定版本 v1 判定，错误信息不应拿 v2 说事，得到 %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("拒绝打开不应改动台账文件")
	}
}

// 物料存在于另一配方中也不能作为接受依据：
// R2/v1 含 M3，但批次绑定的 R1/v1 不含，仍必须拒绝。
func TestOpenMaterialInOtherRecipeStillRejected(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{
			recipesR1TwoVersions()[0],
			{RecipeNo: "R2", Version: "v1", Name: "另一配方", Materials: []materialRecord{
				{MaterialNo: "M3", GramsMilli: 2000},
			}},
		},
		Batches: []*batchRecord{{
			BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 1, Status: StatusExecuting,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M3", GramsMilli: 1, Time: time.Now(), Registrar: "张三"},
			},
		}},
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("物料存在于另一配方不能接受，得到 %v", err)
	}
}

// 归属检查对草稿、执行中、已关闭三种状态的批次一视同仁。
func TestOpenRejectsForeignMaterialForAllStatuses(t *testing.T) {
	for _, status := range []BatchStatus{StatusDraft, StatusExecuting, StatusClosed} {
		t.Run(string(status), func(t *testing.T) {
			dir := t.TempDir()
			writeStateFile(t, dir, &persistedState{
				Version: stateVersion,
				Recipes: recipesR1TwoVersions(),
				Batches: []*batchRecord{{
					BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
					PlannedPortions: 1, Status: status,
					Feedings: []feedingRecord{
						{Seq: 1, MaterialNo: "M3", GramsMilli: 1, Time: time.Now(), Registrar: "张三"},
					},
				}},
			})
			if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("状态 %s 的批次含非配方物料也应拒绝，得到 %v", status, err)
			}
		})
	}
}

// 同一台账内其他批次完整也不能绕过：只要一个批次的投料不属于绑定版本，
// Open 整体失败。不能丢掉问题投料后只核对其余批次。
func TestOpenOneBatchWithForeignMaterialFailsWholeLedger(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: recipesR1TwoVersions(),
		Batches: []*batchRecord{
			{BatchNo: "B-ok-1", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusDraft},
			{BatchNo: "B-bad", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 2, Status: StatusExecuting,
				Feedings: []feedingRecord{
					{Seq: 1, MaterialNo: "M3", GramsMilli: 1, Time: time.Now(), Registrar: "张三"},
				}},
			{BatchNo: "B-ok-2", RecipeNo: "R1", RecipeVersion: "v2", PlannedPortions: 3, Status: StatusClosed,
				Feedings: []feedingRecord{
					// M3 在 v2 中合法：完整批次本身没问题，也救不了 B-bad。
					{Seq: 1, MaterialNo: "M3", GramsMilli: 2, Time: time.Now(), Registrar: "李四"},
				}},
		},
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("存在问题批次时整份台账都应拒绝，得到 %v", err)
	}
}

// 属于绑定版本的物料可以分多次投料，台账正常打开、按登记顺序返回，
// 数量核对继续按物料分别计算；没有投料的配方物料仍显示零实投。
func TestOpenAcceptsMultipleFeedingsOfBoundMaterials(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: recipesR1TwoVersions(),
		Batches: []*batchRecord{{
			BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 2, Status: StatusExecuting,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: 1000, Time: time.Now(), Registrar: "张三"},
				{Seq: 2, MaterialNo: "M2", GramsMilli: 500, Time: time.Now(), Registrar: "李四"},
				{Seq: 3, MaterialNo: "M1", GramsMilli: 250, Time: time.Now(), Registrar: "王五"},
			},
		}},
	})
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("全部属于绑定版本的投料应正常打开: %v", err)
	}
	defer s.Close()
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	got := materialsMap(view)
	if len(got) != 2 {
		t.Fatalf("v1 只有 M1、M2 两项核对，得到 %+v", got)
	}
	// M1：应投 100×2=200，实投 1+0.25=1.25；M2：应投 0.5×2=1，实投 0.5。
	checkRequirement(t, got, "M1", "200", "1.25", "-198.75")
	checkRequirement(t, got, "M2", "1", "0.5", "-0.5")
	if len(view.Feedings) != 3 ||
		view.Feedings[0].MaterialNo != "M1" || view.Feedings[1].MaterialNo != "M2" || view.Feedings[2].MaterialNo != "M1" {
		t.Fatalf("投料应按登记顺序原样返回: %+v", view.Feedings)
	}
}

// 台账打开后保存内容被改成含非配方物料的投料：下一次查询或写入同样失败，
// 查询损坏批次与其他完整批次都不能返回部分成功的视图，写入也不能绕过；
// 被拒绝的写入不保存业务变更、不占用请求编号，原台账内容不变；
// 恢复为完好内容后原记录继续可用，被拒绝过的编号仍可合法提交。
func TestForeignMaterialAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.RegisterRecipe("r-v1", "R1", "v1", "配方初版", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("r-v2", "R1", "v2", "配方改版", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M3", Grams: "2"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	// B2 绑定含 M3 的 v2，是完整批次，作为“不能借其他批次绕过”的对照。
	if _, err := s.CreateBatch("b2", "B2", "R1", "v2", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	if _, err := s.AddFeeding("feed-ok", "B2", "M3", "2", fixedTime, "张三"); err != nil {
		t.Fatal(err)
	}

	// 把 B1 的保存内容改坏：追加一条 M3 投料（B1 绑定的 v1 只有 M1、M2；B2 仍完整）。
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
			b.Feedings = append(b.Feedings, feedingRecord{
				Seq: 1, MaterialNo: "M3", GramsMilli: 1, Time: fixedTime, Registrar: "李四",
			})
		}
	}
	badBytes := writeStateFile(t, dir, &broken)

	for _, batchNo := range []string{"B1", "B2"} {
		_, err := s.GetBatch(batchNo)
		if !errors.Is(err, ErrCorruptData) {
			t.Fatalf("查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		}
		if !strings.Contains(err.Error(), "B1") || !strings.Contains(err.Error(), "M3") {
			t.Fatalf("错误应指出问题批次 B1 与物料 M3，得到 %v", err)
		}
	}
	// 对完整批次写入合法物料也必须因整份台账损坏被拒绝，不能借操作 B2 绕过。
	if _, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "王五"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
	}

	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, badBytes) {
		t.Fatalf("被拒绝的写入不应改动台账文件")
	}
	if bytes.Contains(after, []byte("feed-rejected")) {
		t.Fatalf("被拒绝的写入不应留下请求记录")
	}

	// 恢复后原记录完整可用，被拒绝过的请求编号仍可合法提交。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	b2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatalf("恢复后查询应成功: %v", err)
	}
	if len(b2.Feedings) != 1 || b2.Feedings[0].Grams != "2" {
		t.Fatalf("原有投料记录应保留: %+v", b2)
	}
	f, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "王五")
	if err != nil {
		t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}

// 正常台账上追加投料遇到非配方物料，仍返回 ErrMaterialNotInRecipe：
// 新提交的输入错误不能混同于已保存记录损坏，失败不占用编号、不留记录。
func TestNewFeedingForeignMaterialStillNotInRecipe(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.RegisterRecipe("r-v1", "R1", "v1", "配方初版", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("r-v2", "R1", "v2", "配方改版", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M3", Grams: "2"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	// M3 已在 v2 登记、数量为正，但 B1 绑定 v1：新提交仍按输入错误处理。
	if _, err := s.AddFeeding("f-bad", "B1", "M3", "2", time.Now(), "张三"); !errors.Is(err, ErrMaterialNotInRecipe) {
		t.Fatalf("正常台账上的非配方物料应返回 ErrMaterialNotInRecipe，得到 %v", err)
	}
	// 同一编号改投绑定版本内的物料应成功，说明失败没有占用编号。
	f, err := s.AddFeeding("f-bad", "B1", "M1", "10", time.Now(), "张三")
	if err != nil {
		t.Fatalf("失败不应占用请求编号，合法投料应成功: %v", err)
	}
	if f.Seq != 1 {
		t.Fatalf("被拒绝的投料不应占用序号，得到 seq=%d", f.Seq)
	}
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Feedings) != 1 || view.Feedings[0].MaterialNo != "M1" {
		t.Fatalf("被拒绝的非配方投料不应留下记录: %+v", view.Feedings)
	}
}
