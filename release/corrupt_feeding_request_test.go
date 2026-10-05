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

// 本文件锁定“已保存的成功投料请求，其返回结果必须与台账中的实际投料一致”：
// 保存的请求结果即使变成缺失、null、空对象，或被替换成另一条投料，也不能
// 单独作为登记成功的依据。打开台账时发现这类问题返回 ErrCorruptData、不
// 返回可用对象；已打开的台账在后续任意查询或写入重载时读到同样问题也返回
// 该错误，即使本次访问的是另一条正常记录。拒绝时原文件保持不变：不删除
// 请求、不补造投料、不重新执行原登记。
//
// 正常情形不受影响：结果克数显示为 1 而原内容写 1.000、时间只是时区写法
// 不同，都按一致处理；后来追加其他投料或关闭批次，原成功请求仍重放首次
// 结果且不追加投料。

// corruptRequestTime 是投料请求损坏用例使用的固定时间基。
func corruptRequestTime() time.Time {
	return time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
}

// buildFeedingRequestFixture 用真实接口构造一份含成功投料请求的台账：
// R1/v1（M1=100、M2=0.5、M3=0.010），执行中的 B1（10 份）有两条投料
// （feed-1：B1/M1/1.000/t1/张三 → 序号 1；feed-2：B1/M2/2/t2/李四 →
// 序号 2），执行中的 B2（5 份）有一条投料（b2-feed → B2 内序号 1）。
// 返回解析后的状态，供用例在其副本上改坏请求结果后自行写回。
func buildFeedingRequestFixture(t *testing.T, dir string) *persistedState {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatal(err)
	}
	t1 := corruptRequestTime()
	t2 := t1.Add(time.Hour)
	if f, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三"); err != nil || f.Seq != 1 {
		t.Fatalf("B1 首笔投料应成功且序号为 1，得到 %+v/%v", f, err)
	}
	if _, err := s.AddFeeding("feed-2", "B1", "M2", "2", t2, "李四"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("b2-feed", "B2", "M1", "4", t2, "李四"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var st persistedState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return &st
}

// mutateFeedingPayload 解析指定投料请求的载荷，交给 edit 修改后重新序列化。
func mutateFeedingPayload(t *testing.T, st *persistedState, reqNo string, edit func(*addFeedingPayload)) {
	t.Helper()
	var p addFeedingPayload
	if err := json.Unmarshal([]byte(st.Requests[reqNo].Payload), &p); err != nil {
		t.Fatalf("解析请求载荷失败: %v", err)
	}
	edit(&p)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	st.Requests[reqNo].Payload = string(raw)
}

// mutateFeedingResult 解析指定投料请求保存的结果，交给 edit 修改后重新序列化。
func mutateFeedingResult(t *testing.T, st *persistedState, reqNo string, edit func(*FeedingView)) {
	t.Helper()
	var v FeedingView
	if err := json.Unmarshal(st.Requests[reqNo].Result, &v); err != nil {
		t.Fatalf("解析请求结果失败: %v", err)
	}
	edit(&v)
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	st.Requests[reqNo].Result = raw
}

// writeStateDroppingRequestResult 写回台账，但删掉指定请求记录里的 result
// 字段（连 null 都没有），模拟字段缺失。
func writeStateDroppingRequestResult(t *testing.T, dir string, st *persistedState, reqNo string) []byte {
	t.Helper()
	full, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(full, &doc); err != nil {
		t.Fatal(err)
	}
	var reqs map[string]map[string]json.RawMessage
	if err := json.Unmarshal(doc["requests"], &reqs); err != nil {
		t.Fatal(err)
	}
	delete(reqs[reqNo], "result")
	reqsRaw, err := json.Marshal(reqs)
	if err != nil {
		t.Fatal(err)
	}
	doc["requests"] = reqsRaw
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), out, 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

// Open 读取台账时，任一已保存的成功投料请求无法对应到原批次中一条内容
// （结果、实际投料、原提交内容三者）完全一致的投料，都必须返回
// ErrCorruptData：不返回可用的 Store，错误信息指出问题请求编号及其关联
// 批次（适用时），原文件保持不变。
func TestOpenRejectsInconsistentFeedingRequestResult(t *testing.T) {
	t1 := corruptRequestTime()
	otherTime := t1.Add(24 * time.Hour)

	cases := []struct {
		name  string
		apply func(t *testing.T, dir string, st *persistedState) []byte
		want  []string // 错误信息中必须出现的片段
	}{
		{
			name: "结果字段缺失",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				return writeStateDroppingRequestResult(t, dir, st, "feed-1")
			},
			want: []string{"feed-1", "结果缺失"},
		},
		{
			name: "结果为 null",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				st.Requests["feed-1"].Result = json.RawMessage("null")
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "null"},
		},
		{
			name: "结果为空对象",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				st.Requests["feed-1"].Result = json.RawMessage("{}")
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "空对象"},
		},
		{
			name: "结果不是投料对象",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				// 台账文件本身仍是合法 JSON，但该请求结果是数组，无法解析为
				// 一条投料记录。
				st.Requests["feed-1"].Result = json.RawMessage("[]")
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "无法解析"},
		},
		{
			name: "载荷无法解析",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				st.Requests["feed-1"].Payload = "{坏载荷"
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "原提交内容无法解析"},
		},
		{
			name: "结果被替换成另一条投料",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				mutateFeedingResult(t, st, "feed-1", func(v *FeedingView) {
					// 换成同一批次中真实存在的序号 2：物料、克数、时间、
					// 登记人全部是另一条投料的内容。
					*v = FeedingView{Seq: 2, MaterialNo: "M2", Grams: "2",
						Time: t1.Add(time.Hour), Registrar: "李四"}
				})
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "B1", "M2"},
		},
		{
			name: "结果序号超出该批次现有条数",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				mutateFeedingResult(t, st, "feed-1", func(v *FeedingView) { v.Seq = 99 })
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "B1", "99", "不存在"},
		},
		{
			name: "结果序号为零但其余字段非空",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				mutateFeedingResult(t, st, "feed-1", func(v *FeedingView) { v.Seq = 0 })
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "B1", "0", "不存在"},
		},
		{
			name: "原批次不存在，其他批次有同序号投料",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				// 删除 B1 及其指向 B1 的另一条投料请求；B2 仍有序号 1 的投料，
				// 但不能顶替 feed-1 的对应记录。
				kept := st.Batches[:0]
				for _, b := range st.Batches {
					if b.BatchNo != "B1" {
						kept = append(kept, b)
					}
				}
				st.Batches = kept
				delete(st.Requests, "feed-2")
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "B1", "已不存在"},
		},
		{
			name: "原批次投料被清空，其他批次同序号同物料也不能顶替",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				for _, b := range st.Batches {
					if b.BatchNo == "B1" {
						b.Feedings = nil // B2 仍有序号 1 的 M1 投料
					}
				}
				delete(st.Requests, "feed-2")
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "B1", "1", "不存在"},
		},
		{
			name: "保存结果的物料与实际记录不符",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				mutateFeedingResult(t, st, "feed-1", func(v *FeedingView) { v.MaterialNo = "M2" })
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "B1", "物料"},
		},
		{
			name: "保存结果的克数与实际记录不符",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				mutateFeedingResult(t, st, "feed-1", func(v *FeedingView) { v.Grams = "5" })
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "B1", "克数"},
		},
		{
			name: "保存结果的时间与实际记录不符",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				mutateFeedingResult(t, st, "feed-1", func(v *FeedingView) { v.Time = otherTime })
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "B1", "投料时间"},
		},
		{
			name: "保存结果的登记人与实际记录不符",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				mutateFeedingResult(t, st, "feed-1", func(v *FeedingView) { v.Registrar = "王五" })
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "B1", "登记人"},
		},
		{
			name: "物料数量相同但时间不同，不能仅凭物料数量放行",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				mutateFeedingResult(t, st, "feed-1", func(v *FeedingView) {
					v.Time = otherTime
					v.Registrar = "李四"
				})
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "B1", "投料时间"},
		},
		{
			name: "原提交内容的物料与实际投料不符",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				mutateFeedingPayload(t, st, "feed-1", func(p *addFeedingPayload) { p.MaterialNo = "M2" })
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "B1", "原提交内容"},
		},
		{
			name: "原提交内容的克数与实际投料不符",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				mutateFeedingPayload(t, st, "feed-1", func(p *addFeedingPayload) { p.Grams = "9" })
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "B1", "原提交内容"},
		},
		{
			name: "原提交内容的时间与实际投料不符",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				mutateFeedingPayload(t, st, "feed-1", func(p *addFeedingPayload) { p.Time = otherTime })
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "B1", "原提交时间"},
		},
		{
			name: "原提交内容的登记人与实际投料不符",
			apply: func(t *testing.T, dir string, st *persistedState) []byte {
				mutateFeedingPayload(t, st, "feed-1", func(p *addFeedingPayload) { p.Registrar = "李四" })
				return writeStateFile(t, dir, st)
			},
			want: []string{"feed-1", "B1", "原提交登记人"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st := buildFeedingRequestFixture(t, dir)
			badBytes := tc.apply(t, dir, st)

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("损坏的投料请求结果应使 Open 返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可继续使用的 Store 对象")
			}
			msg := err.Error()
			for _, frag := range tc.want {
				if !strings.Contains(msg, frag) {
					t.Fatalf("错误信息应包含 %q，得到 %v", frag, err)
				}
			}
			// 拒绝打开必须保留原文件：不删除请求、不补造投料、不重新登记。
			got, readErr := os.ReadFile(filepath.Join(dir, stateFileName))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(got, badBytes) {
				t.Fatalf("拒绝打开不应改动台账文件")
			}
		})
	}
}

