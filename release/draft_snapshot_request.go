package release

import (
	"encoding/json"
	"fmt"
)

// draftSnapshotRequest 概括读取台账时“草稿快照类”已保存成功请求（创建批次
// createBatch、草稿调整 updateDraftBatch）的核对上下文。两类结果都是当时
// 尚无投料的草稿视图，共有规则都在此核对一份：
//   - 原提交内容必须能解析出批次编号，保存结果必须完整、能读成批次结果；
//   - 原请求对应的批次必须仍然存在——批次后来再次调整、开始执行、投料或
//     关闭都属于批次现状，不影响这份历史结果；
//   - 保存结果的批次编号必须就是原请求所指的批次，内容相同的另一批次的
//     结果不能顶替；
//   - 结果必须仍是草稿、投料列表为空——后来的投料与关闭现状不能混入；
//   - 结果采用的配方版本必须仍登记在案，名称取这个已登记版本——同编号
//     配方的其他版本即使名称、物料和数量相同也不能顶替。
//
// 两类结果的区别不在这层处理：创建结果以原创建请求选定的配方版本与份数为
// 依据；草稿调整结果以当次实际采用的计划为依据（请求明确给出新版本或新
// 份数时必须符合指定值，配方留空、份数传零的沿用值只须满足完整性与数量
// 规则，不拿批次现在的计划反推）。配方与份数依据的选取由
// validateCreateBatchRequest、validateUpdateDraftRequest 在本结构给出的
// 共有判定之后各自核对。
//
// 各 label 字段保留两类操作各自的错误措辞：整理成一份逻辑后，错误仍带
// 操作类别、请求编号、能确定的批次编号及具体不符原因，不会退化成无法
// 定位的通用提示。
type draftSnapshotRequest struct {
	// opLabel 是错误信息中的操作类别（创建批次请求 / 草稿调整请求）。
	opLabel string
	// resultLabel 是“保存的xx结果”在错误信息中的叫法（创建结果 / 调整结果）。
	resultLabel string
	// successLabel 说明这是哪一类成功（创建成功 / 调整成功），用于原批次
	// 不存在或版本未登记时的错误说明。
	successLabel string
	// snapshotDesc 描述这份快照对应的时刻（首次创建时 / 调整成功时），
	// 用于结果混入投料时的错误说明。
	snapshotDesc string
	// recipeBasisDesc 描述结果采用的配方版本的核对依据（原提交选定的配方 /
	// 结果采用的配方），用于版本未登记或名称不符时的错误说明。
	recipeBasisDesc string
}

// createBatchRequestCheck 是已保存创建批次请求的共有核对配置：依据表述为
// “原提交选定”，快照时刻为首次创建时。
var createBatchRequestCheck = draftSnapshotRequest{
	opLabel:         "创建批次请求",
	resultLabel:     "创建结果",
	successLabel:    "创建成功",
	snapshotDesc:    "首次创建时",
	recipeBasisDesc: "原提交选定的配方",
}

// updateDraftRequestCheck 是已保存草稿调整请求的共有核对配置：依据表述为
// “结果采用”，快照时刻为调整成功时。
var updateDraftRequestCheck = draftSnapshotRequest{
	opLabel:         "草稿调整请求",
	resultLabel:     "调整结果",
	successLabel:    "调整成功",
	snapshotDesc:    "调整成功时",
	recipeBasisDesc: "结果采用的配方",
}

