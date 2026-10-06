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

// 本文件回归保障开始执行结果与关闭结果共有的核对（已整理为
// validateBatchRequestAnchor 一份逻辑）：两类结果都必须对应“原请求指定的
// 那个批次、该批次开始执行时最终固定的配方编号/版本（名称取这个已登记版本）
// 与计划份数”，不能因为另一批次采用相同配方、份数与投料就接受另一批次的
// 结果；草稿创建时的旧计划不能被沿用。两类结果各自的含义（开始：执行中、
// 空投料、零实投；关闭：已关闭、保留全部投料与核对）仍分别核对。
//
// 场景：B1 创建时用 R1/v1、5 份，开始前改成 R1/v2、3 份后开始、投料并关闭。
// 因此开始结果与关闭结果都必须对应 R1/v2、3 份，而不是创建草稿时的
// R1/v1、5 份。B2 是与 B1 配方/份数/投料完全相同的另一个已关闭批次，仅
// 批次编号不同，用于验证另一批次的结果不能顶替。

// setupBatchAnchorLedger 建立并返回一份正常台账：
//   - B1（已关闭）：创建 R1/v1、5 份，开始前调整为 R1/v2、3 份，开始后
//     登记两条投料（fa、fb）再由 close-b1 关闭；
//   - B2（已关闭）：与 B1 最终配方、份数、投料内容完全相同，仅批次与请求
//     编号不同（close-b2），作为“另一批次结果不能顶替”的对照。
func setupBatchAnchorLedger(t *testing.T, dir string) []byte {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerReplayRecipes(t, s) // R1/v1：M1=0.125、M2=0.5；R1/v2：M1=250、M4=2
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraftBatch("u-final", "B1", "R1", "v2", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); err != nil {
		t.Fatal(err)
	}
	base := replayFeedTime()
	if _, err := s.AddFeeding("fa", "B1", "M1", "100.5", base, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("fb", "B1", "M4", "2", base.Add(time.Hour), "李四"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseBatch("close-b1", "B1"); err != nil {
		t.Fatal(err)
	}
	// B2：与 B1 最终配方、份数、投料内容完全相同，仅编号不同。
	if _, err := s.CreateBatch("b2", "B2", "R1", "v2", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b2", "B2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("fa-b2", "B2", "M1", "100.5", base, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("fb-b2", "B2", "M4", "2", base.Add(time.Hour), "李四"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseBatch("close-b2", "B2"); err != nil {
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

// anchorReqResult 取出正常台账中 reqNo 请求的保存结果原文。
func anchorReqResult(t *testing.T, good []byte, reqNo string) json.RawMessage {
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

// mutateAnchorView 读出 reqNo 的保存批次结果，按 edit 修改后重新序列化。
func mutateAnchorView(t *testing.T, good []byte, reqNo string, edit func(*BatchView)) json.RawMessage {
	t.Helper()
	var v BatchView
	if err := json.Unmarshal(anchorReqResult(t, good, reqNo), &v); err != nil {
		t.Fatal(err)
	}
	edit(&v)
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// rewriteAnchorResult 把正常台账中 reqNo 的保存结果替换为 raw，返回改写后内容。
func rewriteAnchorResult(t *testing.T, good []byte, reqNo string, raw json.RawMessage) []byte {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	req := st.Requests[reqNo]
	if req == nil {
		t.Fatalf("正常台账中应存在 %s 请求记录", reqNo)
	}
	req.Result = raw
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// 两类结果都必须以“开始时最终固定的版本与份数”为依据：关闭结果即使只改回
// 创建草稿时的旧版本/旧份数，或被换成内容完全相同的另一批次 B2 的关闭结果，
// 打开台账都必须按 ErrCorruptData 拒绝，错误保留操作类别、请求编号与批次编号。
func TestAnchorRejectsCloseResultUsingOldDraftPlanOrOtherBatch(t *testing.T) {
	cases := []struct {
		name    string
		reqNo   string
		mutate  func(good []byte) json.RawMessage
		wantOp  string
		wantReq string
	}{
		{
			name:    "关闭结果沿用创建草稿时的旧版本",
			reqNo:   "close-b1",
			wantOp:  "关闭请求",
			wantReq: "close-b1",
			mutate: func(good []byte) json.RawMessage {
				return mutateAnchorView(t, good, "close-b1", func(v *BatchView) {
					v.RecipeVersion = "v1" // 开始前已改为 v2，不能沿用旧版
				})
			},
		},
		{
			name:    "关闭结果沿用创建草稿时的旧份数",
			reqNo:   "close-b1",
			wantOp:  "关闭请求",
			wantReq: "close-b1",
			mutate: func(good []byte) json.RawMessage {
				return mutateAnchorView(t, good, "close-b1", func(v *BatchView) {
					v.PlannedPortions = 5 // 开始前已改为 3 份，不能沿用旧份数
				})
			},
		},
		{
			name:    "关闭结果被换成另一批次的同内容结果",
			reqNo:   "close-b1",
			wantOp:  "关闭请求",
			wantReq: "close-b1",
			mutate: func(good []byte) json.RawMessage {
				// B2 与 B1 配方、份数、投料完全相同，但那是另一批次的结果。
				return anchorReqResult(t, good, "close-b2")
			},
		},
		{
			name:    "开始结果沿用创建草稿时的旧版本",
			reqNo:   "start-b1",
			wantOp:  "开始执行请求",
			wantReq: "start-b1",
			mutate: func(good []byte) json.RawMessage {
				return mutateAnchorView(t, good, "start-b1", func(v *BatchView) {
					v.RecipeVersion = "v1"
				})
			},
		},
		{
			name:    "开始结果被换成另一批次的同内容结果",
			reqNo:   "start-b1",
			wantOp:  "开始执行请求",
			wantReq: "start-b1",
			mutate: func(good []byte) json.RawMessage {
				return anchorReqResult(t, good, "start-b2")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupBatchAnchorLedger(t, dir)
			bad := rewriteAnchorResult(t, good, tc.reqNo, tc.mutate(good))
			if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("应有关系不成立的保存结果返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{tc.wantOp, tc.wantReq, "B1"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含 %q（操作类别/请求编号/批次编号），得到 %v", want, err)
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

// 正常台账下，共有锚点核对通过：按原编号、原内容再次提交，开始请求始终返回
// 首次开始时的结果（R1/v2、3 份、执行中、空投料、零实投），关闭请求始终
// 返回关闭确认的完整结果（R1/v2、3 份、已关闭、两条投料及核对）；普通查询
// 显示批次当前现状。两者使用同一份归属/配方计划核对，却保持各自结果含义。
func TestAnchorReplayKeepsStartAndCloseResultMeanings(t *testing.T) {
	dir := t.TempDir()
	setupBatchAnchorLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer s.Close()

	// 开始结果：开始那一刻固定的 R1/v2、3 份，执行中、空投料、零实投。
	startView, err := s.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("重放开始请求应成功: %v", err)
	}
	if startView.RecipeNo != "R1" || startView.RecipeVersion != "v2" ||
		startView.RecipeName != "配方改版" || startView.PlannedPortions != 3 {
		t.Fatalf("开始结果应对应开始时固定的 R1/v2、3 份，得到 %+v", startView)
	}
	if startView.Status != StatusExecuting || len(startView.Feedings) != 0 {
		t.Fatalf("开始结果应是执行中、空投料，得到状态 %s、投料 %d 条",
			startView.Status, len(startView.Feedings))
	}
	startMats := materialsMap(startView)
	checkRequirement(t, startMats, "M1", "750", "0", "-750")
	checkRequirement(t, startMats, "M4", "6", "0", "-6")

	// 关闭结果：同一 R1/v2、3 份，但为已关闭并保留两条投料及实际核对。
	closeView, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("重放关闭请求应成功: %v", err)
	}
	if closeView.RecipeNo != "R1" || closeView.RecipeVersion != "v2" ||
		closeView.RecipeName != "配方改版" || closeView.PlannedPortions != 3 {
		t.Fatalf("关闭结果应对应开始时固定的 R1/v2、3 份，得到 %+v", closeView)
	}
	if closeView.Status != StatusClosed || len(closeView.Feedings) != 2 {
		t.Fatalf("关闭结果应是已关闭并保留 2 条投料，得到状态 %s、投料 %d 条",
			closeView.Status, len(closeView.Feedings))
	}
	closeMats := materialsMap(closeView)
	checkRequirement(t, closeMats, "M1", "750", "100.5", "-649.5")
	checkRequirement(t, closeMats, "M4", "6", "2", "-4")

	// 普通查询显示批次当前现状（已关闭），与首次开始结果不同但互不影响。
	current := mustGetBatch(t, s, "B1")
	if current.Status != StatusClosed || current.PlannedPortions != 3 ||
		current.RecipeVersion != "v2" || len(current.Feedings) != 2 {
		t.Fatalf("查询应显示已关闭的 R1/v2、3 份与 2 条投料，得到 %+v", current)
	}
}