// 核对数量时区分输入写法与实际克数：原请求填写 1.000、成功结果显示 1 是
// 正常情况；时间表示同一刻时，时区写法不同也不判损坏。这些台账必须能正常
// 打开，且原编号、原内容重放仍返回首次结果，不追加投料。
func TestOpenAcceptsEquivalentGramsFormatAndTimezone(t *testing.T) {
	t1 := corruptRequestTime()

	t.Run("保存结果克数写作 1.000 仍与实际 1 克一致", func(t *testing.T) {
		dir := t.TempDir()
		st := buildFeedingRequestFixture(t, dir)
		mutateFeedingResult(t, st, "feed-1", func(v *FeedingView) { v.Grams = "1.000" })
		writeStateFile(t, dir, st)

		s, err := Open(dir)
		if err != nil {
			t.Fatalf("克数写法不同但数值一致不应判损坏: %v", err)
		}
		defer s.Close()
		replay, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
		if err != nil {
			t.Fatalf("原内容重放应成功: %v", err)
		}
		checkFeedingView(t, replay, 1, "M1", "1", t1, "张三")
		view := mustGetBatch(t, s, "B1")
		if len(view.Feedings) != 2 {
			t.Fatalf("重放不应追加投料，得到 %d 条", len(view.Feedings))
		}
	})

	t.Run("保存结果时间仅时区写法不同、指向同一时刻", func(t *testing.T) {
		dir := t.TempDir()
		st := buildFeedingRequestFixture(t, dir)
		// t1 是 08:00 UTC，同一时刻写作东八区 16:00+08:00。
		east8 := time.FixedZone("UTC+8", 8*60*60)
		sameInstant := t1.In(east8)
		mutateFeedingResult(t, st, "feed-1", func(v *FeedingView) { v.Time = sameInstant })
		writeStateFile(t, dir, st)

		s, err := Open(dir)
		if err != nil {
			t.Fatalf("时间仅时区写法不同不应判损坏: %v", err)
		}
		defer s.Close()
		replay, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
		if err != nil {
			t.Fatalf("原内容重放应成功: %v", err)
		}
		checkFeedingView(t, replay, 1, "M1", "1", t1, "张三")
	})

	t.Run("时间数值上是另一时刻即使偏移后看起来相同也判不一致", func(t *testing.T) {
		dir := t.TempDir()
		st := buildFeedingRequestFixture(t, dir)
		// 09:00 UTC 与 08:00 UTC 不是同一时刻，即便各自带不同偏移。
		mutateFeedingResult(t, st, "feed-1", func(v *FeedingView) {
			v.Time = time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
		})
		writeStateFile(t, dir, st)
		s, err := Open(dir)
		if !errors.Is(err, ErrCorruptData) {
			if s != nil {
				s.Close()
			}
			t.Fatalf("不是同一时刻的时间应判损坏，得到 %v", err)
		}
		if s != nil {
			s.Close()
		}
	})
}