// resolve 核对创建批次/草稿调整两类已保存成功请求的共有规则，成功时返回
// 原请求所指的批次编号与解析后的保存结果视图。核对内容：
//   - 原提交内容必须能解析（解析失败时无法确定关联批次，错误只指明请求
//     编号与操作类别）；
//   - 保存结果缺失、为 null、为空对象或无法解析为完整批次结果时拒绝；
//   - 原请求对应的批次必须存在：批次后来的再次调整、开始执行、投料与
//     关闭都属于现状，不影响这份历史结果的核对；
//   - 保存结果的批次编号必须就是原请求所指的批次；
//   - 结果必须仍是草稿、投料列表为空。
//
// 至此共有规则结束：结果采用的配方版本是否仍登记、名称是否一致、份数
// 依据与逐物料数量核对，都不在这里判断，由调用方按各自依据继续核对。
func (cfg draftSnapshotRequest) resolve(st *persistedState, reqNo string, req *requestRecord) (string, *BatchView, error) {
	// createBatchPayload 与 updateDraftPayload 都以批次编号定位批次，用
	// 匿名结构解析即可，不依赖具体载荷类型。
	var payload struct {
		BatchNo string
	}
	if err := json.Unmarshal([]byte(req.Payload), &payload); err != nil {
		// 提交内容本身已无法解析时无法确定关联批次，错误信息只指明请求编号
		// 与操作类别。
		return "", nil, fmt.Errorf("%w: %s %q 保存的提交内容无法解析",
			ErrCorruptData, cfg.opLabel, reqNo)
	}
	batchNo := payload.BatchNo
	if len(req.Result) == 0 {
		return "", nil, fmt.Errorf("%w: %s %q（批次 %q）缺少保存的%s",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, cfg.resultLabel)
	}
	var result BatchView
	if err := json.Unmarshal(req.Result, &result); err != nil {
		return "", nil, fmt.Errorf("%w: %s %q（批次 %q）保存的%s无法解析为完整批次结果",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, cfg.resultLabel)
	}
	// null、空对象或读不出批次编号的结果都不能当作成功结果。
	if result.BatchNo == "" {
		return "", nil, fmt.Errorf("%w: %s %q（批次 %q）保存的%s缺失或不完整",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, cfg.resultLabel)
	}
	// 原请求对应的批次必须仍然存在：批次没有了，保存的结果不能单独作为
	// 成功的依据。批次后来再次调整、开始执行、投料与关闭都属于现状，不
	// 影响这份历史结果的核对。
	if findBatch(st, batchNo) == nil {
		return "", nil, fmt.Errorf("%w: %s %q 对应的批次 %q 不存在，保存的%s不能单独作为%s的依据",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, cfg.resultLabel, cfg.successLabel)
	}
	// 保存结果必须指向原请求的批次：内容相同的另一批次的结果不能顶替。
	if result.BatchNo != batchNo {
		return "", nil, fmt.Errorf("%w: %s %q（批次 %q）保存结果的批次编号为 %q，不能用另一批次的结果顶替",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, result.BatchNo)
	}
	// 结果必须仍是草稿：被改成执行中或已关闭的现状，即与这份历史记录不
	// 一致。
	if result.Status != StatusDraft {
		return "", nil, fmt.Errorf("%w: %s %q（批次 %q）保存结果的状态为 %q，不是草稿",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, result.Status)
	}
	// 当时不存在任何投料：保存结果里出现投料，说明混入了批次后来的现状，
	// 与这份历史记录不一致。
	if len(result.Feedings) != 0 {
		return "", nil, fmt.Errorf("%w: %s %q（批次 %q）保存结果包含 %d 条投料，%s的投料列表应为空，不能混入后来追加的投料",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, len(result.Feedings), cfg.snapshotDesc)
	}
	return batchNo, &result, nil
}

// adoptedRecipe 核对保存结果采用的配方版本仍登记在案、名称与该已登记版本
// 一致，并返回该版本。两类结果共用这一核对：版本没有了，结果里的名称与
// 物料用量都失去依据；同编号配方的其他版本即使名称、物料和数量相同，也
// 不能顶替。创建结果在调用前已确认结果采用的就是原提交选定的版本，因此
// 这里的“结果采用的版本”对创建结果即原选版本。
func (cfg draftSnapshotRequest) adoptedRecipe(st *persistedState, reqNo, batchNo string, result *BatchView) (*recipeRecord, error) {
	r := findRecipe(st, result.RecipeNo, result.RecipeVersion)
	if r == nil {
		return nil, fmt.Errorf("%w: %s %q（批次 %q）%s %q 版本 %q 未登记，保存的%s不能单独作为%s的依据",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, cfg.recipeBasisDesc,
			result.RecipeNo, result.RecipeVersion, cfg.resultLabel, cfg.successLabel)
	}
	// 名称必须与结果实际采用版本的登记名称一致。
	if result.RecipeName != r.Name {
		return nil, fmt.Errorf("%w: %s %q（批次 %q）保存结果的配方名称 %q 与%s %q 版本 %q 的名称 %q 不一致",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, result.RecipeName,
			cfg.recipeBasisDesc, result.RecipeNo, result.RecipeVersion, r.Name)
	}
	return r, nil
}
