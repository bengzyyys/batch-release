package release

import (
	"encoding/json"
	"fmt"
)

// draftSnapshotRequest 概括读取台账时“草稿快照类”已保存成功请求（创建批次
// createBatch、草稿调整 updateDraftBatch）的核对上下文。两类结果都是当时
// 尚无投料的草稿视图。
//
// 四类批次操作共有的归属与完整性规则（原提交解析、保存结果完整、原批次
// 存在、结果批次归属）统一由内嵌的 batchRequestCheck 各步骤核对一份，这里
// 只按草稿快照自己的先后顺序组织这些步骤，并在其后补两类结果共有的草稿
// 业务判断：
//   - 结果必须仍是草稿——后来的开始执行与关闭现状不能混入；
//   - 投料列表必须为空——当时不存在任何投料；
//   - 结果采用的配方版本必须仍登记在案，名称取这个已登记版本（同编号
//     配方的其他版本即使名称、物料和数量相同也不能顶替），见
//     adoptedRecipe。
//
// 两类结果的区别不在这层处理：创建结果以原创建请求选定的配方版本与份数为
// 依据，并要求原提交内容本身符合创建要求（payloadRule，仅创建配置）；草稿
// 调整结果以当次实际采用的计划为依据（请求明确给出新版本或新份数时必须
// 符合指定值，配方留空、份数传零的沿用值只须满足完整性与数量规则，不拿
// 批次现在的计划反推）。配方与份数依据的选取由
// validateCreateBatchRequest、validateUpdateDraftRequest 在本结构给出的
// 共有判定之后各自核对。
//
// 各 label 字段保留两类操作各自的错误措辞：整理成一份逻辑后，错误仍带
// 操作类别、请求编号、能确定的批次编号及具体不符原因，不会退化成无法
// 定位的通用提示。
type draftSnapshotRequest struct {
	batchRequestCheck
	// snapshotDesc 描述这份快照对应的时刻（首次创建时 / 调整成功时），
	// 用于结果混入投料时的错误说明。
	snapshotDesc string
	// recipeBasisDesc 描述结果采用的配方版本的核对依据（原提交选定的配方 /
	// 结果采用的配方），用于版本未登记或名称不符时的错误说明。
	recipeBasisDesc string
	// payloadRule 非空时，在原提交内容可解析之后、保存结果完整性检查之前
	// 核对原提交内容是否符合该类操作的提交要求。只有创建批次请求需要：
	// 批次编号非空、份数为正整数。草稿调整请求的半成品提交（只给编号或只
	// 给版本号、份数为负）由 validateUpdateDraftRequest 在共有判定之后按
	// 自己的语义报告，保持原有先后关系。
	payloadRule func(reqNo, batchNo, payload string) error
}

// createBatchRequestCheck 是已保存创建批次请求的核对配置：依据表述为
// “原提交选定”，快照时刻为首次创建时。
var createBatchRequestCheck = draftSnapshotRequest{
	batchRequestCheck: batchRequestCheck{
		opLabel:      "创建批次请求",
		resultLabel:  "创建结果",
		successLabel: "创建成功",
	},
	snapshotDesc:    "首次创建时",
	recipeBasisDesc: "原提交选定的配方",
	payloadRule:     validateCreateBatchPayloadRule,
}

// updateDraftRequestCheck 是已保存草稿调整请求的核对配置：依据表述为
// “结果采用”，快照时刻为调整成功时。
var updateDraftRequestCheck = draftSnapshotRequest{
	batchRequestCheck: batchRequestCheck{
		opLabel:      "草稿调整请求",
		resultLabel:  "调整结果",
		successLabel: "调整成功",
	},
	snapshotDesc:    "调整成功时",
	recipeBasisDesc: "结果采用的配方",
}

// validateCreateBatchPayloadRule 核对创建批次请求的原提交内容本身符合创建
// 要求：批次编号非空、份数为正整数。被改成非法内容的提交不可能对应一次
// 成功的创建。运行到这里时原提交内容已能解析（batchRequestCheck 的共同
// 步骤先确认），因此再解析失败只作防御性处理。
func validateCreateBatchPayloadRule(reqNo, batchNo, payload string) error {
	var p createBatchPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return fmt.Errorf("%w: 创建批次请求 %q 保存的提交内容无法解析",
			ErrCorruptData, reqNo)
	}
	if p.BatchNo == "" || p.Portions <= 0 {
		return fmt.Errorf("%w: 创建批次请求 %q（批次 %q）保存的提交内容不符合创建要求（批次编号为空或计划份数 %d 不是正整数）",
			ErrCorruptData, reqNo, batchNo, p.Portions)
	}
	return nil
}

// resolve 核对创建批次/草稿调整两类已保存成功请求。四类操作共有的步骤都
// 委托给内嵌 batchRequestCheck 的同名方法（只维护一份），这里按草稿快照
// 原有的先后顺序组织，并在归属确认后补草稿状态与空投料两项业务判断。
// 成功时返回原请求所指的批次编号与解析后的保存结果视图。
//
// 检查顺序与整理前一致：原提交内容可解析 →（创建请求额外核对提交要求）→
// 保存结果完整 → 原批次存在 → 结果批次编号归属 → 结果仍为草稿 → 投料
// 为空。
//
// 至此共有与草稿快照规则结束：结果采用的配方版本是否仍登记、名称是否
// 一致、份数依据与逐物料数量核对，都不在这里判断，由调用方按各自依据
// 继续核对。
func (cfg draftSnapshotRequest) resolve(st *persistedState, reqNo string, req *requestRecord) (string, *BatchView, error) {
	batchNo, err := cfg.parseRequestBatch(reqNo, req)
	if err != nil {
		return "", nil, err
	}
	// 创建请求在解析提交内容之后、检查保存结果之前，先要求提交内容符合
	// 创建要求；草稿调整请求没有这一步。
	if cfg.payloadRule != nil {
		if err := cfg.payloadRule(reqNo, batchNo, req.Payload); err != nil {
			return "", nil, err
		}
	}
	result, err := cfg.requireBatchResult(reqNo, batchNo, req)
	if err != nil {
		return "", nil, err
	}
	if _, err := cfg.requireExistingBatch(st, reqNo, batchNo); err != nil {
		return "", nil, err
	}
	if err := cfg.requireResultOwnedByBatch(reqNo, batchNo, result); err != nil {
		return "", nil, err
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
	return batchNo, result, nil
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