// 正常台账中，用同一编号把原来的 1.000 改成数值相等的 1 再提交，仍返回
// ErrRequestConflict——输入写法差异不影响请求内容匹配，也不触发损坏校验。
func TestEquivalentGramsStringStillConflictsOnContent(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	t1 := corruptRequestTime()
	first, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatal(err)
	}
	checkFeedingView(t, first, 1, "M1", "1", t1, "张三")

	if _, err := s.AddFeeding("feed-1", "B1", "M1", "1", t1, "张三"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("1.000 改成 1 仍属于不同内容，应返回 ErrRequestConflict，得到 %v", err)
	}
	view := mustGetBatch(t, s, "B1")
	if len(view.Feedings) != 1 {
		t.Fatalf("冲突提交不应追加投料，得到 %d 条", len(view.Feedings))
	}
}

// 后来追加其他投料或关闭批次，都不能使原成功请求失效：重新打开台账后，
// 原编号、原内容仍返回第一次的投料，批次查询保持追加后的真实记录。
func TestFeedingRequestStaysValidAfterMoreFeedingsAndClose(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	t1 := corruptRequestTime()
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("feed-2", "B1", "M2", "2", t1.Add(time.Hour), "李四"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseBatch("c1", "B1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("追加投料并关闭批次后应能正常重新打开: %v", err)
	}
	defer s2.Close()
	// 批次已关闭，原成功请求仍重放首次结果而不是按状态拒绝或重复投料。
	replay, err := s2.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("关闭批次后原成功请求仍应可重放: %v", err)
	}
	checkFeedingView(t, replay, 1, "M1", "1", t1, "张三")
	view := mustGetBatch(t, s2, "B1")
	if view.Status != StatusClosed || len(view.Feedings) != 2 {
		t.Fatalf("批次应保持已关闭且仍为 2 条投料: %+v", view)
	}
}

// 台账正常打开后，保存内容中的投料请求结果被改坏：下一次查询或写入在重载
// 时必须返回 ErrCorruptData，即使访问的是另一条正常批次或配方；原请求自身
// 的重放也不能返回被改坏的结果。被拒绝的访问不改文件、不占用请求编号；
// 恢复文件后原请求行为完整可用，被拒绝过的编号可以正常提交新业务。
func TestFeedingRequestCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
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
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatal(err)
	}
	t1 := corruptRequestTime()
	t2 := t1.Add(time.Hour)
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("b2-feed", "B2", "M1", "4", t2, "李四"); err != nil {
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
	// 把 feed-1 的保存结果替换成另一条投料的内容（序号 2 在 B1 中不存在）。
	broken.Requests["feed-1"].Result = mustMarshalFeedingView(t, FeedingView{
		Seq: 2, MaterialNo: "M2", Grams: "2", Time: t2, Registrar: "李四",
	})
	badBytes := writeStateFile(t, dir, &broken)

	// 即使本次访问的是另一条正常记录，也不能忽略损坏的投料请求。
	for _, batchNo := range []string{"B1", "B2"} {
		if v, err := s.GetBatch(batchNo); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("改坏后查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		} else if v != nil {
			t.Fatalf("被拒绝的查询不应返回部分视图: %+v", v)
		}
	}
	if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("查询配方也应重载并返回 ErrCorruptData，得到 %v", err)
	}
	// 原请求自身的重放不能把坏结果当成功结果返回。
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏请求的重放应返回 ErrCorruptData，得到 %v", err)
	}
	// 对正常批次的新写入同样被拒绝。
	if _, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", t2, "王五"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上对正常批次投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.CloseBatch("close-rejected", "B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上关闭正常批次应返回 ErrCorruptData，得到 %v", err)
	}

	// 拒绝时保留原文件，被拒绝的写入不留下请求记录。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, badBytes) {
		t.Fatalf("被拒绝的访问不应改动台账文件")
	}
	for _, marker := range []string{"feed-rejected", "close-rejected"} {
		if bytes.Contains(after, []byte(marker)) {
			t.Fatalf("被拒绝的写入不应留下请求记录 %q", marker)
		}
	}

	// 恢复文件后：原请求完整重放首次结果，被拒绝过的编号可正常使用，
	// 系统没有补造投料、也没有重新执行原登记。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	replay, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("恢复后原请求重放应成功: %v", err)
	}
	checkFeedingView(t, replay, 1, "M1", "1", t1, "张三")
	f, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", t2, "王五")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法提交: %v", err)
	}
	if f.Seq != 2 || f.MaterialNo != "M1" || f.Grams != "1" || f.Registrar != "王五" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
	b1 := mustGetBatch(t, s, "B1")
	if len(b1.Feedings) != 1 {
		t.Fatalf("恢复后 B1 应仍只有原始的 1 条投料，得到 %d 条", len(b1.Feedings))
	}
}

func mustMarshalFeedingView(t *testing.T, v FeedingView) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
